package storage

import "context"

type IdentityLayerReq struct {
	Background         bool
	MAC                string
	ConfigID           string
	SnapshotName       string
	TemplateInstanceID string
	InstanceID         string
	ClassKey           string
	// Template 非空时不复制真实适配留下的实例，而是把这份模板写进身份层（机型没有适配记录时用）。
	Template *NICTemplate `json:"template,omitempty"`
}

// NICTemplate 是按这台机器填好的网卡模板：Reg 带占位符，GUID 与 MAC 是这台机器自己的。
// Generate 为真且 Reg 为空时，不使用事先写好的注册表，而是挂上镜像后按已装网卡和驱动文件现写。
type NICTemplate struct {
	Reg         string `json:"reg"`
	GUID        string `json:"guid"`
	Generate    bool   `json:"generate,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	Device      string `json:"device,omitempty"`
	DriverZip   string `json:"driver_zip,omitempty"`
	DriverStamp string `json:"driver_stamp,omitempty"`
}
type IdentityHostAgent interface {
	CreateIdentityLayer(context.Context, IdentityLayerReq) error
	DeleteIdentityLayer(context.Context, string) error
}

// IdentityLayerReleaser releases idle identity caches only while their current origin matches these sources.
// A relative config or config@snapshot is resolved under this storage node's catalogue.
// A matching but busy layer is an error; absent layers and origins changed since discovery are ignored.
type IdentityLayerReleaser interface {
	ReleaseIdentityLayers(context.Context, []string) ([]string, error)
}

// AsideHostAgent 从重建留下的旧副本里回收操作者明确删除的镜像和配置；分叉副本只由操作者确认后删。
type AsideHostAgent interface {
	DeleteAsideImage(ctx context.Context, imageID string, configIDs []string) error
}
