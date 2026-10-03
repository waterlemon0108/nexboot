package domain

import "time"

// 所有时间戳一律以 UTC 存储和返回。

type OSType string

const (
	OSTypeWindows OSType = "windows"
	OSTypeLinux   OSType = "linux"
)

type ImageState string

const (
	ImageStateNormal    ImageState = "normal"
	ImageStateImporting ImageState = "importing"
	ImageStateError     ImageState = "error"
)

type ReductionStatus string

const (
	ReductionStatusReady    ReductionStatus = "ready"
	ReductionStatusCreating ReductionStatus = "creating"
	ReductionStatusError    ReductionStatus = "error"
)

type TerminalState string

const (
	TerminalStateOnline  TerminalState = "online"
	TerminalStateOffline TerminalState = "offline"
	TerminalStateUnknown TerminalState = "unknown"
)

type ServerRole string

const (
	ServerRoleControl ServerRole = "control"
	ServerRoleStorage ServerRole = "storage"
	ServerRoleAll     ServerRole = "all"
)

type ServerStatus string

const (
	ServerStatusUp   ServerStatus = "up"
	ServerStatusDown ServerStatus = "down"
)

type CloneKind string

const (
	CloneKindEphemeral  CloneKind = "ephemeral"
	CloneKindPersistent CloneKind = "persistent"
)

type TaskType string

const (
	TaskTypeImportImage     TaskType = "import_image"
	TaskTypeCreateConfig    TaskType = "create_config"
	TaskTypeDeleteConfig    TaskType = "delete_config"
	TaskTypeCreateReduction TaskType = "create_reduction"
	TaskTypeDeleteReduction TaskType = "delete_reduction"
	TaskTypeMergeConfig     TaskType = "merge_config"
	TaskTypeMergeReduction  TaskType = "merge_reduction"
	TaskTypeSuperStop       TaskType = "super_stop"
	// TaskTypePublishDataDisk 是超管机不关机时保存并发布数据盘，单列一类，任务列表里能与关机保存区分。
	TaskTypePublishDataDisk TaskType = "publish_data_disk"
	// TaskTypeCopyImage 把还原点另存为独立的新镜像。
	TaskTypeCopyImage        TaskType = "copy_image"
	TaskTypeCreatePool       TaskType = "create_pool"
	TaskTypeDestroyPool      TaskType = "destroy_pool"
	TaskTypeAddDisk          TaskType = "add_disk"
	TaskTypeMigrateDisk      TaskType = "migrate_disk"
	TaskTypeReplaceDisk      TaskType = "replace_disk"
	TaskTypeAddReadCache     TaskType = "add_read_cache"
	TaskTypeRemoveReadCache  TaskType = "remove_read_cache"
	TaskTypeAddWriteCache    TaskType = "add_write_cache"
	TaskTypeRemoveWriteCache TaskType = "remove_write_cache"
	TaskTypeFlushCache       TaskType = "flush_cache"
	TaskTypeBackupDataset    TaskType = "backup_dataset"
	TaskTypeImageHealthCheck TaskType = "image_health_check"
	// TaskTypeDetachDisk 从镜像组里摘掉一路；TaskTypeMirrorUpgrade 依次给条带池的每块数据盘挂上镜像盘。
	TaskTypeDetachDisk       TaskType = "detach_disk"
	TaskTypeMirrorUpgrade    TaskType = "mirror_upgrade"
	TaskTypeCreateBlankImage TaskType = "create_blank_image"
	// TaskTypeExportImage 把镜像流式导出为导入目录下可重新导入的 .zfs 文件。
	TaskTypeExportImage   TaskType = "export_image"
	TaskTypeAddSpecial    TaskType = "add_special"
	TaskTypeRemoveSpecial TaskType = "remove_special"
	TaskTypeAddSpare      TaskType = "add_spare"
	TaskTypeRemoveSpare   TaskType = "remove_spare"
)

type TaskStatus string

const (
	TaskStatusPending TaskStatus = "pending"
	TaskStatusRunning TaskStatus = "running"
	TaskStatusSuccess TaskStatus = "success"
	TaskStatusFailed  TaskStatus = "failed"
)

type PoolDiskRole string

const (
	PoolDiskRoleData       PoolDiskRole = "data"
	PoolDiskRoleReadCache  PoolDiskRole = "read_cache"
	PoolDiskRoleWriteCache PoolDiskRole = "write_cache"
	// PoolDiskRoleSpecial 在更快的设备上放元数据（及小块）；PoolDiskRoleSpare 是热备盘。
	// 两者都会上报，让产品外建的池读回来是真实的；管理它们是后续阶段的事。
	PoolDiskRoleSpecial PoolDiskRole = "special"
	PoolDiskRoleSpare   PoolDiskRole = "spare"
)

// PoolLayout 是池数据 vdev 的构建方式，建好后除非重建无法更改；它决定「加盘」是加一块、一对镜像
// 还是一整组 raidz，以及「移除盘」是否可行。
type PoolLayout string

const (
	PoolLayoutStripe PoolLayout = "stripe" // 每块盘自成 vdev，无冗余
	PoolLayoutMirror PoolLayout = "mirror" // GroupWidth 路镜像组，组间条带
	PoolLayoutRaidz1 PoolLayout = "raidz1"
	PoolLayoutRaidz2 PoolLayout = "raidz2"
	PoolLayoutRaidz3 PoolLayout = "raidz3"
)

// RaidzParity 返回 raidz 布局每组的校验盘数，非 raidz 为 0。
func (l PoolLayout) RaidzParity() int {
	switch l {
	case PoolLayoutRaidz1:
		return 1
	case PoolLayoutRaidz2:
		return 2
	case PoolLayoutRaidz3:
		return 3
	}
	return 0
}

// Valid 判断 l 是否是产品会构建的布局。
func (l PoolLayout) Valid() bool {
	switch l {
	case PoolLayoutStripe, PoolLayoutMirror, PoolLayoutRaidz1, PoolLayoutRaidz2, PoolLayoutRaidz3:
		return true
	}
	return false
}

// ImagePurpose 是镜像用途：可启动的系统盘，或普通数据盘（客户机挂在系统盘旁的已格式化卷）。
type ImagePurpose string

const (
	ImagePurposeSystem ImagePurpose = "system"
	ImagePurposeData   ImagePurpose = "data"
)

// ImageOrigin 是镜像来源：从磁盘镜像或流导入，或在页面上新建空盘。仅作展示。
type ImageOrigin string

const (
	ImageOriginImported ImageOrigin = "imported"
	ImageOriginBlank    ImageOrigin = "blank"
)

type Image struct {
	ID     string
	Name   string
	OSType OSType
	Size   int64
	State  ImageState
	Remark string
	// Purpose 决定镜像出现在哪个选择器（系统盘或数据盘）、是否烘焙启动脚本并检查可启动性，以及超管机克隆怎么保存。
	Purpose ImagePurpose
	Origin  ImageOrigin
	// MountScriptVersion 是导入时烘焙进镜像的启动脚本版本，其下的配置、还原点和客户机克隆都继承它。
	// 为空表示没烘焙，由启动路径逐台注入。
	MountScriptVersion string
	CreatedAt          time.Time
}

type Config struct {
	ID                 string
	ImageID            string
	Name               string
	DefaultReductionID *string
	CreatedAt          time.Time
}

type Reduction struct {
	ID       string
	ConfigID string
	// Name 是这个还原点对应的 ZFS 快照名，带前导 "@"，只能是 ASCII（ZFS 不收别的）。
	// DisplayName 是运维输入、各界面显示的名字；名字折叠后不同时二者不一样（"装完office" -> "@_office"）。
	Name        string
	DisplayName string
	CreatedAt   time.Time
	Status      ReductionStatus
	Remark      string
}

type Group struct {
	ID             string
	Name           string
	IsDefault      bool
	StartIP        string
	ClientMax      int
	Gateway        string
	Netmask        string
	DNS1           string
	DNS2           string
	SystemImageID  string
	SystemConfigID string
	// SystemReductionID 是配置上次移动指针时应用的还原点，即分组里普通机器启动用的那个。
	// 分组绑定镜像+配置，还原点在配置上决定（「应用」）并推到用该配置的所有分组；存在分组上，引用守卫和 DHCP 无需联表。
	SystemReductionID string
	// StorageServerID 把本分组的客户机钉到某个存储节点；nil 表示主节点自己。
	StorageServerID *string
}

type GroupDisk struct {
	ID          string
	GroupID     string
	MountTarget string
	ImageID     string
	ConfigID    string
}

type Terminal struct {
	ID              string
	Name            string
	MAC             string
	IP              string
	GroupID         string
	IsSuper         bool
	State           TerminalState
	OnlineSince     *time.Time
	OfflineAt       *time.Time
	LastHeartbeatAt *time.Time
	// PendingBundleID/At 记录 InjectDriver 放进本超管机克隆、但还没固化成还原点的驱动包。
	// 任何退出超管的路径（DisableSuper 或 StopSuper）都会清掉，见 adaptation_inject.go。
	PendingBundleID *string
	PendingBundleAt *time.Time
	// StorageServerID 记录上次是哪个节点服务了这台客户机的启动，清理时据此找到克隆。nil 表示本节点。
	StorageServerID *string
}

type HealthLevel string

const (
	HealthOK      HealthLevel = "ok"
	HealthWarn    HealthLevel = "warn"
	HealthBlock   HealthLevel = "block"
	HealthUnknown HealthLevel = "unknown"
)

// HealthSeverity 给级别排序用于汇总：block > warn > unknown > ok。
func HealthSeverity(l HealthLevel) int {
	switch l {
	case HealthBlock:
		return 3
	case HealthWarn:
		return 2
	case HealthUnknown:
		return 1
	default:
		return 0
	}
}

type HealthCheckItem struct {
	Name   string      `json:"name"`
	Level  HealthLevel `json:"level"`
	Detail string      `json:"detail"`
}

type ImageHealthReport struct {
	ID             string
	ImageID        string
	Level          HealthLevel
	PartitionStyle string
	// BootModes 是固件能以哪些方式启动镜像（BootModeBIOS、BootModeUEFI）。为空表示检查判断不了，不据此做任何判断。
	BootModes []string
	Items     []HealthCheckItem
	// NICPCIIDs 是镜像带有网卡驱动的 PCI vendor:device 对。
	NICPCIIDs []string
	CreatedAt time.Time
}

const (
	BootModeBIOS = "bios"
	BootModeUEFI = "uefi"
)

// 启动失败的阶段。sanhook/sanboot 来自客户机的 iPXE 脚本；boot_mode 由服务端在下发脚本前判定。
const (
	BootStageSanhook  = "sanhook"
	BootStageSanboot  = "sanboot"
	BootStageBootMode = "boot_mode"
)

// BootFailure 是一台机器最近一次失败的启动。
type BootFailure struct {
	MAC      string
	Stage    string
	Code     string // iPXE 上报的 errno
	Platform string // iPXE ${platform}: efi | pcbios
	ImageID  string
	At       time.Time
}

type Server struct {
	ID   string // 稳定的节点身份（由 machine-id 派生），不是地址
	Name string
	IP   string // 本机的管理地址
	// PortalIP 是客户机访问本节点磁盘和启动端点的地址：HA 下是 VIP，放置时是本节点的存储地址。
	PortalIP string
	// APIURL 是对端节点调用本节点 API 的地址。
	APIURL    string
	Role      ServerRole
	Status    ServerStatus
	Heartbeat *time.Time
	// Epoch 和 HAState 随 HA 引入：防双写计数器，以及节点上次所处的角色。
	Epoch   int64
	HAState string
	// LastSeenAt 由节点自己的心跳循环刷新，过期即可判定节点已死。
	LastSeenAt *time.Time
}

type Pool struct {
	ID              string
	ServerID        string
	Name            string
	Disks           []string
	ReadCacheDisks  []string
	WriteCacheDisks []string
	// Layout 和 GroupWidth 描述数据 vdev：每组镜像路数、每组 raidz 盘数，条带为 1。
	// 建池后从池本身读取，产品外建的池也能如实读回；库里只是缓存。
	Layout     PoolLayout
	GroupWidth int
	Capacity   int64
	Used       int64
}

type PoolStatus struct {
	PoolID    string
	Name      string
	Health    string
	Operation string
	Progress  string
	Capacity  int64
	Used      int64
	// Layout 和 GroupWidth 读自 vdev 树，反映池的真实情况，而不是建池时请求的。
	Layout     PoolLayout
	GroupWidth int
	// Vdevs 是按 zpool status 顺序排列的顶层组，含 cache、log、special、spare；Disks 是其成员的扁平列表，早先的调用方都读它。
	Vdevs []PoolVdev
	Disks []PoolDiskStatus
	// RaidzExpandable 表示本节点能否给池的 raidz 组扩一块盘（需 OpenZFS ≥ 2.3 用户态及池特性）；
	// 不能时 RaidzExpandNote 说明缺哪一半。只对 raidz 池询问。
	RaidzExpandable bool
	RaidzExpandNote string
}

// PoolVdev 是池里的一个顶层组：镜像或 raidz 组，或单独成 vdev 的裸盘（Kind "disk"），
// Role 是它出现在 zpool status 的哪一节。
type PoolVdev struct {
	Name   string // "mirror-0"、"raidz2-0"，裸盘为盘路径
	Kind   string // "mirror"、"raidz1".."raidz3"、"disk"，或 zpool 给的其他名字（"indirect"、"draid1" 等）
	Role   PoolDiskRole
	Status string
	Disks  []PoolDiskStatus
}

type PoolDiskStatus struct {
	Path   string
	Vdev   string // 盘所在的组；裸盘为自身路径，热备盘为 ""
	Role   PoolDiskRole
	Status string
}

type PoolDisk struct {
	ID     string
	PoolID string
	Path   string
	Role   PoolDiskRole
	// Vdev 是盘所在组在 zpool status 里的名字（"mirror-0"、"raidz2-0"）；条带成员自成一组，Vdev 即盘路径。热备盘为空。
	Vdev     string
	Capacity int64
	Used     int64
}

type ClientClone struct {
	ID          string
	TerminalMAC string
	Kind        CloneKind
	ConfigID    string
	ReductionID string
	ServerID    string
	Target      string
	LUN         int
	VolPath     string
}

type Task struct {
	ID         string
	Type       TaskType
	TargetRef  string
	Status     TaskStatus
	Progress   int
	Message    string
	Result     string
	Error      string
	CreatedAt  time.Time
	FinishedAt *time.Time
	// Node 是执行任务的节点地址，创建时记录；记录此字段之前的旧任务为空。
	Node string `json:"Node,omitempty"`
	// TargetName 是运维认识的 TargetRef 名称，列表时填充，从不落库。
	TargetName string `json:"TargetName,omitempty"`
}

// RunsOnPoolNode 判断任务类型是否由池所在节点执行。池写入转发到那台并记在它的库里，其他地方只能问它才找得到。
func (t TaskType) RunsOnPoolNode() bool {
	switch t {
	case TaskTypeCreatePool, TaskTypeDestroyPool, TaskTypeAddDisk, TaskTypeMigrateDisk, TaskTypeReplaceDisk,
		TaskTypeAddReadCache, TaskTypeRemoveReadCache, TaskTypeAddWriteCache, TaskTypeRemoveWriteCache,
		TaskTypeFlushCache, TaskTypeDetachDisk, TaskTypeMirrorUpgrade,
		TaskTypeAddSpecial, TaskTypeRemoveSpecial, TaskTypeAddSpare, TaskTypeRemoveSpare:
		return true
	}
	return false
}

type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

type DriverPackCategory string

const (
	DriverCategoryBootCriticalNIC DriverPackCategory = "boot_critical_nic"
	DriverCategoryNIC             DriverPackCategory = "nic"
	DriverCategoryGPU             DriverPackCategory = "gpu"
	DriverCategoryAudio           DriverPackCategory = "audio"
	DriverCategoryChipset         DriverPackCategory = "chipset"
	DriverCategoryOther           DriverPackCategory = "other"
)

type DriverPackStatus string

const (
	DriverPackEnabled  DriverPackStatus = "enabled"
	DriverPackDisabled DriverPackStatus = "disabled"
)

type DriverPack struct {
	ID          string
	Name        string
	Category    DriverPackCategory
	OSType      OSType
	Arch        string
	Version     string
	ReleaseDate string
	Vendor      string
	Signed      bool
	HWIDs       []string
	Status      DriverPackStatus
	Recommended bool
	StoragePath string
	CreatedAt   time.Time
}

type DriverBundle struct {
	ID        string
	Name      string
	OSType    OSType
	CreatedAt time.Time
}

type DriverBundlePack struct {
	ID       string
	BundleID string
	PackID   string
}

const BackupConfigDefaultID = "default"

type BackupConfig struct {
	ID         string
	BackupPool string
	Enabled    bool
	Schedule   string
	LastRunAt  *time.Time
}

const SystemSettingsDefaultID = "default"

type SystemSettings struct {
	ID        string
	ImportDir string
	// ClientIface 是 dnsmasq 服务客户机的网卡，为空表示自动检测。
	// AllowCrossSubnet 允许分组不在该网卡网段内，适用于交换机把 DHCP 中继到服务器的现场。
	ClientIface      string
	AllowCrossSubnet bool
	// ReplicationRateMBPS 限制批量复制流的速率。三种状态：nil 未设置（用部署默认值），0 明确不限速
	// （无客户机的维护窗口正是解除限速的时候），>0 为 MB/s 上限。
	ReplicationRateMBPS *int
	// DataPool 是本节点存放镜像/克隆的 ZFS 池。建池前为空：节点可以无池安装，再从界面建池命名并记在这里。
	DataPool string
}

// ReplicationState 是一个复制目标的记账行：最后落地的快照、时间和上次的错误。运维和滞后告警读它；
// 真正的增量基准从池里读。
type ReplicationState struct {
	Target       string // 对端 URL
	Kind         string // "standby-pull"
	Root         string
	LastSnapshot string
	LastOKAt     *time.Time
	LastError    string
	UpdatedAt    time.Time
}

type BackupState struct {
	ID           string
	SourceName   string
	LastSnapshot string
	UpdatedAt    time.Time
}

// Alarm 是由告警 sweeper 对账的有状态健康告警。未关闭（active/acknowledged）时按 AlarmKey 去重，条件消失后自动恢复。
type Alarm struct {
	ID          string
	AlarmKey    string
	Severity    string // info | warn | error
	Type        string
	Source      string // pool | service | backup | image
	Resource    string
	Threshold   string
	Value       string
	Status      string // active | acknowledged | recovered
	Message     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	RecoveredAt *time.Time
}

// AuditLog 是一条操作或登录审计。操作记录由审计中间件自动采集，登录记录（含失败）由登录 handler 写入。
type AuditLog struct {
	ID         string
	Type       string // operation | login
	Username   string
	Action     string
	Module     string
	Detail     string
	IP         string
	UserAgent  string
	Status     string // ok | err
	HTTPStatus int
	CostMs     int64
	CreatedAt  time.Time
}
