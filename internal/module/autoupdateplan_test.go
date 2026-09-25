package module

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore 把配置落到临时目录，避免污染 /data/config。
func newTestStore(t *testing.T) *AutoUpdateStore {
	t.Helper()
	t.Setenv(autoUpdateConfigEnv, filepath.Join(t.TempDir(), "autoUpdate.json"))
	return NewAutoUpdateStore()
}

func candidateByIndex(t *testing.T, candidates []AutoUpdateCandidate, index int) AutoUpdateCandidate {
	t.Helper()
	if index >= len(candidates) {
		t.Fatalf("候选列表长度不足，期望至少 %d 项，实际 %d", index+1, len(candidates))
	}
	return candidates[index]
}

func TestPlanAutoUpdateDefaultSetting(t *testing.T) {
	containers := []ContainerSnapshot{
		{ID: "1", Name: "nginx", Image: "nginx:latest", HaveUpdate: true},
		{ID: "2", Name: "redis", Image: "redis:7", HaveUpdate: false},
		{ID: "3", Name: "dockerCopilot", Image: "0nlylty/dockercopilot:latest", HaveUpdate: true},
	}
	candidates := PlanAutoUpdate(containers, defaultAutoUpdateSetting())

	if got := candidateByIndex(t, candidates, 0); !got.WillUpdate {
		t.Errorf("nginx 有更新且未排除，应当被自动更新，实际 ExcludeReason=%q", got.ExcludeReason)
	}
	if got := candidateByIndex(t, candidates, 1); got.WillUpdate {
		t.Errorf("redis 没有检测到更新，不应被自动更新")
	}
	// dockerCopilot 自身必须永远被跳过，否则会在更新过程中把自己重启掉。
	if got := candidateByIndex(t, candidates, 2); got.WillUpdate || !got.Protected {
		t.Errorf("dockerCopilot 自身必须受保护，实际 WillUpdate=%v Protected=%v", got.WillUpdate, got.Protected)
	}
}

func TestPlanAutoUpdateExcludeList(t *testing.T) {
	containers := []ContainerSnapshot{
		{ID: "1", Name: "mysql", Image: "mysql:8.0", HaveUpdate: true},
		{ID: "2", Name: "redis-cache", Image: "redis:7-alpine", HaveUpdate: true},
		{ID: "3", Name: "web", Image: "myreg.local:5000/app:1.0", HaveUpdate: true},
		{ID: "4", Name: "worker", Image: "registry.io/team/worker:v2", HaveUpdate: true},
	}

	setting := defaultAutoUpdateSetting()
	setting.ExcludeList = []string{"mysql", "redis*", "myreg.local:5000/app"}
	candidates := PlanAutoUpdate(containers, setting)

	if got := candidateByIndex(t, candidates, 0); got.WillUpdate || !got.Excluded {
		t.Errorf("mysql 命中精确排除，应被跳过，实际 WillUpdate=%v", got.WillUpdate)
	}
	if got := candidateByIndex(t, candidates, 1); got.WillUpdate || !got.Excluded {
		t.Errorf("redis-cache 命中 redis* 通配排除，应被跳过，实际 WillUpdate=%v", got.WillUpdate)
	}
	if got := candidateByIndex(t, candidates, 2); got.WillUpdate {
		t.Errorf("带端口号的私有仓库镜像应按仓库名命中排除，应被跳过，实际 WillUpdate=%v", got.WillUpdate)
	}
	if got := candidateByIndex(t, candidates, 3); !got.WillUpdate {
		t.Errorf("worker 未被排除，应当被自动更新，实际 ExcludeReason=%q", got.ExcludeReason)
	}
}

func TestPlanAutoUpdateProtectSelfDisabled(t *testing.T) {
	containers := []ContainerSnapshot{
		{ID: "1", Name: "dockerCopilot", Image: "0nlylty/dockercopilot:latest", HaveUpdate: true},
	}
	setting := defaultAutoUpdateSetting()
	setting.ProtectSelf = false

	if got := candidateByIndex(t, PlanAutoUpdate(containers, setting), 0); !got.WillUpdate {
		t.Errorf("关闭自身保护后 dockerCopilot 应可被更新，实际 ExcludeReason=%q", got.ExcludeReason)
	}
}

func TestPlanAutoUpdateSelfProtectionBeatsExcludeReason(t *testing.T) {
	// 自身保护必须先于排除列表判断，这样即使用户没把 dockercopilot 写进排除列表，
	// 返回的原因也是"自身保护"而不是"未命中排除列表"。
	containers := []ContainerSnapshot{
		{ID: "1", Name: "dockerCopilot", Image: "0nlylty/dockercopilot:latest", HaveUpdate: true},
	}
	candidates := PlanAutoUpdate(containers, defaultAutoUpdateSetting())
	if got := candidateByIndex(t, candidates, 0); !got.Protected || got.Excluded {
		t.Errorf("应判定为 Protected 而非 Excluded，实际 Protected=%v Excluded=%v", got.Protected, got.Excluded)
	}
}

func TestMatchesExcludeList(t *testing.T) {
	cases := []struct {
		name          string
		excludeList   []string
		containerName string
		imageName     string
		want          bool
	}{
		{"空列表不排除任何容器", nil, "nginx", "nginx:latest", false},
		{"容器名精确匹配", []string{"nginx"}, "nginx", "nginx:latest", true},
		{"容器名大小写不敏感", []string{"NGINX"}, "nginx", "nginx:latest", true},
		{"按镜像仓库名匹配", []string{"redis"}, "cache", "redis:7", true},
		{"按镜像全名匹配", []string{"redis:7"}, "cache", "redis:7", true},
		{"通配匹配容器名", []string{"web-*"}, "web-1", "nginx:latest", true},
		{"通配不误伤", []string{"web-*"}, "api-1", "nginx:latest", false},
		{"带端口仓库按仓库名匹配", []string{"reg.local:5000/app"}, "app", "reg.local:5000/app:1.0", true},
		{"不相关容器不排除", []string{"mysql"}, "nginx", "nginx:latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesExcludeList(tc.excludeList, tc.containerName, tc.imageName); got != tc.want {
				t.Errorf("MatchesExcludeList(%v, %q, %q) = %v, 期望 %v",
					tc.excludeList, tc.containerName, tc.imageName, got, tc.want)
			}
		})
	}
}

func TestStripImageTag(t *testing.T) {
	cases := map[string]string{
		"nginx:latest":                "nginx",
		"nginx":                       "nginx",
		"registry.local:5000/app:1.0": "registry.local:5000/app",
		"registry.local:5000/app":     "registry.local:5000/app",
		"a/b/c:v1":                    "a/b/c",
	}
	for input, want := range cases {
		if got := stripImageTag(input); got != want {
			t.Errorf("stripImageTag(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestAutoUpdateStoreCreatesDefaultConfig(t *testing.T) {
	store := newTestStore(t)

	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("首次启动应自动创建配置文件: %v", err)
	}
	setting := store.Get()
	// 升级后不能擅自打开自动更新，否则用户一升级容器就被批量重启。
	if setting.Enabled {
		t.Error("默认配置的自动更新开关必须是关闭的")
	}
	if !setting.ProtectSelf {
		t.Error("默认配置必须开启自身保护")
	}
	if !setting.DeleteOldImage {
		t.Error("默认配置应当开启删除旧镜像")
	}
}

func TestAutoUpdateStorePersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoUpdate.json")
	t.Setenv(autoUpdateConfigEnv, path)

	store := NewAutoUpdateStore()
	enabled := true
	deleteOld := false
	exclude := []string{"mysql", "  redis*  ", "mysql", ""}
	if _, err := store.Apply(AutoUpdatePatch{
		Enabled:        &enabled,
		DeleteOldImage: &deleteOld,
		ExcludeList:    &exclude,
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	// 重新加载：模拟进程重启，验证配置真的落盘了。
	reloaded := NewAutoUpdateStore()
	got := reloaded.Get()
	if !got.Enabled {
		t.Error("enabled 未持久化")
	}
	if got.DeleteOldImage {
		t.Error("deleteOldImage 未持久化")
	}
	want := []string{"mysql", "redis*"}
	if len(got.ExcludeList) != len(want) {
		t.Fatalf("排除列表应去空去重，期望 %v，实际 %v", want, got.ExcludeList)
	}
	for i := range want {
		if got.ExcludeList[i] != want[i] {
			t.Errorf("排除列表第 %d 项 = %q，期望 %q（需去掉首尾空格并去重）", i, got.ExcludeList[i], want[i])
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置文件失败: %v", err)
	}
	var onDisk AutoUpdateSetting
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("落盘的配置不是合法 JSON: %v", err)
	}
}

func TestAutoUpdateStoreNilExcludeListKeepsCurrent(t *testing.T) {
	store := newTestStore(t)
	exclude := []string{"mysql"}
	if _, err := store.Apply(AutoUpdatePatch{ExcludeList: &exclude}); err != nil {
		t.Fatalf("写入排除列表失败: %v", err)
	}

	// 前端只切开关、不带排除列表时，不能把已有排除项清空。
	enabled := true
	updated, err := store.Apply(AutoUpdatePatch{Enabled: &enabled})
	if err != nil {
		t.Fatalf("写入开关失败: %v", err)
	}
	if len(updated.ExcludeList) != 1 || updated.ExcludeList[0] != "mysql" {
		t.Errorf("只更新开关时排除列表应保持原值，实际 %v", updated.ExcludeList)
	}

	// 明确传空数组才是用户主动清空。
	empty := []string{}
	cleared, err := store.Apply(AutoUpdatePatch{ExcludeList: &empty})
	if err != nil {
		t.Fatalf("清空排除列表失败: %v", err)
	}
	if len(cleared.ExcludeList) != 0 {
		t.Errorf("传空数组应清空排除列表，实际 %v", cleared.ExcludeList)
	}
}

func TestAutoUpdateStoreRecoversFromBrokenConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoUpdate.json")
	t.Setenv(autoUpdateConfigEnv, path)
	if err := os.WriteFile(path, []byte("{ 这不是合法 json"), 0644); err != nil {
		t.Fatalf("准备损坏配置失败: %v", err)
	}

	store := NewAutoUpdateStore()
	if store.Get().Enabled {
		t.Error("配置损坏时应回退到默认（关闭）配置")
	}
	if _, err := os.Stat(path + ".broken"); err != nil {
		t.Errorf("损坏的配置应被备份为 .broken 便于排查: %v", err)
	}
}

func TestAutoUpdateStoreRecordRun(t *testing.T) {
	store := newTestStore(t)

	first := []AutoUpdateRecord{{ContainerName: "a", Success: true}}
	if err := store.RecordRun(TriggerCron, first, time.Now()); err != nil {
		t.Fatalf("写入执行记录失败: %v", err)
	}
	second := []AutoUpdateRecord{{ContainerName: "b", Success: false, Message: "拉取失败"}}
	if err := store.RecordRun(TriggerCron, second, time.Now()); err != nil {
		t.Fatalf("写入执行记录失败: %v", err)
	}

	got := store.Get()
	if got.LastRunAt == "" {
		t.Error("lastRunAt 应当被写入")
	}
	if len(got.LastResult) != 2 {
		t.Fatalf("应累积 2 条记录，实际 %d", len(got.LastResult))
	}
	// 最新一轮的结果排在最前，前端直接截取前几条就是最近执行情况。
	if got.LastResult[0].ContainerName != "b" {
		t.Errorf("最新记录应排在最前，实际 %+v", got.LastResult)
	}
	// 记录不能无限增长。
	records := make([]AutoUpdateRecord, autoUpdateMaxRecords+10)
	for i := range records {
		records[i] = AutoUpdateRecord{ContainerName: "x"}
	}
	if err := store.RecordRun(TriggerCron, records, time.Now()); err != nil {
		t.Fatalf("写入执行记录失败: %v", err)
	}
	if n := len(store.Get().LastResult); n > autoUpdateMaxRecords {
		t.Errorf("历史记录应被截断到 %d 条，实际 %d", autoUpdateMaxRecords, n)
	}
}
