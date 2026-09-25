package utiles

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

func GetContainerList(ctx *svc.ServiceContext) ([]MyType.Container, error) {
	// 获取所有容器（包括停止的容器）
	dockerContainerList, err := ctx.DockerClient.ContainerList(context.Background(), container.ListOptions{
		All: true, // 设置为true来获取所有容器
	})
	if err != nil {
		logx.Errorf("get container list error: %v", err)
		return nil, err
	}
	var containerList []MyType.Container
	for _, dockerContainerInfo := range dockerContainerList {
		containerInfo := MyType.Container{
			Container: dockerContainerInfo,
		}
		containerList = append(containerList, containerInfo)
	}
	return containerList, nil
}

func CheckImageUpdate(ctx *svc.ServiceContext, containerListData []MyType.Container) []MyType.Container {
	for i, v := range containerListData {
		if ctx.HubImageInfo.NeedUpdate(v.ImageID) {
			containerListData[i].Update = true
		}
	}
	return containerListData
}

// ContainerName 取容器的可读名称。
// Docker API 返回的名字带前导斜杠（/nginx），对外展示和匹配都要去掉。
func ContainerName(container MyType.Container) string {
	if len(container.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(container.Names[0], "/")
}

// ContainerImage 取容器使用的镜像名，取不到时退回镜像 ID。
func ContainerImage(container MyType.Container) string {
	if container.Image != "" {
		return container.Image
	}
	return container.ImageID
}
