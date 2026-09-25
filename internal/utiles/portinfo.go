package utiles

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
)

// BuildPortList 把 Docker 返回的原始端口记录整理成可直接渲染的列表。
//
// ContainerList 对同一个容器端口可能给出多条记录（绑不同 IP、tcp 与 udp 各一条，
// 或者不同 daemon 版本的差异），这里统一去重并排序，保证同一个容器每次展示顺序稳定。
func BuildPortList(c MyType.Container) []MyType.PortInfo {
	ports := make([]MyType.PortInfo, 0, len(c.Ports))
	for _, p := range c.Ports {
		if p.PrivatePort == 0 {
			continue
		}
		ports = append(ports, MyType.PortInfo{
			HostPort:      p.PublicPort,
			ContainerPort: p.PrivatePort,
			Protocol:      p.Type,
			HostIP:        p.IP,
		})
	}
	return NormalizePorts(ports)
}

// NormalizePorts 对端口列表去重并排序。
//
// 排序规则：已映射到宿主机的排在前面（按宿主机端口升序，用户最关心这些），
// 只在容器内暴露的排在后面（按容器端口升序），协议仅作最后的稳定兜底。
func NormalizePorts(ports []MyType.PortInfo) []MyType.PortInfo {
	seen := make(map[string]struct{}, len(ports))
	result := make([]MyType.PortInfo, 0, len(ports))
	for _, p := range ports {
		if p.ContainerPort == 0 {
			continue
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		key := fmt.Sprintf("%d/%d/%s", p.HostPort, p.ContainerPort, protocol)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		p.Protocol = protocol
		result = append(result, p)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		aMapped := a.HostPort > 0
		bMapped := b.HostPort > 0
		if aMapped != bMapped {
			return aMapped
		}
		if aMapped && a.HostPort != b.HostPort {
			return a.HostPort < b.HostPort
		}
		if a.ContainerPort != b.ContainerPort {
			return a.ContainerPort < b.ContainerPort
		}
		return a.Protocol < b.Protocol
	})
	return result
}

// IsHostNetwork 判断容器是否直接用宿主机网络。
// 这种容器不会有端口映射记录，端口就是宿主机端口，展示时要单独说明。
func IsHostNetwork(c MyType.Container) bool {
	return string(c.HostConfig.NetworkMode) == "host"
}

// ExposedSpecsToPortList 把容器配置里 EXPOSE 的端口转成展示结构。
//
// 什么时候需要它：端口映射记录为空，但容器其实是有端口的。
// 两种情况会这样 ——
//  1. host 网络模式：端口不经过 NAT，所以 ContainerList 的 Ports 是空的；
//  2. 普通容器但没做 -p：端口只在容器内部可见。
//
// 两种情况下容器配置里的 ExposedPorts（来自镜像 EXPOSE 或 docker run --expose）
// 仍然有内容，可以拿来告诉用户「这容器声明了哪些端口」。
//
// specs 形如 ["8080/tcp", "21116/udp"]；direct=true 表示 host 网络，
// 端口直接就是宿主机端口，HostPort 记为与 ContainerPort 相同。
//
// 注意 EXPOSE 只是**声明**，不代表程序真的在监听 —— 前端提示语要留这个余地。
func ExposedSpecsToPortList(specs []string, direct bool) []MyType.PortInfo {
	ports := make([]MyType.PortInfo, 0, len(specs))
	for _, spec := range specs {
		numStr, protocol := spec, "tcp"
		if i := strings.IndexByte(spec, '/'); i >= 0 {
			numStr, protocol = spec[:i], spec[i+1:]
		}
		n, err := strconv.ParseUint(numStr, 10, 16)
		if err != nil || n == 0 {
			continue
		}
		info := MyType.PortInfo{
			ContainerPort: uint16(n),
			Protocol:      protocol,
			Direct:        direct,
		}
		if direct {
			// host 网络下没有端口转换，容器端口即宿主机端口
			info.HostPort = uint16(n)
		}
		ports = append(ports, info)
	}
	return NormalizePorts(ports)
}
