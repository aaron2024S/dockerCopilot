package module

// 自动更新触发来源。
const (
	// TriggerCron 定时任务触发，需要遵守总开关。
	TriggerCron = "cron"
	// TriggerManual 用户手动点"立即执行"，无视总开关。
	TriggerManual = "manual"
)

// ContainerSnapshot 容器在筛选阶段需要的最小信息集，
// 由上层从 Docker 列表 + 更新检测缓存组装出来。
type ContainerSnapshot struct {
	ID      string
	Name    string
	Image   string
	ImageID string
	State   string
	// HaveUpdate 该容器所用镜像当前是否检测到有更新。
	HaveUpdate bool
}

// AutoUpdateCandidate 单台容器的筛选结论。
// 前端可以据此完整展示"哪些会被自动更新、哪些被跳过、原因是什么"。
type AutoUpdateCandidate struct {
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	ImageID       string `json:"imageId"`
	State         string `json:"state"`
	HaveUpdate    bool   `json:"haveUpdate"`
	// Excluded 命中用户配置的排除列表。
	Excluded bool `json:"excluded"`
	// Protected 命中自身保护（dockerCopilot 容器），优先级高于排除列表。
	Protected bool `json:"protected"`
	// WillUpdate 本台容器会被本轮自动更新选中。
	WillUpdate bool `json:"willUpdate"`
	// ExcludeReason 被跳过的原因，WillUpdate 为真时为空串。
	ExcludeReason string `json:"excludeReason"`
}

// AutoUpdateRunSummary 一轮自动更新的执行摘要，供接口返回与历史留档。
type AutoUpdateRunSummary struct {
	Trigger    string             `json:"trigger"`
	StartedAt  string             `json:"startedAt"`
	FinishedAt string             `json:"finishedAt"`
	// Skipped 本轮没有实际执行（开关关闭、或上一轮还没结束）。
	Skipped bool `json:"skipped"`
	// Reason 跳过原因。
	Reason string `json:"reason"`
	// Updated 本轮实际处理过的容器结果，含成功与失败。
	Updated []AutoUpdateRecord `json:"updated"`
}

// PlanAutoUpdate 依据配置筛选出本轮应当自动更新的容器。
// 纯函数，不触碰 Docker，方便单测覆盖各种排除组合。
func PlanAutoUpdate(containers []ContainerSnapshot, setting AutoUpdateSetting) []AutoUpdateCandidate {
	candidates := make([]AutoUpdateCandidate, 0, len(containers))
	for _, container := range containers {
		item := AutoUpdateCandidate{
			ContainerID:   container.ID,
			ContainerName: container.Name,
			Image:         container.Image,
			ImageID:       container.ImageID,
			State:         container.State,
			HaveUpdate:    container.HaveUpdate,
		}
		switch {
		case !container.HaveUpdate:
			item.ExcludeReason = "未检测到更新"
		case setting.ProtectSelf && IsSelfContainer(container.Name, container.Image):
			// 自身保护永远优先：先判断它，用户即使手动把 dockerCopilot 加进排除列表
			// 之外也不会被更新选中，避免在更新过程中把自己重启掉。
			item.Protected = true
			item.ExcludeReason = "dockerCopilot 自身，永不自动更新"
		case MatchesExcludeList(setting.ExcludeList, container.Name, container.Image):
			item.Excluded = true
			item.ExcludeReason = "命中排除列表"
		default:
			item.WillUpdate = true
		}
		candidates = append(candidates, item)
	}
	return candidates
}

// MatchesExcludeList 判断容器是否命中排除列表。
// 容器名、镜像全名、镜像仓库名三者都参与匹配，支持 redis* 这类通配写法。
func MatchesExcludeList(excludeList []string, containerName, imageName string) bool {
	if len(excludeList) == 0 {
		return false
	}
	imageRepo := stripImageTag(imageName)
	for _, pattern := range excludeList {
		if matchExcludePattern(pattern, containerName) ||
			matchExcludePattern(pattern, imageName) ||
			matchExcludePattern(pattern, imageRepo) {
			return true
		}
	}
	return false
}
