package storage

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// 领域实体在池里的命名规则及其反向识别（由名字判断是什么）都只在这里定义，别处不要另写前缀常量。

// BaselineReduction 是每个镜像和配置都带有的起点快照。
const BaselineReduction = "0"

// ReplicationSnapshotPrefix 标记复制轮次冻结目录用的递归快照。它们只用于记账，列还原点时不得展示。
const ReplicationSnapshotPrefix = "rep-"

// BackupSnapshotPrefix 标记本地备份打的快照，同样只用于记账。
const BackupSnapshotPrefix = "ndbackup-"

// ExportSnapshotPrefix 标记镜像导出期间冻结镜像的临时快照，zfs send 结束即销毁。
// 导出流里也带着它，导入时在建基线前去掉。
const ExportSnapshotPrefix = "ndexport-"

// 布局 v2 把池分成两个容器：目录（镜像、配置、还原点、数据库副本）在 CatalogueRoot 下，
// 以一条 `zfs send -R` 流在节点间移动；客户机克隆（CLIENT-、SCLIENT-）在 RunRoot 下，从不离开本节点。
// 这样复制才能整体冻结并发送目录，而不捎带运行中客户机的写入。
const (
	CatalogueRoot = "nd"
	RunRoot       = "run"
	// LayoutProperty 是池根上记录布局版本的 ZFS 用户属性，容器建好后值为 LayoutV2。
	LayoutProperty = "ndiskless:layout"
	LayoutV2       = "2"
)

// ContainerFor 返回池内相对名所属的容器。
func ContainerFor(name string) string {
	switch Classify(name).Kind {
	case CarrierClient, CarrierSuper, CarrierInspect, CarrierExport:
		return RunRoot
	}
	return CatalogueRoot
}

// SystemSnapshot 判断快照名是否为产品记账标记（复制、备份、导出）而非还原点。
func SystemSnapshot(name string) bool {
	return strings.HasPrefix(name, ReplicationSnapshotPrefix) ||
		strings.HasPrefix(name, BackupSnapshotPrefix) ||
		strings.HasPrefix(name, ExportSnapshotPrefix)
}

// ReservedCatalogueChild 判断 CatalogueRoot 下的名字是否属于容器自身（数据库副本、驱动文件库），
// 这些永远不会被当作镜像或配置收编。
func ReservedCatalogueChild(name string) bool {
	return name == "db" || name == "files"
}

const (
	clientClonePrefix = "CLIENT-"
	superClonePrefix  = "SCLIENT-"
	inspectPrefix     = "INSPECT-"
	exportPrefix      = "EXPORT-"
	adaptCheckPrefix  = "NDADAPTCHK-"
	dataCloneInfix    = "-DATA-"
	defaultConfigSfx  = "_default"
	supersededSfx     = "_before_super"
	// 显示名折叠后什么都不剩时使用，"____" 这样的数据集名在 `zfs list` 里毫无信息。
	fallbackImageID      = "image"
	fallbackSnapshotName = "point"
)

// DefaultConfigName 是导入时随镜像创建的默认配置名。
func DefaultConfigName(imageID string) string { return imageID + defaultConfigSfx }

// NewConfigName 为新建或派生的配置命名。显示名不唯一且可能折叠成同一 ascii，靠时间戳保证唯一。
func NewConfigName(sourceID, displayName string) string {
	return fmt.Sprintf("%s_%s_%d", sourceID, sanitizeForID(displayName), time.Now().UTC().UnixNano())
}

// SupersededConfigName 是超管保存把克隆换上位期间旧配置的暂存名。
// 用固定名而非时间戳，中断后的重试才能认出上次留下的东西。
func SupersededConfigName(configID string) string { return configID + supersededSfx }

// ImageName 由显示名生成镜像数据集名（显示名原样存库，只有这里须为 ascii）。不同名字可能折叠成
// 同一串（"教学一班" 和 "教学二班" 都是 "____"），taken 用于避免第二次导入落到第一次的数据集上。
func ImageName(displayName string, taken func(id string) bool) string {
	id := collapsePadding(sanitizeForID(displayName))
	if strings.Trim(id, "_-") == "" {
		id = fallbackImageID
	}
	if taken == nil || (!taken(id) && !taken(DefaultConfigName(id))) {
		return id
	}
	return fmt.Sprintf("%s-%d", id, time.Now().UTC().UnixNano())
}

// SnapshotName 由显示名生成还原点的 ZFS 快照名（显示名原样存库）。taken 判断同配置下是否已有该快照，
// 防止两个折叠成同一串的名字被静默当成同一个。
func SnapshotName(displayName string, taken func(name string) bool) string {
	// 去掉两端填充，否则 "2026Q3-驱动更新" 会变成看着像坏掉的 "2026q3-"。
	name := strings.Trim(collapsePadding(sanitizeForSnapshot(displayName)), "_-")
	if name == "" {
		name = fallbackSnapshotName
	}
	if taken == nil || !taken(name) {
		return name
	}
	// 用数字后缀而非时间戳：快照名是排障时在 `zfs list` 里给人看的。
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", name, n)
		if !taken(candidate) {
			return candidate
		}
	}
}

// ReductionID 是配置还原点在库里的行 ID。
func ReductionID(configID, snapshot string) string {
	return configID + "_" + sanitizeForID(snapshot)
}

func ClientCloneName(mac string) string { return clientClonePrefix + NormalizeMAC(mac) }

func ClientDataCloneName(mac string, lun int) string {
	return fmt.Sprintf("%s%s%d", ClientCloneName(mac), dataCloneInfix, lun)
}

func SuperClientCloneName(mac string) string { return superClonePrefix + NormalizeMAC(mac) }

func SuperClientDataCloneName(mac string, lun int) string {
	return fmt.Sprintf("%s%s%d", SuperClientCloneName(mac), dataCloneInfix, lun)
}

// PoolID 是存储池在库里的行 ID，与池内命名共用同一折叠规则。
// 必须带节点：数据库整体复制，各节点数据池通常都叫 "tank"，只按名字会把集群的多个池并成一行，
// 最后写入者独占，其余节点显示没有存储。
func PoolID(node, name string) string {
	if node == "" {
		return "pool-" + sanitizeForID(name)
	}
	return "pool-" + sanitizeForID(node) + "--" + sanitizeForID(name)
}

// InspectCloneName 是镜像健康检查挂载的临时克隆。只存在几秒，但足以挡住合并，
// 所以反向识别要认出它并提示「稍候」而不是「删掉它」。
func InspectCloneName(imageID string) string {
	return fmt.Sprintf("%s%s-%d", inspectPrefix, strings.ToUpper(sanitizeForID(imageID)), time.Now().UTC().UnixNano())
}

// ExportCloneName 是导出或复制还原点所用的临时克隆，使流里只带接收端会去掉的快照。
func ExportCloneName(configID string) string {
	return fmt.Sprintf("%s%s-%d", exportPrefix, strings.ToUpper(sanitizeForID(configID)), time.Now().UTC().UnixNano())
}

// AdaptCheckCloneName 是读超管机适配结果用的临时克隆（克隆自 AdaptCheckSnapshot）。
// 归为体检类：同样只活几秒、只在本节点，阻塞提示也同样是「稍候」。
func AdaptCheckCloneName(mac string) string { return adaptCheckPrefix + NormalizeMAC(mac) }

// AdaptCheckSnapshot 是读适配结果时打在超管机系统盘上的临时快照名。
const AdaptCheckSnapshot = "ndadaptcheck"

// CarrierKind 是池内名字所代表的对象类别。
type CarrierKind string

const (
	CarrierEntity     CarrierKind = "entity"     // 镜像或配置
	CarrierClient     CarrierKind = "client"     // 临时客户机克隆
	CarrierSuper      CarrierKind = "super"      // 超管机持久克隆
	CarrierInspect    CarrierKind = "inspect"    // 进行中的健康检查
	CarrierExport     CarrierKind = "export"     // 正在导出或复制的还原点
	CarrierSuperseded CarrierKind = "superseded" // 被超管保存替下的配置
)

// Carrier 是池内名字反解出的领域对象。
type Carrier struct {
	Kind CarrierKind
	Name string // 原样的池内相对名
	MAC  string // 规范化 MAC，仅客户机、超管克隆和适配回读克隆
	LUN  int    // 仅数据盘克隆；系统盘为 0
}

// Classify 把池内相对名反解为它承载的对象。
func Classify(name string) Carrier {
	switch {
	case strings.HasPrefix(name, inspectPrefix):
		return Carrier{Kind: CarrierInspect, Name: name}
	case strings.HasPrefix(name, adaptCheckPrefix):
		return Carrier{Kind: CarrierInspect, Name: name, MAC: strings.TrimPrefix(name, adaptCheckPrefix)}
	case strings.HasPrefix(name, exportPrefix):
		return Carrier{Kind: CarrierExport, Name: name}
	case strings.HasPrefix(name, superClonePrefix):
		mac, lun := cloneMACAndLUN(strings.TrimPrefix(name, superClonePrefix))
		return Carrier{Kind: CarrierSuper, Name: name, MAC: mac, LUN: lun}
	case strings.HasPrefix(name, clientClonePrefix):
		mac, lun := cloneMACAndLUN(strings.TrimPrefix(name, clientClonePrefix))
		return Carrier{Kind: CarrierClient, Name: name, MAC: mac, LUN: lun}
	case strings.HasSuffix(name, supersededSfx):
		return Carrier{Kind: CarrierSuperseded, Name: name}
	}
	return Carrier{Kind: CarrierEntity, Name: name}
}

// cloneMACAndLUN 把 "AABBCCDDEEFF-DATA-1" 拆成机器和磁盘。
func cloneMACAndLUN(rest string) (mac string, lun int) {
	if idx := strings.Index(rest, dataCloneInfix); idx >= 0 {
		fmt.Sscanf(rest[idx+len(dataCloneInfix):], "%d", &lun)
		return rest[:idx], lun
	}
	return rest, 0
}

// DatasetID 取池路径的末段："tank/img_default" -> "img_default"。
func DatasetID(dataset string) string {
	if idx := strings.LastIndex(dataset, "/"); idx >= 0 {
		return dataset[idx+1:]
	}
	return dataset
}

// sanitizeForID 把显示名转成可用于数据集名的形式：ZFS 只接受 ascii，其余字符变成 "_"。
func sanitizeForID(name string) string { return fold(name, isDatasetSafe) }

// sanitizeForSnapshot 按 ZFS 快照字符集折叠，它额外允许点和冒号，"2026.07.28" 得以原样保留。
func sanitizeForSnapshot(name string) string { return fold(name, isSnapshotSafe) }

func isDatasetSafe(r rune) bool {
	return r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func isSnapshotSafe(r rune) bool { return isDatasetSafe(r) || r == '.' || r == ':' }

func fold(name string, safe func(rune) bool) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(strings.ToLower(name)) {
		switch {
		case safe(r):
			out.WriteRune(r)
		case unicode.IsSpace(r):
			out.WriteRune('-')
		default:
			out.WriteRune('_')
		}
	}
	if out.Len() == 0 {
		return "config"
	}
	return out.String()
}

// collapsePadding 把连续的替换字符压成一个，以非 ascii 为主的名字仍像个名字。
func collapsePadding(id string) string {
	var out strings.Builder
	for _, r := range id {
		if (r == '_' || r == '-') && out.Len() > 0 {
			last := out.String()[out.Len()-1]
			if last == '_' || last == '-' {
				continue
			}
		}
		out.WriteRune(r)
	}
	return out.String()
}
