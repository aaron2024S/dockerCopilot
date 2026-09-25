package container

import (
	"context"
	"github.com/onlyLTY/dockerCopilot/internal/utiles"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ContainersListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

type Info struct {
	Id          string `json:"id"`
	Status      string `json:"status"`
	Name        string `json:"name"`
	UsingImage  string `json:"usingImage"`
	CreateImage string `json:"createImage"`
	CreateTime  string `json:"createTime"`
	RunningTime string `json:"runningTime"`
	HaveUpdate  bool   `json:"haveUpdate"`
	// Ports 端口列表，已映射到宿主机的排在前面；空数组表示没映射任何端口
	Ports []types.PortInfo `json:"ports"`
	// NetworkMode 网络模式，host 表示直接用宿主机网络（此时 Ports 为空是正常的）
	NetworkMode string `json:"networkMode"`
}

func NewContainersListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ContainersListLogic {
	return &ContainersListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ContainersListLogic) ContainersList() (resp *types.Resp, err error) {
	// 获取所有容器（包括停止的容器）
	resp = &types.Resp{}
	list, err := utiles.GetContainerList(l.svcCtx)
	if err != nil {
		resp.Code = 500
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Msg = "success"
	var containerInfoList []Info
	list = utiles.CheckImageUpdate(l.svcCtx, list)
	for _, v := range list {
		var containerInfo Info
		containerInfo.Id = v.ID
		containerInfo.Status = v.State
		if len(v.Names) > 0 {
			ContainerName := v.Names[0][1:]
			containerInfo.Name = ContainerName
		} else {
			containerInfo.Name = "get container name error"
			l.Error("get container name error" + v.ID)
		}
		if v.Image != "" {
			containerInfo.UsingImage = v.Image
		} else {
			containerInfo.UsingImage = v.ImageID
			l.Error("image dont have name" + v.ID)
		}
		// 端口映射记录为空时退回用容器 EXPOSE 的端口，理由见 ExposedSpecsToPortList 注释。
		// 注意 GetContainerInspect 出错时返回的是零值，Config 为 nil，
		// 原写法直接取 .Config.Image 会 panic，这里必须判空。
		var exposedSpecs []string
		containerInspect, err := utiles.GetContainerInspect(l.svcCtx, v.ID)
		if err != nil {
			containerInfo.CreateImage = ""
			l.Error("get image name error" + v.ID)
		} else if containerInspect.Config != nil {
			containerInfo.CreateImage = containerInspect.Config.Image
			for p := range containerInspect.Config.ExposedPorts {
				exposedSpecs = append(exposedSpecs, string(p))
			}
		}
		t := time.Unix(v.Created, 0)
		containerInfo.CreateTime = t.Format("2006-01-02 15:04:05")
		containerInfo.RunningTime = v.Status
		containerInfo.HaveUpdate = v.Update
		containerInfo.Ports = utiles.BuildPortList(v)
		if len(containerInfo.Ports) == 0 {
			// host 网络没有 NAT 映射记录；普通容器没做 -p 时也读不到映射。
			// 这两种情况都靠 EXPOSE 兜底，至少让用户知道声明了哪些端口。
			containerInfo.Ports = utiles.ExposedSpecsToPortList(exposedSpecs, utiles.IsHostNetwork(v))
		}
		containerInfo.NetworkMode = string(v.HostConfig.NetworkMode)
		containerInfoList = append(containerInfoList, containerInfo)
	}
	resp.Data = containerInfoList
	return resp, nil
}
