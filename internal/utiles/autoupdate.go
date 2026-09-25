package utiles

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/core/logx"
)

// AutoUpdateRunner 自动更新调度器。
//
// 触发时机：没有自己的独立定时点，而是跟在每小时 :30 的镜像更新检测之后执行
// （注册处见 dockercopilot.go）；手动触发走 POST /api/autoUpdate/run。
//
// 流程：保证 digest 缓存新鲜 -> 按配置筛出候选容器 -> 逐台串行更新并清理旧镜像。
// 刻意串行而非并发：并行更新会让 Docker daemon 同时停止并创建多个容器，
// 既容易打满资源，一旦出问题也很难定位是哪台引起的。
type AutoUpdateRunner struct {
	svcCtx *svc.ServiceContext
	store  *module.AutoUpdateStore
	// running 防止上一轮还没跑完下一轮又进来，把任务堆积起来。
	running atomic.Bool
}

// NewAutoUpdateRunner 创建调度器。
func NewAutoUpdateRunner(svcCtx *svc.ServiceContext) *AutoUpdateRunner {
	return &AutoUpdateRunner{
		svcCtx: svcCtx,
		store:  svcCtx.AutoUpdate,
	}
}

// IsRunning 当前是否有一轮自动更新正在执行。
func (r *AutoUpdateRunner) IsRunning() bool {
	return r.running.Load()
}

// Candidates 返回当前所有容器的自动更新筛选结论。
// 前端用它渲染"哪些容器会被自动更新、哪些被跳过、原因是什么"。
func (r *AutoUpdateRunner) Candidates() ([]module.AutoUpdateCandidate, error) {
	return r.buildCandidates()
}

// Run 执行一轮自动更新。
// trigger 为 cron 时会先检查总开关；manual 是用户主动触发，无视总开关。
func (r *AutoUpdateRunner) Run(trigger string) module.AutoUpdateRunSummary {
	summary := module.AutoUpdateRunSummary{
		Trigger:   trigger,
		StartedAt: time.Now().Format(time.RFC3339),
		Updated:   []module.AutoUpdateRecord{},
	}

	// 定时任务重叠属于正常现象，记一行日志后跳过即可，不算失败。
	if !r.running.CompareAndSwap(false, true) {
		summary.Skipped = true
		summary.Reason = "上一轮自动更新尚未结束，跳过本次"
		summary.FinishedAt = time.Now().Format(time.RFC3339)
		logx.Info("自动更新跳过：" + summary.Reason)
		return summary
	}
	defer r.running.Store(false)

	setting := r.store.Get()
	if trigger == module.TriggerCron && !setting.Enabled {
		summary.Skipped = true
		summary.Reason = "自动更新开关未开启"
		summary.FinishedAt = time.Now().Format(time.RFC3339)
		return summary
	}

	// 1. 先确认 digest 缓存新鲜，避免拿着过期的"有更新"结论去重启容器。
	r.refreshDigestIfStale()

	// 2. 规划本轮目标
	candidates, err := r.buildCandidates()
	if err != nil {
		summary.Skipped = true
		summary.Reason = "获取容器列表失败: " + err.Error()
		summary.FinishedAt = time.Now().Format(time.RFC3339)
		logx.Errorf("自动更新中止：%s", summary.Reason)
		return summary
	}

	targets := make([]module.AutoUpdateCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.WillUpdate {
			targets = append(targets, candidate)
		}
	}
	logx.Infof("自动更新开始（%s）：命中 %d 台容器", trigger, len(targets))

	// 3. 逐台串行更新
	successCount := 0
	for _, target := range targets {
		record := r.updateOne(target, setting.DeleteOldImage)
		summary.Updated = append(summary.Updated, record)
		if record.Success {
			successCount++
		}
	}

	finishedAt := time.Now()
	summary.FinishedAt = finishedAt.Format(time.RFC3339)
	logx.Infof("自动更新结束：成功 %d / 共 %d 台", successCount, len(targets))

	// 4. 留档
	if err := r.store.RecordRun(trigger, summary.Updated, finishedAt); err != nil {
		logx.Errorf("写入自动更新执行记录失败: %v", err)
	}

	// 5. 刷新检测缓存，让前端列表里的"有更新"标记及时消失。
	if len(targets) > 0 {
		r.refreshDigest()
	}
	return summary
}

// updateOne 更新一台容器。
// 拿不到更新锁说明用户正在手动操作，这台直接跳过等下一轮，绝不抢锁硬上。
func (r *AutoUpdateRunner) updateOne(target module.AutoUpdateCandidate, deleteOldImage bool) module.AutoUpdateRecord {
	record := module.AutoUpdateRecord{
		ContainerID:   target.ContainerID,
		ContainerName: target.ContainerName,
		Image:         target.Image,
	}

	if !r.svcCtx.UpdateLock.TryLock() {
		record.Success = false
		record.Message = "已有其他更新任务在执行，本轮跳过"
		record.FinishedAt = time.Now().Format(time.RFC3339)
		logx.Infof("自动更新跳过 %s：%s", target.ContainerName, record.Message)
		return record
	}
	defer r.svcCtx.UpdateLock.Unlock()

	err := UpdateContainer(r.svcCtx, UpdateOptions{
		ContainerID:     target.ContainerID,
		ContainerName:   target.ContainerName,
		ImageNameAndTag: target.Image,
		DelOldContainer: os.Getenv("DelOldContainer") != "false",
		DeleteOldImage:  deleteOldImage,
		TaskID:          uuid.New().String(),
	})
	record.FinishedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		record.Success = false
		record.Message = err.Error()
		logx.Errorf("自动更新 %s 失败: %v", target.ContainerName, err)
		return record
	}
	record.Success = true
	record.Message = "更新成功"
	logx.Infof("自动更新 %s 成功", target.ContainerName)
	return record
}

// buildCandidates 组装容器快照并交给纯函数筛选。
func (r *AutoUpdateRunner) buildCandidates() ([]module.AutoUpdateCandidate, error) {
	containers, err := GetContainerList(r.svcCtx)
	if err != nil {
		return nil, err
	}
	snapshots := make([]module.ContainerSnapshot, 0, len(containers))
	for _, container := range containers {
		snapshots = append(snapshots, module.ContainerSnapshot{
			ID:         container.ID,
			Name:       ContainerName(container),
			Image:      ContainerImage(container),
			ImageID:    container.ImageID,
			State:      container.State,
			HaveUpdate: r.svcCtx.HubImageInfo.NeedUpdate(container.ImageID),
		})
	}
	return module.PlanAutoUpdate(snapshots, r.store.Get()), nil
}

func (r *AutoUpdateRunner) refreshDigestIfStale() {
	if !r.svcCtx.HubImageInfo.IsStale(module.ImageCheckFreshness) {
		return
	}
	r.refreshDigest()
}

func (r *AutoUpdateRunner) refreshDigest() {
	list, err := GetImagesList(r.svcCtx)
	if err != nil {
		logx.Errorf("刷新镜像更新检测失败: %v", err)
		return
	}
	// 撞上已有检测时 CheckUpdate 会等它跑完再返回，正常都能拿到新鲜结论；
	// 只有等到超时才返回 false —— 此时沿用现有缓存继续，不因为刷新失败就中断本轮自动更新。
	if !r.svcCtx.HubImageInfo.CheckUpdate(list) {
		logx.Error("刷新镜像更新检测超时，本轮沿用现有缓存")
	}
}
