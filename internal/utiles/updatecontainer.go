package utiles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/core/logx"
)

// UpdateOptions 更新一台容器所需的全部入参。
// 原先散落的一串位置参数在加入"自动更新""删旧镜像"之后会越来越难维护，统一收进结构体。
type UpdateOptions struct {
	ContainerID     string
	ContainerName   string
	ImageNameAndTag string
	// DelOldContainer 是否删除被替换下来的旧容器。沿用 DelOldContainer 环境变量的语义。
	DelOldContainer bool
	// DeleteOldImage 是否在更新成功后清理旧镜像。仅在确认无其他容器引用时才会真正删除。
	DeleteOldImage bool
	TaskID         string
}

// UpdateContainer 把容器更新到目标镜像。
//
// 相比旧实现，这里补了四件自动更新场景必需的事：
//  1. 拉取前记录旧镜像 ID 与原始运行状态；
//  2. 更新成功且确认无引用后删除旧镜像；
//  3. 原本处于停止状态的容器，更新完依然保持停止，不擅自拉起服务；
//  4. 创建/启动新容器失败时回滚——删掉半成品并让旧容器以原名和原状态复活。
func UpdateContainer(serviceContext *svc.ServiceContext, opts UpdateOptions) error {
	ctx := context.Background()
	progress := newUpdateProgress(serviceContext, opts.TaskID, opts.ContainerName)

	progress.set(0, "正在连接Docker", "正在连接Docker")
	serviceContext.DockerClient.NegotiateAPIVersion(ctx)

	// 1. 先摸清旧容器的底细。原镜像 ID、真实容器名、原始运行状态，
	//    这三项在回滚和"不擅自启动已停止容器"时都要用到。
	inspectedContainer, err := serviceContext.DockerClient.ContainerInspect(ctx, opts.ContainerID)
	if err != nil {
		logx.Errorf("获取容器信息失败: %v", err)
		return progress.fail("获取容器信息失败", err)
	}
	oldImageID := inspectedContainer.Image
	originalName := strings.TrimPrefix(inspectedContainer.Name, "/")
	if originalName == "" {
		originalName = opts.ContainerName
	}
	wasRunning := inspectedContainer.State != nil && inspectedContainer.State.Running

	// 2. 拉取新镜像
	progress.set(10, "正在拉取新镜像", "正在拉取新镜像")
	reader, err := serviceContext.DockerClient.ImagePull(ctx, opts.ImageNameAndTag, image.PullOptions{})
	if err != nil {
		logx.Errorf("Failed to pull image: %s", err)
		return progress.fail("拉取镜像失败", err)
	}
	pullErr := decodePullResp(reader, func(detail string) {
		progress.set(25, "正在拉取新镜像", detail)
	})
	if closeErr := reader.Close(); closeErr != nil {
		logx.Errorf("关闭拉取镜像响应流失败: %v", closeErr)
	}
	if pullErr != nil {
		logx.Errorf("Failed to pull image: %s", pullErr)
		return progress.fail("拉取镜像失败", pullErr)
	}

	// 拉取完成后 tag 已指向新镜像，取它的 ID 用于判断镜像是否真的发生了变化。
	newImageID := resolveImageID(serviceContext, opts.ImageNameAndTag)

	// 3. 停止旧容器
	progress.set(30, "正在停止容器", "正在停止容器")
	stopOptions := container.StopOptions{
		Signal:  "SIGINT",
		Timeout: intPtr(10),
	}
	if err := serviceContext.DockerClient.ContainerStop(ctx, opts.ContainerID, stopOptions); err != nil {
		logx.Errorf("停止容器失败: %v", err)
		return progress.fail("停止容器失败", err)
	}

	// 4. 重命名旧容器，给新容器腾出名字
	progress.set(40, "正在重命名旧容器", "正在重命名旧容器")
	if err := serviceContext.DockerClient.ContainerRename(ctx, opts.ContainerID, originalName+"-"+time.Now().Format("2006-01-02-15-04-05")); err != nil {
		logx.Errorf("重命名旧容器失败: %v", err)
		// 旧容器此时只是被停了，名字没变，直接恢复原状即可。
		restoreContainerState(serviceContext, opts.ContainerID, wasRunning)
		return progress.fail("重命名旧容器失败", err)
	}

	// 5. 用旧容器的配置创建新容器
	progress.set(60, "正在创建新容器", "正在创建新容器")
	inspectedContainer.Config.Hostname = ""
	inspectedContainer.Config.Image = opts.ImageNameAndTag
	inspectedContainer.Image = opts.ImageNameAndTag
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: inspectedContainer.NetworkSettings.Networks,
	}
	created, err := serviceContext.DockerClient.ContainerCreate(
		ctx,
		inspectedContainer.Config,
		inspectedContainer.HostConfig,
		networkingConfig,
		nil,
		originalName,
	)
	if err != nil {
		logx.Errorf("创建新容器失败: %v", err)
		// 新容器没建出来，名字是空的，可以把旧容器改回来。
		rollbackOldContainer(serviceContext, opts.ContainerID, originalName, wasRunning)
		return progress.fail("创建新容器失败，已回滚旧容器", err)
	}

	// 6. 启动新容器。启动失败必须先删掉这个半成品，否则名字被占住，旧容器改不回来。
	progress.set(80, "正在启动新容器", "正在启动新容器")
	if err := serviceContext.DockerClient.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		logx.Errorf("启动新容器失败: %v", err)
		removeContainerQuietly(serviceContext, created.ID)
		rollbackOldContainer(serviceContext, opts.ContainerID, originalName, wasRunning)
		return progress.fail("启动新容器失败，已回滚旧容器", err)
	}

	// 7. 还原原始运行状态。原本就停着的容器，更新完不能顺手把它启动起来。
	if !wasRunning {
		if err := serviceContext.DockerClient.ContainerStop(ctx, created.ID, stopOptions); err != nil {
			logx.Errorf("恢复容器原停止状态失败（新容器可能已启动）: %v", err)
		} else {
			progress.set(85, "已保持容器停止状态", "容器原本处于停止状态，更新后维持停止")
		}
	}

	// 8. 删除旧容器。这一步失败不影响"更新成功"的结论，只记日志。
	if opts.DelOldContainer {
		if err := serviceContext.DockerClient.ContainerRemove(ctx, opts.ContainerID, container.RemoveOptions{}); err != nil {
			logx.Errorf("删除旧容器 %s 失败（不影响本次更新）: %v", opts.ContainerID, err)
		}
	}

	// 9. 清理旧镜像。必须放在旧容器摘除之后，否则旧容器仍在引用它。
	if opts.DeleteOldImage {
		progress.set(90, "正在清理旧镜像", "正在清理旧镜像")
		deleteOldImageIfUnused(serviceContext, oldImageID, newImageID, opts.ImageNameAndTag)
	}

	progress.done("更新成功", "更新成功")
	return nil
}

// resolveImageID 取镜像当前指向的 ID。取不到时返回空串，调用方会退化为"不删旧镜像"。
func resolveImageID(serviceContext *svc.ServiceContext, imageNameAndTag string) string {
	inspected, _, err := serviceContext.DockerClient.ImageInspectWithRaw(context.Background(), imageNameAndTag)
	if err != nil {
		logx.Errorf("获取镜像 %s 的ID失败: %v", imageNameAndTag, err)
		return ""
	}
	return inspected.ID
}

// deleteOldImageIfUnused 安全删除旧镜像，三重保险缺一不可：
// 开关为真、新旧镜像 ID 不同、确认没有其他容器还在引用。
// 任何一步不满足都只写日志，绝不让整台容器的更新任务失败。
func deleteOldImageIfUnused(serviceContext *svc.ServiceContext, oldImageID, newImageID, imageNameAndTag string) {
	if oldImageID == "" {
		return
	}
	// 镜像没实质变化（例如 tag 被重新指向同一层）时，oldImageID 就是新镜像，
	// 此时删除会把刚拉下来的镜像干掉，必须跳过。
	if newImageID != "" && oldImageID == newImageID {
		logx.Infof("镜像 %s 实际未变化，跳过删除旧镜像", imageNameAndTag)
		return
	}
	stillUsed, err := isImageInUse(serviceContext, oldImageID)
	if err != nil {
		logx.Errorf("检查旧镜像引用关系失败，跳过删除 %s: %v", oldImageID, err)
		return
	}
	if stillUsed {
		logx.Infof("旧镜像 %s 仍被其他容器引用，跳过删除", oldImageID)
		return
	}
	if err := RemoveImage(serviceContext, oldImageID, false); err != nil {
		logx.Errorf("删除旧镜像 %s 失败（不影响本次更新）: %v", oldImageID, err)
		return
	}
	logx.Infof("已删除旧镜像 %s", oldImageID)
}

// isImageInUse 判断某个镜像是否还被任意容器（含已停止）引用。
func isImageInUse(serviceContext *svc.ServiceContext, imageID string) (bool, error) {
	list, err := serviceContext.DockerClient.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		return false, err
	}
	for _, item := range list {
		if item.ImageID == imageID {
			return true, nil
		}
	}
	return false, nil
}

// rollbackOldContainer 更新失败时把旧容器救回来：改回原名，并恢复它原本的启停状态。
// 调用前必须确保占用该名字的新容器已经被删除。
func rollbackOldContainer(serviceContext *svc.ServiceContext, containerID, originalName string, wasRunning bool) {
	ctx := context.Background()
	if err := serviceContext.DockerClient.ContainerRename(ctx, containerID, originalName); err != nil {
		logx.Errorf("回滚失败：旧容器无法改回原名 %s: %v", originalName, err)
		return
	}
	if !restoreContainerState(serviceContext, containerID, wasRunning) {
		return
	}
	logx.Infof("已回滚旧容器 %s（原运行状态 running=%v）", originalName, wasRunning)
}

// restoreContainerState 按原始状态恢复容器，返回是否成功。
func restoreContainerState(serviceContext *svc.ServiceContext, containerID string, wasRunning bool) bool {
	if !wasRunning {
		// 原本就停着，改名后可保持停止，不需要额外操作。
		return true
	}
	if err := serviceContext.DockerClient.ContainerStart(context.Background(), containerID, container.StartOptions{}); err != nil {
		logx.Errorf("回滚失败：旧容器 %s 无法重新启动: %v", containerID, err)
		return false
	}
	return true
}

// removeContainerQuietly 清理创建失败或启动失败的新容器，避免它占着名字导致回滚做不成。
func removeContainerQuietly(serviceContext *svc.ServiceContext, containerID string) {
	if containerID == "" {
		return
	}
	err := serviceContext.DockerClient.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
	if err != nil {
		logx.Errorf("清理失败的新容器 %s 失败: %v", containerID, err)
	}
}

// updateProgress 收敛任务进度上报，避免散落的赋值把状态改花。
type updateProgress struct {
	svcCtx *svc.ServiceContext
	taskID string
	value  svc.TaskProgress
}

func newUpdateProgress(svcCtx *svc.ServiceContext, taskID, name string) *updateProgress {
	return &updateProgress{
		svcCtx: svcCtx,
		taskID: taskID,
		value: svc.TaskProgress{
			TaskID: taskID,
			Name:   name,
		},
	}
}

func (p *updateProgress) set(percentage int, message, detail string) {
	p.value.Percentage = percentage
	p.value.Message = message
	p.value.DetailMsg = detail
	p.svcCtx.UpdateProgress(p.taskID, p.value)
}

func (p *updateProgress) done(message, detail string) {
	p.value.Percentage = 100
	p.value.Message = message
	p.value.DetailMsg = detail
	p.value.IsDone = true
	p.svcCtx.UpdateProgress(p.taskID, p.value)
}

func (p *updateProgress) fail(message string, err error) error {
	p.value.Message = message
	p.value.DetailMsg = err.Error()
	p.value.Percentage = 25
	p.value.IsDone = true
	p.svcCtx.UpdateProgress(p.taskID, p.value)
	return err
}

func decodePullResp(reader io.Reader, onProgress func(detail string)) error {
	decoder := json.NewDecoder(reader)
	for {
		var msg dockerMsgType.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			logx.Errorf("Failed to decode pull image response: %s", err)
			return fmt.Errorf("拉取镜像失败: %w", err)
		}
		if msg.Error != nil {
			logx.Errorf("Error: %s", msg.Error)
			return fmt.Errorf("拉取镜像失败: %w", msg.Error)
		}
		var formattedMsg string
		if msg.Progress != nil {
			formattedMsg = fmt.Sprintf("进度%s: %s", msg.Status, msg.Progress.String())
		} else {
			formattedMsg = fmt.Sprintf("进度%s", msg.Status)
		}
		onProgress(formattedMsg)
		logx.Infof("拉取镜像进度\t %s: %s\n", msg.Status, msg.Progress)
	}
}

func intPtr(v int) *int {
	return &v
}
