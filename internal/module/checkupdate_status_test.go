package module

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

// newTestImage 造一个测试用镜像。
// ID 来自内嵌的 image.Summary：提升字段能读，但**不能**直接写在结构体字面量里，
// 必须像这样显式包一层 Summary。
func newTestImage(id, name, tag string) types.Image {
	return types.Image{
		Summary:   image.Summary{ID: id},
		ImageName: name,
		ImageTag:  tag,
	}
}

func TestCheckUpdateStatusInitialState(t *testing.T) {
	d := NewImageCheck()

	got := d.Status()
	if got.Running {
		t.Error("刚构造时应为「没在跑」")
	}
	if !got.LastCheckedAt.IsZero() {
		t.Error("刚构造时应为「从未检测过」")
	}
}

// tryRun / finishRun 是状态接口的全部依据，单独验证它们。
func TestTryRunAndFinishToggleRunning(t *testing.T) {
	d := NewImageCheck()

	if !d.tryRun() {
		t.Fatal("空闲时 tryRun 应返回 true")
	}
	if !d.Status().Running {
		t.Error("tryRun 之后 Running 应为 true")
	}
	if d.Status().StartedAt.IsZero() {
		t.Error("tryRun 之后 StartedAt 不应为零值")
	}

	d.finishRun()
	if d.Status().Running {
		t.Error("finishRun 之后 Running 应为 false")
	}
	if d.Status().LastCheckedAt.IsZero() {
		t.Error("finishRun 之后 LastCheckedAt 应被刷新")
	}
}

// 已有检测在跑时，第二次 tryRun 必须立刻失败 —— 这是"不叠加并发检测"的核心。
func TestTryRunRejectsConcurrentRun(t *testing.T) {
	d := NewImageCheck()

	if !d.tryRun() {
		t.Fatal("第一轮应能取得运行权")
	}
	if d.tryRun() {
		t.Error("已有检测在跑时不应再次取得运行权")
	}

	d.finishRun()

	if !d.tryRun() {
		t.Error("上一轮结束后应能重新取得运行权")
	}
	d.finishRun()
}

// waitRun 应当等到前一轮结束才返回，而不是空转返回。
func TestWaitRunBlocksUntilRoundFinishes(t *testing.T) {
	d := NewImageCheck()

	if !d.tryRun() {
		t.Fatal("第一轮应能取得运行权")
	}
	// 模拟"前一轮跑到一半"：稍后收尾
	go func() {
		time.Sleep(60 * time.Millisecond)
		d.finishRun()
	}()

	start := time.Now()
	if !d.waitRun(2 * time.Second) {
		t.Fatal("应等到前一轮结束后返回 true")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("只等了 %v，说明没有真的等待前一轮结束", elapsed)
	}
}

// 等超时返回 false，且不能影响前一轮的运行状态。
func TestWaitRunGivesUpAfterTimeout(t *testing.T) {
	d := NewImageCheck()

	if !d.tryRun() {
		t.Fatal("第一轮应能取得运行权")
	}

	start := time.Now()
	if d.waitRun(80 * time.Millisecond) {
		t.Error("前一轮一直没结束时应等超时并返回 false")
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Errorf("只等了 %v，说明没有真的等到超时", elapsed)
	}
	if !d.Status().Running {
		t.Error("等待方的失败不应影响前一轮的运行状态")
	}

	// 等待者不能把信号通道弄坏：前一轮收尾后，新一轮仍应能正常进入
	d.finishRun()
	if !d.tryRun() {
		t.Error("前一轮结束后应能重新取得运行权")
	}
	d.finishRun()
}

// 并发压测：多个调用方同时抢运行权，验证
//   1) 不会死锁（抢不到的都靠 waitRun 正常返回）；
//   2) 任意时刻最多只有一轮在跑 —— 这正是这次加闸的目的。
//
// 本机没有 cgo 编译器、跑不了 -race，这个用例用运行计数把"重叠"直接测出来。
func TestConcurrentRunsNeverOverlap(t *testing.T) {
	d := NewImageCheck()

	const workers = 24
	var wg sync.WaitGroup
	var running int32
	var peak int32

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if !d.tryRun() {
				// 抢不到就是撞上了别人的一轮：等它结束即可（真实调用方走的就是这条路）
				if !d.waitRun(2 * time.Second) {
					t.Error("waitRun 超时，说明状态没收尾或存在死锁")
				}
				return
			}

			cur := atomic.AddInt32(&running, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			time.Sleep(3 * time.Millisecond) // 装作在跑一轮检测
			atomic.AddInt32(&running, -1)
			d.finishRun()
		}()
	}
	wg.Wait()

	if peak > 1 {
		t.Errorf("峰值有 %d 轮检测同时在跑，应当最多 1 轮", peak)
	}
	if d.Status().Running {
		t.Error("全部结束后不应还留在运行态")
	}
}

// 撞上已有检测时，CheckUpdate 应等它结束并复用结论，而不是再跑一遍。
//
// 验证方式：占住运行权 -> 起一个 goroutine 模拟"前一轮收尾" -> 调用 CheckUpdate。
// 若真的复用了结论，它不会去遍历 imageList（列表里放一条会触发联网请求的镜像就能看出来），
// 这里用 LastTotal 判断：复用路径下统计值保持前一轮写的值不变。
func TestCheckUpdateCoalescesWithRunningRound(t *testing.T) {
	d := NewImageCheck()

	// 先写下一轮的结论（手动构造，不经过 CheckUpdate）
	d.mu.Lock()
	d.lastTotal = 7
	d.lastNeedUpdate = 3
	d.mu.Unlock()

	if !d.tryRun() {
		t.Fatal("应能取得运行权")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.finishRun()
	}()

	list := []types.Image{newTestImage("sha256:aaa", "nginx", "latest")}
	if !d.CheckUpdate(list) {
		t.Fatal("应等到前一轮结束后返回 true")
	}

	got := d.Status()
	if got.LastTotal != 7 || got.LastNeedUpdate != 3 {
		t.Errorf("统计值被改写为 total=%d need=%d，说明又跑了一遍而不是复用已有结论",
			got.LastTotal, got.LastNeedUpdate)
	}
	if got.Running {
		t.Error("复用路径不应把自己留在运行态")
	}
}

// CheckUpdate 的首尾置位与统计值。
//
// 构造的镜像没有 RepoDigests：checkSingleImage 会在发任何请求之前就返回，
// 所以这个用例不联网，跑得很快。
func TestCheckUpdateRecordsStatsWithoutNetwork(t *testing.T) {
	d := NewImageCheck()
	list := []types.Image{
		newTestImage("sha256:aaa", "nginx", "latest"),
		newTestImage("sha256:bbb", "redis", "alpine"),
	}

	d.CheckUpdate(list)

	got := d.Status()
	if got.Running {
		t.Error("检测结束后 Running 应为 false")
	}
	if got.LastCheckedAt.IsZero() {
		t.Error("检测结束后 LastCheckedAt 应被刷新")
	}
	if got.LastTotal != 2 {
		t.Errorf("LastTotal = %d，期望 2", got.LastTotal)
	}
	if got.LastNeedUpdate != 0 {
		t.Errorf("LastNeedUpdate = %d，期望 0（无 repoDigest 无法比对，不算有更新）", got.LastNeedUpdate)
	}
}

// 自身镜像（dockercopilot）不参与检测，但总数仍与传入列表一致。
func TestCheckUpdateSkipsSelfImage(t *testing.T) {
	d := NewImageCheck()
	list := []types.Image{
		newTestImage("sha256:self", "0nlylty/dockercopilot", "latest"),
		newTestImage("sha256:other", "nginx", "latest"),
	}

	d.CheckUpdate(list)

	if d.NeedUpdate("sha256:self") {
		t.Error("自身镜像不应被标记为有更新")
	}
	if got := d.Status().LastTotal; got != 2 {
		t.Errorf("LastTotal = %d，期望 2（与接口返回的 total 口径保持一致）", got)
	}
}
