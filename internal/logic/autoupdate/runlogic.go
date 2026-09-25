package autoupdate

import (
	"context"

	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type RunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunLogic {
	return &RunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// Run 立即手动执行一轮自动更新。
//
// 刻意做成异步：一轮要更新多台容器，每台都包含一次镜像拉取，耗时可能几分钟，
// 同步等待会把请求挂死。这里立刻返回，前端改为轮询 GET /api/autoUpdate
// 看 running 字段和 lastResult 结果 —— 和现有容器更新的"发起后轮询进度"是一致的。
func (l *RunLogic) Run() (resp *types.Resp, err error) {
	resp = &types.Resp{}

	if l.svcCtx.AutoUpdateRunner == nil {
		resp.Code = 500
		resp.Msg = "自动更新调度器未初始化"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}
	if l.svcCtx.AutoUpdateRunner.IsRunning() {
		resp.Code = 409
		resp.Msg = "上一轮自动更新还在执行中，请稍后再试"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	runner := l.svcCtx.AutoUpdateRunner
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("自动更新执行过程中发生 panic: %v", r)
			}
		}()
		// 用户主动触发，无视总开关，方便在开关关闭时先试跑一次看效果。
		runner.Run(module.TriggerManual)
	}()

	resp.Code = 200
	resp.Msg = "已开始执行自动更新，请稍后刷新查看结果"
	resp.Data = map[string]interface{}{}
	return resp, nil
}
