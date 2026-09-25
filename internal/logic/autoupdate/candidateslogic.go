package autoupdate

import (
	"context"

	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// CandidatesResp 自动更新候选容器清单。
type CandidatesResp struct {
	Running bool `json:"running"`
	// WillUpdate 本轮会被自动更新的容器数量，供前端直接展示。
	WillUpdate int `json:"willUpdate"`
	// Candidates 全部容器的筛选结论，含被跳过的及其原因。
	Candidates []module.AutoUpdateCandidate `json:"candidates"`
}

type CandidatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCandidatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CandidatesLogic {
	return &CandidatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// Candidates 返回当前所有容器的自动更新筛选结论。
// 前端用它渲染排除列表的勾选项，让用户直接选容器而不是手打容器名。
func (l *CandidatesLogic) Candidates() (resp *types.Resp, err error) {
	resp = &types.Resp{}

	if l.svcCtx.AutoUpdateRunner == nil {
		resp.Code = 500
		resp.Msg = "自动更新调度器未初始化"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	candidates, err := l.svcCtx.AutoUpdateRunner.Candidates()
	if err != nil {
		resp.Code = 500
		resp.Msg = "获取容器列表失败: " + err.Error()
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	willUpdate := 0
	for _, candidate := range candidates {
		if candidate.WillUpdate {
			willUpdate++
		}
	}

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = CandidatesResp{
		Running:    l.svcCtx.AutoUpdateRunner.IsRunning(),
		WillUpdate: willUpdate,
		Candidates: candidates,
	}
	return resp, nil
}
