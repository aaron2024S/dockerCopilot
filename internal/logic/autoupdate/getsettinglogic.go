package autoupdate

import (
	"context"

	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// SettingResp 配置查询响应。
// 内嵌 AutoUpdateSetting 会被 encoding/json 拉平成同级字段，前端拿到的就是扁平对象。
type SettingResp struct {
	module.AutoUpdateSetting
	// ConfigPath 配置文件实际路径，便于排查"配置没生效"这类问题。
	ConfigPath string `json:"configPath"`
	// Running 当前是否有一轮自动更新正在执行。
	Running bool `json:"running"`
}

type GetSettingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewGetSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSettingLogic {
	return &GetSettingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetSettingLogic) GetSetting() (resp *types.Resp, err error) {
	resp = &types.Resp{}

	running := false
	if l.svcCtx.AutoUpdateRunner != nil {
		running = l.svcCtx.AutoUpdateRunner.IsRunning()
	}

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = SettingResp{
		AutoUpdateSetting: l.svcCtx.AutoUpdate.Get(),
		ConfigPath:        l.svcCtx.AutoUpdate.Path(),
		Running:           running,
	}
	return resp, nil
}
