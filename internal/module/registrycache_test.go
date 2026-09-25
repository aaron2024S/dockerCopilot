package module

import (
	"testing"
	"time"
)

// 缓存命中时 GetRegistryAddress 不应再发起任何探测。
// 断言用的是特意挑的假域名：如果缓存没生效，函数会真的去连官网/加速站，
// 几乎不可能返回这个值。
func TestGetRegistryAddressUsesCachedHost(t *testing.T) {
	ResetRegistryHostCache()
	defer ResetRegistryHostCache()

	const fake = "cache-probe.invalid"
	putRegistryHostCache(DefaultRegistryDomain, fake, time.Minute)

	for _, imageRef := range []string{"nginx", "nginx:latest", "library/redis:7-alpine"} {
		got, err := GetRegistryAddress(imageRef)
		if err != nil {
			t.Fatalf("GetRegistryAddress(%q) 返回错误: %v", imageRef, err)
		}
		if got != fake {
			t.Errorf("GetRegistryAddress(%q) = %q，期望命中缓存 %q", imageRef, got, fake)
		}
	}
}

// 镜像名里显式带了域名时，那个域名本身就可直连，不应被 Docker Hub 的缓存影响，
// 也不该产生网络请求。
func TestGetRegistryAddressKeepsExplicitRegistry(t *testing.T) {
	ResetRegistryHostCache()
	defer ResetRegistryHostCache()
	putRegistryHostCache(DefaultRegistryDomain, "cache-probe.invalid", time.Minute)

	cases := map[string]string{
		"ghcr.io/linuxserver/sonarr:latest": "ghcr.io",
		"registry.example.com/foo/bar:1.0":  "registry.example.com",
		"docker.io/library/nginx:latest":    "cache-probe.invalid",
	}
	for imageRef, want := range cases {
		got, err := GetRegistryAddress(imageRef)
		if err != nil {
			t.Fatalf("GetRegistryAddress(%q) 返回错误: %v", imageRef, err)
		}
		if got != want {
			t.Errorf("GetRegistryAddress(%q) = %q，期望 %q", imageRef, got, want)
		}
	}
}

// 过期条目必须失效，否则加速站切换后会一直沿用旧地址。
func TestRegistryHostCacheExpiry(t *testing.T) {
	ResetRegistryHostCache()
	defer ResetRegistryHostCache()

	putRegistryHostCache(DefaultRegistryDomain, "expired.invalid", -time.Second)
	if _, ok := getRegistryHostCache(DefaultRegistryDomain); ok {
		t.Error("已过期的缓存条目仍然命中")
	}

	putRegistryHostCache(DefaultRegistryDomain, "fresh.invalid", time.Minute)
	got, ok := getRegistryHostCache(DefaultRegistryDomain)
	if !ok || got != "fresh.invalid" {
		t.Errorf("未过期条目应命中，got=%q ok=%v", got, ok)
	}
}

func TestResetRegistryHostCache(t *testing.T) {
	putRegistryHostCache(DefaultRegistryDomain, "before-reset.invalid", time.Minute)
	ResetRegistryHostCache()
	if _, ok := getRegistryHostCache(DefaultRegistryDomain); ok {
		t.Error("ResetRegistryHostCache 之后仍然命中")
	}
}

// 并发读不应触发数据竞争（配合 -race 更有意义，这里至少保证不死锁、不落空）。
func TestRegistryHostCacheConcurrentRead(t *testing.T) {
	ResetRegistryHostCache()
	defer ResetRegistryHostCache()
	putRegistryHostCache(DefaultRegistryDomain, "concurrent.invalid", time.Minute)

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				if _, ok := getRegistryHostCache(DefaultRegistryDomain); !ok {
					t.Error("并发读时缓存未命中")
					return
				}
			}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}
