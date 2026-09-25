package svc

import (
	"sync"
	"time"

	"github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
)

const (
	// maxProgressTasks 进度表条目硬上限，超过就开始回收。
	maxProgressTasks = 500

	// progressTaskTTL 已完成的进度条目保留时长，够前端轮询到结果即可。
	progressTaskTTL = 2 * time.Hour
)

type ServiceContext struct {
	Config                     config.Config
	CookieCheckMiddleware      rest.Middleware
	Jwtuuid                    string
	BearerTokenCheckMiddleware rest.Middleware
	JwtSecret                  string
	PortainerJwt               string
	HubImageInfo               *module.ImageUpdateData
	IndexCheckMiddleware       rest.Middleware
	ProgressStore              ProgressStoreType
	DockerClient               *client.Client
	// AutoUpdate 自动更新的持久化配置（开关 / 排除列表 / 删旧镜像）。
	AutoUpdate *module.AutoUpdateStore
	// AutoUpdateRunner 自动更新调度器。
	// 这里用接口而不是具体类型，是为了让 svc 不必反向依赖 utiles 包
	// （utiles 依赖 svc，直接引用会形成循环）。具体实现由 main 注入。
	AutoUpdateRunner AutoUpdateRunner
	// UpdateLock 更新容器的全局互斥锁：手动更新与自动更新共用，
	// 保证任意时刻只有一台容器在被 stop / rename / create。
	UpdateLock sync.Mutex
	mu         sync.Mutex
}

// AutoUpdateRunner 自动更新调度器需要暴露给 HTTP 层的能力。
type AutoUpdateRunner interface {
	// Run 执行一轮自动更新，trigger 取值见 module.TriggerCron / module.TriggerManual。
	Run(trigger string) module.AutoUpdateRunSummary
	// Candidates 返回所有容器的自动更新筛选结论。
	Candidates() ([]module.AutoUpdateCandidate, error)
	// IsRunning 当前是否有一轮自动更新正在执行。
	IsRunning() bool
}

type TaskProgress struct {
	TaskID     string
	Percentage int
	Message    string
	Name       string
	DetailMsg  string
	IsDone     bool
	// UpdatedAt 仅用于内部回收，不返回给前端。
	UpdatedAt time.Time `json:"-"`
}

type ProgressStoreType map[string]TaskProgress

func NewServiceContext(c config.Config) *ServiceContext {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logx.Errorf("Unable to create docker client: %s", err)
	}
	ctx := &ServiceContext{
		Config:        c,
		HubImageInfo:  module.NewImageCheck(),
		ProgressStore: make(ProgressStoreType),
		DockerClient:  cli,
		AutoUpdate:    module.NewAutoUpdateStore(),
	}
	return ctx
}

func (ctx *ServiceContext) UpdateProgress(taskID string, progress TaskProgress) {
	progress.UpdatedAt = time.Now()

	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.ProgressStore[taskID] = progress
	ctx.sweepProgressLocked()
}

func (ctx *ServiceContext) GetProgress(taskID string) (TaskProgress, bool) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	progress, ok := ctx.ProgressStore[taskID]
	return progress, ok
}

// sweepProgressLocked 回收历史进度条目。
// 自动更新每小时可能产生多条任务，不回收的话这个 map 会一直涨。
// 调用方必须已持有 ctx.mu。
func (ctx *ServiceContext) sweepProgressLocked() {
	if len(ctx.ProgressStore) <= maxProgressTasks {
		return
	}
	deadline := time.Now().Add(-progressTaskTTL)
	for taskID, progress := range ctx.ProgressStore {
		if progress.IsDone && progress.UpdatedAt.Before(deadline) {
			delete(ctx.ProgressStore, taskID)
		}
	}
	if len(ctx.ProgressStore) <= maxProgressTasks {
		return
	}
	// 仍然超限就按时间从旧到新继续丢已完成的条目。
	type entry struct {
		taskID    string
		updatedAt time.Time
	}
	var done []entry
	for taskID, progress := range ctx.ProgressStore {
		if progress.IsDone {
			done = append(done, entry{taskID: taskID, updatedAt: progress.UpdatedAt})
		}
	}
	for i := 0; i < len(done) && len(ctx.ProgressStore) > maxProgressTasks; i++ {
		oldest := i
		for j := i + 1; j < len(done); j++ {
			if done[j].updatedAt.Before(done[oldest].updatedAt) {
				oldest = j
			}
		}
		done[i], done[oldest] = done[oldest], done[i]
		delete(ctx.ProgressStore, done[i].taskID)
	}
}
