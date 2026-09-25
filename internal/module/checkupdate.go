package module

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	url2 "net/url"
	"strings"
	"sync"
	"time"

	ref "github.com/distribution/reference"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// ImageCheckList 检查更新处理后的镜像列表
type ImageCheckList struct {
	NeedUpdate bool
}

// ImageUpdateData 镜像更新检测结果的内存缓存。
// 定时任务在写、HTTP 请求在读，必须加锁 —— 原实现直接并发读写裸 map，存在数据竞争。
type ImageUpdateData struct {
	mu        sync.RWMutex
	data      map[string]ImageCheckList
	lastCheck time.Time

	// 以下字段服务于「检测状态」接口：前端同步请求超时后，靠它判断这轮检测是否已经结束，
	// 从而拿到真实结论，而不是把「客户端放弃等待」当成「检测失败」。
	running        bool
	startedAt      time.Time
	lastTotal      int
	lastNeedUpdate int

	// runDone 是「当前这一轮检测的结束信号」：非 nil 表示有检测在跑，close 表示这一轮结束。
	// 作用见 beginRun —— 让后到的调用方精确地等到前一轮结束，而不是空转轮询。
	runDone chan struct{}
}

// CheckUpdateStatus 一次检测的状态快照。
type CheckUpdateStatus struct {
	// Running 当前是否有检测正在执行。
	Running bool
	// StartedAt 最近一次检测的开始时间；零值表示从未检测过。
	// 一轮结束后不会清零，保留下来便于观察上一轮是何时启动的。
	StartedAt time.Time
	// LastCheckedAt 上一轮完成时间；零值表示从未检测过。
	LastCheckedAt time.Time
	// LastTotal 上一轮参与检测的镜像总数。
	LastTotal int
	// LastNeedUpdate 上一轮判定有更新的镜像数量。
	LastNeedUpdate int
}

const ContentDigestHeader = "Docker-Content-Digest"

// ImageCheckFreshness 检测结果被视为"新鲜"的时长。
// 定时链路里检测与更新是同一趟任务，不存在读到陈旧结论的问题；
// 这个上限是给"用户随时点立即执行"准备的 —— 距上次检测超过 30 分钟就先重测再动容器。
const ImageCheckFreshness = 30 * time.Minute

// CheckRunWaitTimeout 后到的检测调用最多等前一轮跑完的时长。
//
// 一轮检测正常是秒级（registry 决策已按域名缓存），只有在网络整体异常时才会拖到分钟级，
// 所以三分钟是个宽松的上限：等不到就放弃，绝不把调用方无限期挂住。
const CheckRunWaitTimeout = 3 * time.Minute

func NewImageCheck() *ImageUpdateData {
	return &ImageUpdateData{
		data: map[string]ImageCheckList{},
	}
}

// CheckUpdate 重新检测全部镜像的更新状态。
// 每次都用新 map 整体替换：既保证读到的是一致快照，
// 也顺带清掉已删除镜像的残留条目（原实现只增不减，会一直涨）。
//
// 同一时刻只允许一轮检测在跑，撞上时**合流**而不是再排一轮：
//   - 定时任务（:30）、手动「检测更新」按钮、自动更新前的刷新，三条路径都可能撞在一起，
//     各跑一轮纯属重复烧网络，还可能让两份结论互相覆盖；
//   - 后到的调用方改为等前一轮结束，然后**直接复用它的结论**（它刚写完，必然新鲜），
//     不再自己重复检测一遍。
//
// 返回值：true 表示返回时缓存是新鲜的（自己跑的，或复用了刚跑完的一轮）；
// false 表示等到 CheckRunWaitTimeout 仍未等到，缓存可能不够新，调用方应按需降级处理。
func (i *ImageUpdateData) CheckUpdate(imageList []types.Image) bool {
	if !i.tryRun() {
		if !i.waitRun(CheckRunWaitTimeout) {
			logx.Error("等待既有镜像更新检测超时，本轮放弃")
			return false
		}
		logx.Info("已有检测在进行中，直接复用刚完成的一轮结论")
		return true
	}
	// 无论中途怎么退出都要收尾，否则状态接口会一直显示「检测中」、前端跟着一直轮询。
	defer i.finishRun()

	next := make(map[string]ImageCheckList, len(imageList))
	needUpdate := 0
	for _, image := range imageList {
		if strings.Contains(image.ImageName, "0nlylty/dockercopilot") {
			continue
		}
		result := i.checkSingleImage(image)
		if result.NeedUpdate {
			needUpdate++
		}
		next[image.ID] = result
	}

	i.mu.Lock()
	i.data = next
	i.lastTotal = len(imageList)
	i.lastNeedUpdate = needUpdate
	i.mu.Unlock()
	return true
}

// tryRun 尝试立即取得运行权，取不到就直接返回 false（不等待）。
func (i *ImageUpdateData) tryRun() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runDone != nil {
		return false
	}
	i.runDone = make(chan struct{})
	i.running = true
	i.startedAt = time.Now()
	return true
}

// waitRun 等到当前这一轮检测结束；超时返回 false。
//
// 等完一轮后要重新看一眼：万一期间又有一轮开始了，就接着等那一轮 ——
// 无论如何都要保证返回时"至少有一轮刚刚结束"，缓存是新鲜的。
func (i *ImageUpdateData) waitRun(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		i.mu.RLock()
		done := i.runDone
		i.mu.RUnlock()
		if done == nil {
			return true
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
}

// finishRun 收尾并刷新 lastCheck。
//
// 顺序很重要：它必须在写入 data / lastTotal / lastNeedUpdate 之后执行。
// 轮询方是靠「lastCheckedAt 变了」判定这一轮结束的，等它看到新的 lastCheckedAt 时，
// 上一轮的统计值必然已经写好（互斥锁提供 happens-before）。
func (i *ImageUpdateData) finishRun() {
	i.mu.Lock()
	i.running = false
	i.lastCheck = time.Now()
	done := i.runDone
	i.runDone = nil
	i.mu.Unlock()

	// 关在解锁之后：close 会立刻唤醒所有等待者，它们马上又要抢锁，
	// 持锁期间唤醒等于凭空多一次争抢。
	if done != nil {
		close(done)
	}
}

// Status 返回检测状态快照，供状态接口轮询。纯内存读取，无网络开销。
func (i *ImageUpdateData) Status() CheckUpdateStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return CheckUpdateStatus{
		Running:        i.running,
		StartedAt:      i.startedAt,
		LastCheckedAt:  i.lastCheck,
		LastTotal:      i.lastTotal,
		LastNeedUpdate: i.lastNeedUpdate,
	}
}

// NeedUpdate 查询某个镜像 ID 当前是否检测到有更新。
func (i *ImageUpdateData) NeedUpdate(imageID string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	item, ok := i.data[imageID]
	return ok && item.NeedUpdate
}

// LastCheckAt 返回上一次检测完成的时间，零值表示尚未检测过。
func (i *ImageUpdateData) LastCheckAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lastCheck
}

// IsStale 判断检测结果是否已经过期，过期时自动更新应当先重新检测。
func (i *ImageUpdateData) IsStale(maxAge time.Duration) bool {
	last := i.LastCheckAt()
	return last.IsZero() || time.Since(last) > maxAge
}

// checkSingleImage 检测单个镜像，返回检测结论。
func (i *ImageUpdateData) checkSingleImage(image types.Image) ImageCheckList {
	// 本地没有 repoDigest 就无法比对（例如本地 build 出来的镜像）。
	// 提前返回还能省掉后续两次网络请求。
	if len(image.RepoDigests) == 0 {
		logx.Error("未在本地获取到repoDigest" + image.ImageName + ":" + image.ImageTag)
		return ImageCheckList{}
	}

	token, err := GetToken(image, "")
	if err != nil {
		logx.Error("获取token失败或者无需获取token，继续尝试检查" + err.Error())
	}
	digestURL, err := BuildManifestURL(image)
	if err != nil {
		logx.Error("获取digestURL失败" + err.Error())
		return ImageCheckList{}
	}
	remoteDigest, err := GetDigest(digestURL, token)
	if err != nil {
		logx.Error("获取digest失败" + err.Error())
		return ImageCheckList{}
	}
	if remoteDigest == "" {
		logx.Error("远端digest为空" + image.ImageName + ":" + image.ImageTag)
		return ImageCheckList{}
	}

	return compareDigest(image, remoteDigest)
}

// compareDigest 把远端 digest 与本地 repoDigest 比对。
//
// 原实现的问题：在循环里反复覆盖 needUpdate，最后一次迭代会把 true 冲回 false，
// 镜像有多个 repoDigest 时会漏报更新。
//
// 这里的策略：优先只比对"仓库名与 ImageName 一致"的那条 repoDigest —— 那才是我们
// 接下来要拉取的仓库；镜像可能被 retag 到别的仓库，那些 digest 与本次更新无关，
// 混在一起比会误报。确实找不到匹配仓库时，退回"任一不一致即需更新"，
// 保证敏感度不低于旧行为。
func compareDigest(image types.Image, remoteDigest string) ImageCheckList {
	var mismatched bool
	matchedRepository := false

	for _, localRepoDigest := range image.RepoDigests {
		parts := strings.SplitN(localRepoDigest, "@", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		localRepo, localDigest := parts[0], parts[1]
		if !sameRepository(localRepo, image.ImageName) {
			continue
		}
		matchedRepository = true
		if localDigest != remoteDigest {
			logx.Info(image.ImageName + ":" + image.ImageTag + " need update")
			logx.Infof("localDigest: %s, remoteDigest: %s", localDigest, remoteDigest)
			return ImageCheckList{NeedUpdate: true}
		}
	}

	if matchedRepository {
		logx.Info(image.ImageName + ":" + image.ImageTag + " not need update")
		return ImageCheckList{}
	}

	// 没有仓库名匹配的本地 digest，退化成整体比对。
	for _, localRepoDigest := range image.RepoDigests {
		parts := strings.SplitN(localRepoDigest, "@", 2)
		if len(parts) == 2 && parts[1] != "" && parts[1] != remoteDigest {
			mismatched = true
			break
		}
	}
	if mismatched {
		logx.Infof("无同名仓库的 repoDigest，按整体比对判定需要更新: %s:%s", image.ImageName, image.ImageTag)
		return ImageCheckList{NeedUpdate: true}
	}
	logx.Info(image.ImageName + ":" + image.ImageTag + " not need update")
	return ImageCheckList{}
}

// sameRepository 判断本地 repoDigest 的仓库名是否与镜像名指向同一个仓库。
// docker.io 官方镜像的 repoDigest 常省略 library/ 前缀，两种写法都要认。
func sameRepository(localRepo, imageName string) bool {
	if localRepo == imageName {
		return true
	}
	return strings.TrimPrefix(localRepo, "library/") == strings.TrimPrefix(imageName, "library/")
}

func BuildManifestURL(image types.Image) (string, error) {
	normalizedRef, err := ref.ParseDockerRef(image.ImageName + ":" + image.ImageTag)
	if err != nil {
		return "", err
	}
	normalizedTaggedRef, isTagged := normalizedRef.(ref.NamedTagged)
	if !isTagged {
		return "", errors.New("镜像无tag" + normalizedRef.String())
	}

	host, ErrGetRegistryAddress := GetRegistryAddress(normalizedTaggedRef.Name())
	img, tag := ref.Path(normalizedTaggedRef), normalizedTaggedRef.Tag()

	if ErrGetRegistryAddress != nil {
		return "", ErrGetRegistryAddress
	}

	url := url2.URL{
		Scheme: "https",
		Host:   host,
		Path:   fmt.Sprintf("/v2/%s/manifests/%s", img, tag),
	}
	return url.String(), nil
}

func GetDigest(url string, token string) (string, error) {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}

	req, _ := http.NewRequest("HEAD", url, nil)

	if token != "" {
		req.Header.Add("Authorization", token)
	}
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.oci.image.index.v1+json")

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetDigest关闭body失败" + err.Error())
		}
	}(res.Body)

	if res.StatusCode != 200 {
		wwwAuthHeader := res.Header.Get("www-authenticate")
		if wwwAuthHeader == "" {
			wwwAuthHeader = "not present"
		}
		return "", fmt.Errorf("registry responded to head request with %q, auth: %q", res.Status, wwwAuthHeader)
	}
	return res.Header.Get(ContentDigestHeader), nil
}
