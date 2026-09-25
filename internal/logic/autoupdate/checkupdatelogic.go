package autoupdate

import (
	"context"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/utiles"
	"github.com/zeromicro/go-zero/core/logx"
)

// CheckUpdateResp 手动检测更新的结果。
type CheckUpdateResp struct {
	// NeedUpdateCount 本次检测判定有更新的镜像数量。
	NeedUpdateCount int `json:"needUpdateCount"`
	// Total 本次参与检测的镜像数量。
	Total int `json:"total"`
	// CheckedAt 本次检测完成时间（RFC3339）。
	CheckedAt string `json:"checkedAt"`
	// Skipped 为 true 表示本次请求没等到结论 —— 已有一轮检测在跑，且等过了 CheckRunWaitTimeout。
	// 此时下面三个字段都不可信，前端应转去轮询 /checkUpdate/status 等那一轮自己跑完，
	// 而不是把上一轮的旧结论当成本次结果提示给用户。
	Skipped bool `json:"skipped"`
}

type CheckUpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCheckUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckUpdateLogic {
	return &CheckUpdateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CheckUpdate 立即手动执行一次镜像更新检测，结论写入内存缓存供容器列表读取。
//
// 定时检测在每小时 :30 执行；用户不想等到下个整点时，可以点这里立刻刷新结论。
// 这里同步等待结果再返回：一轮检测是若干个轻量 HEAD 请求，通常几秒到几十秒完成，
// 服务端配置的 Timeout 足够大，不会中途被切断；前端据返回值直接提示检测结果。
func (l *CheckUpdateLogic) CheckUpdate() (resp *types.Resp, err error) {
	resp = &types.Resp{}

	if l.svcCtx.HubImageInfo == nil {
		resp.Code = 500
		resp.Msg = "镜像更新检测模块未初始化"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	list, err := utiles.GetImagesList(l.svcCtx)
	if err != nil {
		resp.Code = 500
		resp.Msg = "获取镜像列表失败: " + err.Error()
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	// 整体重算并替换缓存，与定时任务走同一条路径，避免两套结论不一致。
	// 撞上已有检测时 CheckUpdate 会等它跑完，所以正常情况下这里返回的就是刚刷新的结论。
	if !l.svcCtx.HubImageInfo.CheckUpdate(list) {
		resp.Code = 200
		resp.Msg = "已有检测在进行中，请稍候查看结果"
		resp.Data = CheckUpdateResp{Skipped: true}
		return resp, nil
	}

	// 统计值取状态快照，与 /checkUpdate/status 返回的是同一份数字 ——
	// 「同步拿到结果」和「超时后转为轮询」两条路径给用户的结论必须完全一致。
	status := l.svcCtx.HubImageInfo.Status()

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = CheckUpdateResp{
		NeedUpdateCount: status.LastNeedUpdate,
		Total:           status.LastTotal,
		CheckedAt:       status.LastCheckedAt.Format(time.RFC3339),
	}
	return resp, nil
}
