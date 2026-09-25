package utiles

import (
	"reflect"
	"testing"

	dockertypes "github.com/docker/docker/api/types"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestNormalizePortsDeduplicateAndSort(t *testing.T) {
	got := NormalizePorts([]MyType.PortInfo{
		{ContainerPort: 9000},                                  // 未映射，协议留空
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},   // 已映射
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},   // 完全重复，应被丢掉
		{HostPort: 6443, ContainerPort: 6443, Protocol: "tcp"}, // 已映射
		{HostPort: 1234, ContainerPort: 0, Protocol: "tcp"},    // 容器端口非法，应被丢掉
		{HostPort: 0, ContainerPort: 3000},                     // 未映射
		{HostPort: 53, ContainerPort: 53, Protocol: "udp"},     // 同端口的 udp
		{HostPort: 53, ContainerPort: 53, Protocol: "tcp"},     // 同端口的 tcp
	})
	want := []MyType.PortInfo{
		{HostPort: 53, ContainerPort: 53, Protocol: "tcp"},
		{HostPort: 53, ContainerPort: 53, Protocol: "udp"},
		{HostPort: 6443, ContainerPort: 6443, Protocol: "tcp"},
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 0, ContainerPort: 3000, Protocol: "tcp"},
		{HostPort: 0, ContainerPort: 9000, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("端口整理结果不符合预期\n got: %+v\nwant: %+v", got, want)
	}
}

func TestNormalizePortsEmpty(t *testing.T) {
	if got := NormalizePorts(nil); len(got) != 0 {
		t.Fatalf("空输入应返回空列表，实际得到 %+v", got)
	}
}

// 同一个映射在 IPv4 / IPv6 上会各有一条记录，展示时要合成一条。
func TestBuildPortListMergesDuplicateBindings(t *testing.T) {
	c := MyType.Container{Container: dockertypes.Container{
		Ports: []dockertypes.Port{
			{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{PrivatePort: 9000, Type: "tcp"},
		},
	}}
	got := BuildPortList(c)
	want := []MyType.PortInfo{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0"},
		{HostPort: 0, ContainerPort: 9000, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("端口整理结果不符合预期\n got: %+v\nwant: %+v", got, want)
	}
}

func TestIsHostNetwork(t *testing.T) {
	host := MyType.Container{Container: dockertypes.Container{}}
	host.HostConfig.NetworkMode = "host"
	if !IsHostNetwork(host) {
		t.Fatal("host 网络模式应被识别")
	}

	bridge := MyType.Container{Container: dockertypes.Container{}}
	bridge.HostConfig.NetworkMode = "bridge"
	if IsHostNetwork(bridge) {
		t.Fatal("bridge 网络模式不应被识别为 host")
	}
}

// host 网络容器的端口来自镜像 EXPOSE，且容器端口就是宿主机端口（Direct 为 true）。
func TestExposedSpecsToPortListHostNetwork(t *testing.T) {
	got := ExposedSpecsToPortList([]string{"21115/tcp", "21116/tcp", "21116/udp", "21118/tcp"}, true)
	want := []MyType.PortInfo{
		{HostPort: 21115, ContainerPort: 21115, Protocol: "tcp", Direct: true},
		{HostPort: 21116, ContainerPort: 21116, Protocol: "tcp", Direct: true},
		{HostPort: 21116, ContainerPort: 21116, Protocol: "udp", Direct: true},
		{HostPort: 21118, ContainerPort: 21118, Protocol: "tcp", Direct: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host 网络 EXPOSE 解析结果不符合预期\n got: %+v\nwant: %+v", got, want)
	}
}

// 普通容器没做 -p 时：端口仅在容器内，HostPort 保持 0，Direct 为 false。
func TestExposedSpecsToPortListBridge(t *testing.T) {
	got := ExposedSpecsToPortList([]string{"3000/tcp"}, false)
	want := []MyType.PortInfo{{ContainerPort: 3000, Protocol: "tcp"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("未映射端口解析结果不符合预期\n got: %+v\nwant: %+v", got, want)
	}
}

// 没有 "/" 的 spec 按 tcp 处理；非法端口号直接丢掉，不能因此崩掉整张列表。
func TestExposedSpecsToPortListMalformed(t *testing.T) {
	got := ExposedSpecsToPortList([]string{"8080", "abc/tcp", "0/tcp", "/tcp", "70000/tcp"}, false)
	want := []MyType.PortInfo{{ContainerPort: 8080, Protocol: "tcp"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("异常 spec 应被丢弃且默认 tcp\n got: %+v\nwant: %+v", got, want)
	}
}

func TestExposedSpecsToPortListEmpty(t *testing.T) {
	if got := ExposedSpecsToPortList(nil, true); len(got) != 0 {
		t.Fatalf("空输入应返回空列表，实际得到 %+v", got)
	}
}
