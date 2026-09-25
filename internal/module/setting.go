package module

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// DefaultAutoUpdateConfigPath 自动更新配置的默认落盘位置。
	// 与前端 logo 配置（/data/config/imageLogos.js）同目录，部署时 /data 已挂载卷，重启不丢。
	DefaultAutoUpdateConfigPath = "/data/config/autoUpdate.json"

	// autoUpdateConfigEnv 可覆盖配置文件路径，方便本地调试与测试。
	autoUpdateConfigEnv = "AUTO_UPDATE_CONFIG"

	// autoUpdateMaxRecords 配置里保留的历史执行记录条数上限。
	autoUpdateMaxRecords = 50

	// autoUpdateMaxExclude 排除列表条数上限，防止配置被写爆。
	autoUpdateMaxExclude = 200

	// selfContainerKeyword 用于识别本工具自身，自动更新必须永久跳过它，
	// 否则会在执行过程中把自己重启掉。
	selfContainerKeyword = "dockercopilot"
)

// AutoUpdateRecord 单台容器的自动更新结果，供前端展示"上次自动更新做了什么"。
type AutoUpdateRecord struct {
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	FinishedAt    string `json:"finishedAt"`
}

// AutoUpdateSetting 自动更新功能持久化的全部配置。
type AutoUpdateSetting struct {
	Enabled        bool               `json:"enabled"`
	DeleteOldImage bool               `json:"deleteOldImage"`
	ExcludeList    []string           `json:"excludeList"`
	ProtectSelf    bool               `json:"protectSelf"`
	LastRunAt      string             `json:"lastRunAt"`
	LastTrigger    string             `json:"lastTrigger"`
	LastResult     []AutoUpdateRecord `json:"lastResult"`
}

// AutoUpdatePatch 局部更新请求。用指针区分"没传"和"传了 false/空数组"。
type AutoUpdatePatch struct {
	Enabled        *bool     `json:"enabled,optional"`
	DeleteOldImage *bool     `json:"deleteOldImage,optional"`
	ExcludeList    *[]string `json:"excludeList,optional"`
	ProtectSelf    *bool     `json:"protectSelf,optional"`
}

// defaultAutoUpdateSetting 首次运行时写入的默认配置。
// Enabled 默认 false —— 升级后不能擅自改变用户已有的更新行为。
func defaultAutoUpdateSetting() AutoUpdateSetting {
	return AutoUpdateSetting{
		Enabled:        false,
		DeleteOldImage: true,
		ExcludeList:    []string{},
		ProtectSelf:    true,
		LastResult:     []AutoUpdateRecord{},
	}
}

// AutoUpdateStore 配置的读写封装：内存缓存 + 加锁 + 原子落盘。
type AutoUpdateStore struct {
	mu      sync.RWMutex
	path    string
	setting AutoUpdateSetting
}

// NewAutoUpdateStore 加载配置。文件不存在时写入默认配置，文件损坏时备份后重建。
func NewAutoUpdateStore() *AutoUpdateStore {
	configPath := strings.TrimSpace(os.Getenv(autoUpdateConfigEnv))
	if configPath == "" {
		configPath = DefaultAutoUpdateConfigPath
	}
	store := &AutoUpdateStore{
		path:    configPath,
		setting: defaultAutoUpdateSetting(),
	}
	store.load()
	return store
}

// Path 返回配置文件实际路径，供日志与接口展示。
func (s *AutoUpdateStore) Path() string {
	return s.path
}

func (s *AutoUpdateStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := s.persist(s.setting); saveErr != nil {
				logx.Errorf("创建自动更新默认配置失败: %v", saveErr)
			} else {
				logx.Infof("已创建自动更新默认配置: %s", s.path)
			}
			return
		}
		logx.Errorf("读取自动更新配置失败，使用默认配置: %v", err)
		return
	}

	var loaded AutoUpdateSetting
	if err := json.Unmarshal(raw, &loaded); err != nil {
		// 损坏的配置不要静默丢弃，先留一份备份方便排查。
		brokenPath := s.path + ".broken"
		if renameErr := os.Rename(s.path, brokenPath); renameErr != nil {
			logx.Errorf("备份损坏的自动更新配置失败: %v", renameErr)
		} else {
			logx.Errorf("自动更新配置解析失败，已备份到 %s: %v", brokenPath, err)
		}
		if saveErr := s.persist(s.setting); saveErr != nil {
			logx.Errorf("重建自动更新默认配置失败: %v", saveErr)
		}
		return
	}

	s.mu.Lock()
	s.setting = loaded.sanitized()
	s.mu.Unlock()
	logx.Infof("已加载自动更新配置: enabled=%v, 排除 %d 项, deleteOldImage=%v",
		s.setting.Enabled, len(s.setting.ExcludeList), s.setting.DeleteOldImage)
}

// Get 返回配置副本，避免调用方直接改动内部状态。
func (s *AutoUpdateStore) Get() AutoUpdateSetting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.setting.clone()
}

// Apply 局部更新配置并落盘，返回更新后的完整配置。
func (s *AutoUpdateStore) Apply(patch AutoUpdatePatch) (AutoUpdateSetting, error) {
	s.mu.Lock()
	next := s.setting.clone()
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if patch.DeleteOldImage != nil {
		next.DeleteOldImage = *patch.DeleteOldImage
	}
	if patch.ProtectSelf != nil {
		next.ProtectSelf = *patch.ProtectSelf
	}
	if patch.ExcludeList != nil {
		next.ExcludeList = *patch.ExcludeList
	}
	next = next.sanitized()
	s.setting = next
	s.mu.Unlock()

	if err := s.persist(next); err != nil {
		return next, err
	}
	return next.clone(), nil
}

// RecordRun 记录一次自动更新执行结果，同时刷新 lastRunAt。
func (s *AutoUpdateStore) RecordRun(trigger string, records []AutoUpdateRecord, finishedAt time.Time) error {
	s.mu.Lock()
	next := s.setting.clone()
	next.LastRunAt = finishedAt.Format(time.RFC3339)
	next.LastTrigger = trigger
	// 最新的记录排在前面，便于前端直接取最近若干条展示。
	merged := make([]AutoUpdateRecord, 0, len(records)+len(next.LastResult))
	merged = append(merged, records...)
	merged = append(merged, next.LastResult...)
	next.LastResult = merged
	next = next.sanitized()
	s.setting = next
	s.mu.Unlock()

	return s.persist(next)
}

// IsExcluded 判断容器是否命中当前配置里的排除列表。
func (s *AutoUpdateStore) IsExcluded(containerName, imageName string) bool {
	return MatchesExcludeList(s.Get().ExcludeList, containerName, imageName)
}

// IsSelfContainer 依据容器名与镜像名识别 dockerCopilot 自身。
func IsSelfContainer(containerName, imageName string) bool {
	return strings.Contains(strings.ToLower(containerName), selfContainerKeyword) ||
		strings.Contains(strings.ToLower(imageName), selfContainerKeyword)
}

// persist 原子落盘：先写临时文件再 rename，避免写一半掉电导致配置损坏。
// 调用方需自行负责加锁与状态更新。
func (s *AutoUpdateStore) persist(setting AutoUpdateSetting) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(setting, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		// rename 失败时清掉临时文件，避免残留干扰下次写入。
		if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
			logx.Errorf("清理自动更新配置临时文件失败: %v", removeErr)
		}
		return err
	}
	return nil
}

// sanitized 归一化配置：排除列表去空去重限量，历史记录截断。
func (s AutoUpdateSetting) sanitized() AutoUpdateSetting {
	out := s

	seen := make(map[string]struct{}, len(out.ExcludeList))
	list := make([]string, 0, len(out.ExcludeList))
	for _, item := range out.ExcludeList {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		list = append(list, trimmed)
		if len(list) >= autoUpdateMaxExclude {
			logx.Errorf("自动更新排除列表超过 %d 项，已截断", autoUpdateMaxExclude)
			break
		}
	}
	out.ExcludeList = list

	if out.LastResult == nil {
		out.LastResult = []AutoUpdateRecord{}
	}
	if len(out.LastResult) > autoUpdateMaxRecords {
		out.LastResult = out.LastResult[:autoUpdateMaxRecords]
	}
	return out
}

// clone 深拷贝，防止切片被外部改动。
func (s AutoUpdateSetting) clone() AutoUpdateSetting {
	out := s
	if s.ExcludeList != nil {
		out.ExcludeList = append([]string(nil), s.ExcludeList...)
	}
	if s.LastResult != nil {
		out.LastResult = append([]AutoUpdateRecord(nil), s.LastResult...)
	}
	return out
}

// matchExcludePattern 大小写不敏感地做精确匹配或通配匹配。
func matchExcludePattern(pattern, value string) bool {
	if pattern == "" || value == "" {
		return false
	}
	lowerPattern := strings.ToLower(pattern)
	lowerValue := strings.ToLower(value)
	if lowerPattern == lowerValue {
		return true
	}
	matched, err := path.Match(lowerPattern, lowerValue)
	if err != nil {
		// 通配表达式本身非法时只退化成精确匹配，不中断整个匹配流程。
		return false
	}
	return matched
}

// stripImageTag 去掉镜像的 tag 部分，仅保留仓库名。
// 需要同时考虑 registry 端口号里的冒号，例如 registry.local:5000/nginx:latest。
func stripImageTag(imageName string) string {
	lastSlash := strings.LastIndex(imageName, "/")
	lastColon := strings.LastIndex(imageName, ":")
	if lastColon > lastSlash {
		return imageName[:lastColon]
	}
	return imageName
}
