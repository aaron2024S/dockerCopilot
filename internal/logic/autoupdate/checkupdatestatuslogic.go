package autoupdate

import (
	"context"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// CheckUpdateStatusResp 一次检测的状态快照。
//
// 存在的意义：POST /api/checkUpdate 是同步接口，而一轮检测的耗时不可控
// （取决于镜像数量和 registry 响应速度）。前端等待到上限后只能主动断开，
// 但那只是「客户端放弃等待」—— 服务端并不受影响，仍会跑完并把结论写进内存。
// 有了这个接口，前端断开后可以转成轮询，直到拿到真实结论，
// 而不是把超时当成「检测失败」报给用户。
type CheckUpdateStatusResp struct {
	// Running 当前是否有检测正在执行。
	Running bool `json:"running"`
	// StartedAt 本轮开始时间；空字符串表示当前没有在跑。
	StartedAt string `json:"startedAt"`
	// LastCheckedAt 上一轮完成时间；空字符串表示从未检测过。
	//
	// 时间戳带纳秒精度（RFC3339Nano）：前端是靠「这个值变了没有」判断本轮是否结束的，
	// 秒级精度在同秒内完成的连续两轮里会撞车，导致轮询一直等不到结果。
	LastCheckedAt string `json:"lastCheckedAt"`
	// LastTotal 上一轮参与检测的镜像总数。
	LastTotal int `json:"lastTotal"`
	// LastNeedUpdateCount 上一轮判定有更新的镜像数量。
	LastNeedUpdateCount int `json:"lastNeedUpdateCount"`
}

type CheckUpdateStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCheckUpdateStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckUpdateStatusLogic {
	return &CheckUpdateStatusLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CheckUpdateStatus 返回镜像更新检测的当前状态。
//
// 只读内存快照：不触发检测、不发网络请求、不动任何数据，
// 因此前端可以放心高频轮询。
func (l *CheckUpdateStatusLogic) CheckUpdateStatus() (resp *types.Resp, err error) {
	resp = &types.Resp{}

	if l.svcCtx.HubImageInfo == nil {
		resp.Code = 500
		resp.Msg = "镜像更新检测模块未初始化"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	status := l.svcCtx.HubImageInfo.Status()

	out := CheckUpdateStatusResp{
		Running:             status.Running,
		LastTotal:           status.LastTotal,
		LastNeedUpdateCount: status.LastNeedUpdate,
	}
	if !status.StartedAt.IsZero() {
		out.StartedAt = status.StartedAt.Format(time.RFC3339Nano)
	}
	if !status.LastCheckedAt.IsZero() {
		out.LastCheckedAt = status.LastCheckedAt.Format(time.RFC3339Nano)
	}

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = out
	return resp, nil
}
