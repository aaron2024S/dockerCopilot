package container

import (
	"context"
	"os"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/utiles"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateLogic) Update(req *types.ContainerUpdateReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}

	// 与自动更新共用一把锁：自动任务正在跑时不允许再手动更新同一批容器，
	// 否则两边会同时 stop / rename / create，把容器搞成中间状态。
	if !l.svcCtx.UpdateLock.TryLock() {
		resp.Code = 409
		resp.Msg = "已有更新任务正在执行，请等当前任务结束后重试"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}

	taskID := uuid.New().String()
	go func() {
		defer l.svcCtx.UpdateLock.Unlock()
		// Catch any panic and log the error
		defer func() {
			if r := recover(); r != nil {
				l.Errorf("Recovered from panic in UpdateContainer: %v", r)
			}
		}()
		err := utiles.UpdateContainer(l.svcCtx, utiles.UpdateOptions{
			ContainerID:     req.Id,
			ContainerName:   req.ContainerName,
			ImageNameAndTag: req.ImageNameAndTag,
			DelOldContainer: os.Getenv("DelOldContainer") != "false",
			// 手动更新时也遵循自动更新配置里的"删旧镜像"开关，保持行为一致。
			DeleteOldImage: l.svcCtx.AutoUpdate.Get().DeleteOldImage,
			TaskID:         taskID,
		})
		if err != nil {
			l.Errorf("Error in UpdateContainer: %v", err)
		}
	}()

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]string{"taskID": taskID}
	return resp, nil
}
