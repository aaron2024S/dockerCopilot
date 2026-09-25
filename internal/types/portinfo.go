package types

// PortInfo 是容器端口的对外展示结构。
//
// HostPort 为 0 表示这个端口只在容器内部暴露，没有映射到宿主机；
// 前端据此把「已映射」和「仅暴露」区分成两种样式。
type PortInfo struct {
	// HostPort 宿主机上对外提供的端口，0 表示未映射
	HostPort uint16 `json:"hostPort"`
	// ContainerPort 容器内部的端口
	ContainerPort uint16 `json:"containerPort"`
	// Protocol tcp / udp
	Protocol string `json:"protocol"`
	// HostIP 绑定的宿主机地址，未映射或绑全部地址时可能为空
	HostIP string `json:"hostIp,omitempty"`
	// Direct 表示 host 网络模式：没有 NAT 映射这回事，容器端口直接就是宿主机端口。
	// 此时 HostPort 与 ContainerPort 相等，前端只显示一个数字而不是「宿主机:容器」。
	Direct bool `json:"direct,omitempty"`
}
