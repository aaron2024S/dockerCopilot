package module

import (
	"encoding/json"
	"errors"
	"fmt"
	ref "github.com/distribution/reference"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const ChallengeHeader = "WWW-Authenticate"
const (
	DefaultRegistryDomain = "docker.io"
	DefaultRegistryHost   = "index.docker.io"
)

var DefaultAcceleratorHostList = []string{"docker.1ms.run", "docker.m.daocloud.io",
	"docker.1panel.top", "docker.1panel.live", "proxy.1panel.live", "dockerproxy.1panel.live", "docker.1panel.dev",
	"docker.anye.in", "hub.rat.dev", "docker.amingg.com"}

func GetToken(image types.Image, registryAuth string) (string, error) {
	logx.Infof("image name %s", image.ImageName)
	normalizedRef, err := ref.ParseNormalizedNamed(image.ImageName)
	if err != nil {
		return "", err
	}

	URL := GetChallengeURL(normalizedRef)

	var req *http.Request
	if req, err = GetChallengeRequest(URL); err != nil {
		return "", err
	}

	client := &http.Client{}
	var res *http.Response
	if res, err = client.Do(req); err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetToken关闭Body失败" + err.Error())
		}
	}(res.Body)
	v := res.Header.Get(ChallengeHeader)

	challenge := strings.ToLower(v)
	if strings.HasPrefix(challenge, "basic") {
		if registryAuth == "" {
			return "", fmt.Errorf("no credentials available")
		}

		return fmt.Sprintf("Basic %s", registryAuth), nil
	}
	if strings.HasPrefix(challenge, "bearer") {
		return GetBearerHeader(challenge, normalizedRef, registryAuth)
	}

	return "", errors.New("unsupported challenge type from registry")
}

func GetChallengeRequest(URL url.URL) (*http.Request, error) {
	req, err := http.NewRequest("GET", URL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Watchtower (Docker)")
	return req, nil
}

func GetBearerHeader(challenge string, imageRef ref.Named, registryAuth string) (string, error) {
	client := http.Client{}
	authURL, err := GetAuthURL(challenge, imageRef)

	if err != nil {
		return "", err
	}

	var r *http.Request
	if r, err = http.NewRequest("GET", authURL.String(), nil); err != nil {
		return "", err
	}

	if registryAuth != "" {
		logx.Info("私有镜像，无法获取是否有更新")
		r.Header.Add("Authorization", fmt.Sprintf("Basic %s", registryAuth))
	} else {
		logx.Info("No credentials found.")
	}

	var authResponse *http.Response
	if authResponse, err = client.Do(r); err != nil {
		return "", err
	}

	body, _ := io.ReadAll(authResponse.Body)
	tokenResponse := &types.TokenResponse{}

	err = json.Unmarshal(body, tokenResponse)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Bearer %s", tokenResponse.Token), nil
}

func GetAuthURL(challenge string, imageRef ref.Named) (*url.URL, error) {
	loweredChallenge := strings.ToLower(challenge)
	raw := strings.TrimPrefix(loweredChallenge, "bearer")

	pairs := strings.Split(raw, ",")
	values := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		trimmed := strings.Trim(pair, " ")
		if key, val, ok := strings.Cut(trimmed, "="); ok {
			values[key] = strings.Trim(val, `"`)
		}
	}
	if values["realm"] == "" || values["service"] == "" {

		return nil, fmt.Errorf("challenge header did not include all values needed to construct an auth url")
	}

	authURL, _ := url.Parse(values["realm"])
	q := authURL.Query()
	q.Add("service", values["service"])

	scopeImage := ref.Path(imageRef)

	scope := fmt.Sprintf("repository:%s:pull", scopeImage)
	q.Add("scope", scope)

	authURL.RawQuery = q.Encode()
	return authURL, nil
}

func GetChallengeURL(imageRef ref.Named) url.URL {
	host, _ := GetRegistryAddress(imageRef.Name())

	URL := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   "/v2/",
	}
	return URL
}

// ---------------------------------------------------------- registry 地址探测缓存

// 为什么要缓存这一段：
// 对 docker.io 系的镜像，GetRegistryAddress 要逐个体检候选 host —— 先试官网
// index.docker.io，不通再依次试 10 个加速站，每个 checkHost 都有 5 秒超时。
// 而一轮镜像更新检测里，每个镜像会调用它两次（GetToken -> GetChallengeURL 一次、
// BuildManifestURL 一次）。47 个镜像就是 94 次探测：官网不通时每次白等 5 秒，
// 累计能把一轮检测拖到十几分钟 —— 这正是前端 3 分钟超时的来源。
//
// 关键点：这个决策只跟 registry 域名有关，跟具体镜像无关（同一个 registry 必然走同一个
// host），所以按域名缓存一份结论就够，不需要按镜像缓存。
var registryHostCache = struct {
	mu    sync.RWMutex
	items map[string]registryHostCacheEntry
}{items: make(map[string]registryHostCacheEntry)}

type registryHostCacheEntry struct {
	host      string
	expiresAt time.Time
}

const (
	// RegistryHostCacheTTL 探测成功后的缓存时长。
	RegistryHostCacheTTL = 10 * time.Minute
	// RegistryHostCacheFailTTL 候选全部探不通时的缓存时长。
	// 负缓存刻意取短值：既避免每轮检测都重付「11 × 5 秒」，
	// 又保证加速站恢复后不用等太久就能被重新探到。
	RegistryHostCacheFailTTL = time.Minute
)

// getRegistryHostCache 读缓存，第二个返回值表示是否命中且未过期。
func getRegistryHostCache(domain string) (string, bool) {
	registryHostCache.mu.RLock()
	entry, ok := registryHostCache.items[domain]
	registryHostCache.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.host, true
}

func putRegistryHostCache(domain, host string, ttl time.Duration) {
	registryHostCache.mu.Lock()
	if registryHostCache.items == nil {
		registryHostCache.items = make(map[string]registryHostCacheEntry)
	}
	registryHostCache.items[domain] = registryHostCacheEntry{
		host:      host,
		expiresAt: time.Now().Add(ttl),
	}
	registryHostCache.mu.Unlock()
}

// ResetRegistryHostCache 清空探测缓存，供测试使用。
func ResetRegistryHostCache() {
	registryHostCache.mu.Lock()
	registryHostCache.items = make(map[string]registryHostCacheEntry)
	registryHostCache.mu.Unlock()
}

// GetRegistryAddress 返回访问该镜像所在 registry 时实际要连的 host。
//
// 镜像名里显式带了域名（ghcr.io、私有 registry）时，那个域名本身就可直连，
// 没有候选可挑 —— 直接返回，不进缓存也不产生任何网络请求。
// 只有 docker.io 需要探测，结果按域名做 TTL 缓存（见上面的说明）。
func GetRegistryAddress(imageRef string) (string, error) {
	normalizedRef, err := ref.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", err
	}

	address := ref.Domain(normalizedRef)
	if address != DefaultRegistryDomain {
		return address, nil
	}
	return resolveDockerHubHost(), nil
}

// resolveDockerHubHost 取出（或探出）访问 Docker Hub 用的 host。
func resolveDockerHubHost() string {
	if host, ok := getRegistryHostCache(DefaultRegistryDomain); ok {
		return host
	}
	host, found := probeDockerHubHost()
	ttl := RegistryHostCacheTTL
	if !found {
		ttl = RegistryHostCacheFailTTL
	}
	putRegistryHostCache(DefaultRegistryDomain, host, ttl)
	return host
}

// probeDockerHubHost 依次体检候选 host，返回第一个可用的。
// found=false 表示候选全部不可用，调用方退回官网地址（与原实现的兜底行为一致）。
func probeDockerHubHost() (host string, found bool) {
	if checkHost(DefaultRegistryHost) {
		return DefaultRegistryHost, true
	}
	logx.Info("Docker Hub 官网不可达，开始尝试镜像加速站")
	for _, candidate := range DefaultAcceleratorHostList {
		if checkHost(candidate) {
			logx.Infof("使用镜像加速站 %s 访问 Docker Hub", candidate)
			return candidate, true
		}
	}
	logx.Error("Docker Hub 官网与全部加速站均不可达，仍按官网地址尝试")
	return DefaultRegistryHost, false
}

func checkHost(host string) bool {
	URL := "https://" + host + "/v2/"
	// 创建带有超时设置的 http.Client
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	// 发送 HEAD 请求
	resp, err := client.Get(URL)
	if err != nil {
		logx.Errorf("Failed to connect to %s: %s", URL, err)
		return false
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Errorf("关闭body失败" + err.Error())
		}
	}(resp.Body)

	// 检查 HTTP 响应状态码
	if resp.StatusCode == http.StatusOK ||
		resp.StatusCode == http.StatusUnauthorized {
		return true
	}

	logx.Errorf("Failed to connect to %s: %s", URL, resp.Status)
	return false
}
