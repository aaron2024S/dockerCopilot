package autoupdate

import (
	"context"

	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateSettingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewUpdateSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateSettingLogic {
	return &UpdateSettingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdateSetting 更新自动更新配置。
// 排除列表缺省（nil）时保持原值，只有明确传了数组才会覆盖，
// 避免前端只切开关却把排除列表误清空。
func (l *UpdateSettingLogic) UpdateSetting(req *types.AutoUpdateUpdateReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}

	patch := module.AutoUpdatePatch{
		Enabled:        &req.Enabled,
		DeleteOldImage: &req.DeleteOldImage,
		ProtectSelf:    &req.ProtectSelf,
	}
	if req.ExcludeList != nil {
		patch.ExcludeList = &req.ExcludeList
	}

	setting, err := l.svcCtx.AutoUpdate.Apply(patch)
	if err != nil {
		l.Errorf("保存自动更新配置失败: %v", err)
		resp.Code = 500
		resp.Msg = "保存自动更新配置失败: " + err.Error()
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	l.Infof("自动更新配置已更新: enabled=%v, deleteOldImage=%v, 排除 %d 项",
		setting.Enabled, setting.DeleteOldImage, len(setting.ExcludeList))
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = setting
	return resp, nil
}
