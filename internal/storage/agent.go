package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

var ErrNotImplemented = errors.New("storage method not implemented")

// ErrSnapshotMissing 表示还原点记录还在但快照已不存在（例如保存落在非写入者上，被复制回滚）。
// 调用方要把它和「池读不了」区分开：没有快照的还原点即使是当前点也必须能删，否则整组开不了机。
var ErrSnapshotMissing = errors.New("restore point snapshot is missing")

// SuperSessionMissing 表示要保存的盘在池里找不到任何超管会话痕迹（持久克隆、半途交换、旧快照都没有）。
// LUN 0 是系统盘；调用方负责转成针对操作者所勾选磁盘的提示。
type SuperSessionMissing struct {
	LUN int
}

func (e SuperSessionMissing) Error() string {
	return fmt.Sprintf("no super session to save for lun %d", e.LUN)
}

// PreservedCopy 是节点挪到一边而非销毁的目录副本：-diverged- 含现行目录没有的内容，
// -rebuilding- 是重建留下的旧副本。何时删除只能由操作者决定。
type PreservedCopy struct {
	Dataset string    `json:"dataset"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created"`
	Used    int64     `json:"used"`
	// Points 统计其中的还原点和镜像快照；rep-、ndbackup-、ndexport- 标记不计。
	Points int `json:"points"`
}

const (
	PreservedDiverged   = "diverged"
	PreservedRebuilding = "rebuilding"
)

// PublishDataDiskReq 请求在超管机运行中把某块数据盘发布为还原点；LUN 0（系统盘）不可发布。
type PublishDataDiskReq struct {
	MAC           string
	LUN           int
	ConfigID      string
	ReductionName string
	MountTarget   string
}

// PublishDataDiskResult 是新还原点和重新挂回客户机的磁盘。
type PublishDataDiskResult struct {
	Reduction domain.Reduction
	LUN       LUN
}

// DiskNotReattached 表示发布已成功但磁盘没能挂回；客户机和系统盘不受影响，重启即恢复。
type DiskNotReattached struct {
	LUN int
	Err error
}

func (e DiskNotReattached) Error() string {
	return fmt.Sprintf("已发布，但该磁盘未能重新挂载（重启客户机后恢复）：%v", e.Err)
}

func (e DiskNotReattached) Unwrap() error { return e.Err }

// SuperBaseOutdated 表示配置上有比超管机基点更新的还原点，保存会把它们删掉：
// promote 只接管到基点为止的快照，最后一步会销毁其余部分，ZFS 不报错，新点静默消失。
type SuperBaseOutdated struct {
	LUN   int
	Base  string
	Newer []string
}

func (e SuperBaseOutdated) Error() string {
	return fmt.Sprintf("配置上存在比该超管机更新的还原点 %s，保存会将其删除，已终止；请先删除该还原点，或取消超管放弃本次改动",
		strings.Join(e.Newer, "、"))
}

type LUN struct {
	LUN         int
	Target      string
	MountTarget string
	VolPath     string
}

type LUNInfo struct {
	Server    string
	Target    string
	System    LUN
	DataDisks []LUN
}

type ClientSource struct {
	ImageID  string
	ConfigID string
	// SnapshotName 是 ZFS 快照后缀（还原点名去掉 '@'），不是还原点 ID；由调用方解析，存储层不查库。
	SnapshotName string
	MountTarget  string
	// LUN：0 为系统盘，1..n 为按分组顺序排列的数据盘。由调用方指定而非存储层按位置推导，
	// 因为同一编号还决定客户机内脚本分配的盘符，两处各自推导迟早会对不上。
	LUN int
}

type ClientReq struct {
	MAC       string
	IP        string
	GroupID   string
	Super     bool
	System    ClientSource
	DataDisks []ClientSource
	// MountScript 非空时（Windows 分组）在克隆后并入系统盘的本地组策略开机脚本。
	// 注入失败只记日志，绝不阻断开机。
	MountScript []byte
}

// ExportImageReq 请求把镜像以自包含、可重新导入的 ZFS 裸流写入 W。
type ExportImageReq struct {
	ImageID string
	// ConfigID 和 Snapshot 非空时导出该还原点，而不是镜像原始内容。
	ConfigID string
	Snapshot string
	W        io.Writer
	// OnProgress 报告已写字节和 zfs 试运行估算的总量；估算读不到时 estimated 为 0。
	OnProgress func(written, estimated int64)
}

// CopyReductionReq 把一个还原点复制成独立的新镜像。
type CopyReductionReq struct {
	ConfigID   string
	Snapshot   string
	Name       string
	OSType     domain.OSType
	Purpose    domain.ImagePurpose
	OnProgress func(percent int, message string)
	OnTarget   func(imageID string)
}

type ImportImageReq struct {
	Name       string
	SourcePath string
	ImportDir  string
	OSType     domain.OSType
	// Purpose 为空表示系统盘。
	Purpose domain.ImagePurpose
	// MountScript 非空时（Windows 导入）在首个快照前并入镜像，下游配置、还原点和客户机克隆
	// 都直接继承，不必每台客户机每次开机重新挂载写入。
	MountScript []byte
	// OnProgress 报告整个导入过程的总进度（0-100）和阶段说明。
	OnProgress func(percent int, message string)
	// OnTarget 在任何数据集创建之前告知镜像 ID。
	OnTarget func(imageID string)
}

// BlankImageReq 凭空创建数据盘镜像：SizeBytes 大小的稀疏卷，GPT 分区表加一个分区，
// 按 Filesystem 格式化（"ntfs" 为 Windows，"ext4" 为 Linux）。Label 为空时用磁盘名。
type BlankImageReq struct {
	Name       string
	SizeBytes  int64
	Filesystem string
	Label      string
	OnProgress func(percent int, message string)
	OnTarget   func(imageID string)
}

type ImportImageResult struct {
	Image     domain.Image
	Config    domain.Config
	Reduction domain.Reduction
	// MountScriptInjected 为 false 时调用方不得记录烘焙版本，开机时退回逐台注入。
	MountScriptInjected bool
}

// CreateConfigResult 是新配置和随之创建的基线还原点。配置数据集没有快照就无法开机，
// 所以两者必须由调用方在同一事务里记录。
type CreateConfigResult struct {
	Config    domain.Config
	Reduction domain.Reduction
}

// VolumeUsage 是卷的逻辑大小（volsize，客户机看到的盘）与池上实际占用（压缩后，克隆只计
// 不与源共享的部分）。内容 30G 的 200G 镜像实占 30G。
type VolumeUsage struct {
	Size int64
	Used int64
}

// SpaceUsage 是数据池自身的账：已写、可写（out of space 按它判，从不按逻辑大小之和），
// 以及按池内相对名（"img-win"、"img-win_default"，即目录 ID）索引的各卷用量。
type SpaceUsage struct {
	PoolUsed      int64
	PoolAvailable int64
	Volumes       map[string]VolumeUsage
}

// PoolInventory 是池里的全部内容，已去掉池名前缀。
type PoolInventory struct {
	Datasets  []string
	Snapshots []string
	// Origins 记录数据集克隆自哪个快照（均为池内相对名），根（镜像）没有条目。
	// 只凭池恢复目录时，靠这条 CoW 血缘区分镜像、配置和派生。
	Origins map[string]string
}

type MergeConfigReq struct {
	ImageID         string
	ConfigID        string
	DeleteConfigIDs []string
	// ReductionNames 是保留配置现有的还原点名（"@r1"）。合并要把配置 promote 进镜像，
	// 而 ZFS 拒绝 promote 与父级有同名快照的克隆，所以这些快照须先销毁，成功后调用方删对应记录。
	ReductionNames []string
	// RollbackTo 非空时先把配置回滚到该还原点再合并；此时 ReductionNames 只列到该点为止。
	RollbackTo string
}

// MergeConfigResult 是合并后在新镜像上重建的配置的基线还原点，也是该配置此后唯一的还原点。
type MergeConfigResult struct {
	Reduction domain.Reduction
}

// SuperStopDisk 是超管机关机时的一块数据盘：导出的 LUN、克隆来源的数据配置、改动要存成的还原点。
// ReductionName 为空表示丢弃改动。
type SuperStopDisk struct {
	LUN           int
	ConfigID      string
	ReductionName string
}

// SuperStopReq 说明超管会话保留什么：ReductionName 对应系统盘（空为丢弃），DataDisks 逐盘同理。
// 未保存的部分一律销毁。
type SuperStopReq struct {
	MAC           string
	ConfigID      string
	ReductionName string
	DataDisks     []SuperStopDisk
}

// SuperStopSavedDisk 是已存成还原点的数据盘。
type SuperStopSavedDisk struct {
	LUN       int
	Reduction domain.Reduction
}

// SuperStopResult 列出关机保存的内容：系统盘还原点（丢弃时为零值）和每块已保存的数据盘。
type SuperStopResult struct {
	System    domain.Reduction
	DataDisks []SuperStopSavedDisk
}

// SuperAdaptationReq 向超管机持久克隆放入一次性全自动驱动适配：驱动包 zip，以及注册为
// 本地组策略开机脚本的 AdaptScript，下次开机由 SYSTEM 运行（装驱动、登记启动驱动、自动关机）。
type SuperAdaptationReq struct {
	MAC         string
	Source      ClientSource // SCLIENT 持久盘的克隆来源
	BundleZip   []byte       // 驱动包，放到 C:\ndadapt\bundle.zip
	AdaptScript []byte       // 开机以 SYSTEM 运行的自包含 adapt.ps1
	// ResetClone 先丢弃之前未烘焙的 SCLIENT 系统盘，用于操作者覆盖失败或待处理的注入。
	ResetClone bool
	// MountScript 非空时另注册为常驻开机脚本 mount-disks.ps1：与 AdaptScript 不同，它会烘焙进
	// 还原点，每个克隆每次开机都按服务端重新分配数据盘盘符。
	MountScript []byte
}

// SuperAdaptationResult 是注入脚本写进 C:\ndadapt\done.txt 的结果，超管机自动关机后读回。
type SuperAdaptationResult struct {
	Done bool   // done.txt 存在（脚本已跑完）
	OK   bool   // 适配成功（exit 0）
	Log  string // done.txt 原文
}

// BackupReq 请求把整个目录容器备份一轮到备份池。增量基点每轮从两个池现读而不存档，
// 这样合并或超管保存不会悄悄让下一次备份失效。
type BackupReq struct {
	BackupPool string
	Snapshot   string // 本轮标记，"ndbackup-<id>"
	// Reuse 发送池上已有的标记而不新建。备机不能自建：复制用 -F 接收会删掉只在目标端的快照，
	// 本地标记撑不到下一轮。标记由写入者打、复制带到各节点；Reuse 也跳过清理，标记集归写入者管。
	Reuse bool
}

// BackupResult 说明本轮的发送方式：基于 Base 增量，或全量（Base 为空）。
// FellBack 表示增量因 promote 改写血缘而收不下，已退回全量重建。
type BackupResult struct {
	Base     string
	FellBack bool
}

// InspectImageReq 指定要做健康检查的还原点快照，由调用方解析，存储层不查库。
type InspectImageReq struct {
	ImageID  string
	ConfigID string
	// SnapshotName 是 ZFS 快照后缀（还原点名去掉 '@'）。
	SnapshotName string
	OSType       domain.OSType
}

type InspectItem struct {
	Name   string
	Level  domain.HealthLevel
	Detail string
}

type InspectReport struct {
	Level          domain.HealthLevel
	PartitionStyle string   // "mbr"、"gpt" 或 ""
	BootModes      []string // domain.BootModeBIOS / BootModeUEFI；无法判断时为 nil
	Items          []InspectItem
	// NICPCIIDs 是镜像网卡驱动覆盖的 PCI vendor:device（"8086:15B8"），用于和开机网卡比对；仅 Windows。
	NICPCIIDs []string
}

type DiskInfo struct {
	Path   string `json:"Path"`
	Name   string `json:"Name"`
	Type   string `json:"Type"`
	Size   int64  `json:"Size"`
	Model  string `json:"Model"`
	Serial string `json:"Serial"`
	InUse  bool   `json:"InUse"`
	UsedBy string `json:"UsedBy"`
}

// StorageAgent 是跨节点接缝（ADR-0003）：每个方法操作的镜像、配置、还原点或客户机将来可能在
// 别的节点上，都可能变成 RPC。池拓扑始终是节点本地的事，见 PoolAdmin。
type StorageAgent interface {
	CreateClientLUN(context.Context, ClientReq) (LUNInfo, error)
	CleanupClientClones(context.Context, string, []string) error
	// ReclaimIdleClientClones 供离线回收使用：在客户机锁内检查，decidedAt 之后开过机或持有
	// iSCSI 会话的跳过；返回是否回收。
	ReclaimIdleClientClones(ctx context.Context, mac string, decidedAt time.Time) (bool, error)
	// ActiveClientMACs 列出本节点有活动 iSCSI 会话的客户机 MAC，在线状态以真实连接为准，不靠客户机内心跳。
	ActiveClientMACs(context.Context) ([]string, error)
	// ClientCloneMACs 列出本节点仍有临时克隆的 MAC；减去 ActiveClientMACs 即离线回收的对象。
	ClientCloneMACs(context.Context) ([]string, error)

	ImportImage(context.Context, ImportImageReq) (ImportImageResult, error)
	// CreateBlankImage 创建只含一个已格式化分区的数据盘镜像，结果形状与导入相同。
	CreateBlankImage(context.Context, BlankImageReq) (ImportImageResult, error)
	// RollbackImportImage 撤销后续记账失败的导入。放在接缝上，迫使远端实现必须提供，
	// 而不是调用方悄悄退回不完整的清理。
	RollbackImportImage(context.Context, ImportImageReq, ImportImageResult) error
	// DeleteImage 删除镜像及所列配置。配置之间可能互为克隆（派生克隆父配置），销毁顺序只有池知道，所以一并交给存储层。
	DeleteImage(context.Context, string, []string) error
	CreateConfig(context.Context, string, string, *string) (CreateConfigResult, error)
	DeleteConfig(context.Context, string) error
	MergeConfig(context.Context, MergeConfigReq) (MergeConfigResult, error)
	CreateReduction(context.Context, string, string) (domain.Reduction, error)
	DeleteReduction(context.Context, string, string) error
	// MergeReduction 销毁被合并掉的快照并返回实际销毁的那些：ZFS 没有事务，中途失败时前面的已删，库必须跟着改。
	MergeReduction(context.Context, string, []string) ([]string, error)
	// ReductionDependents 列出克隆自该还原点快照的数据集（派生配置、客户机克隆）；有依赖则不能销毁。
	ReductionDependents(context.Context, string, string) ([]string, error)
	// ConfigDependents 列出克隆自配置任一快照的数据集，是销毁整个配置前的同类检查。
	ConfigDependents(context.Context, string) ([]string, error)
	// Inventory 列出节点实际持有的内容，供库与之对账；名称为池内相对名，如 "img-win_default@0"。
	Inventory(context.Context) (PoolInventory, error)
	// SpaceUsage 一次列出数据池已用/可用和各卷逻辑大小与实占，供镜像页展示和导入前检查。
	SpaceUsage(context.Context) (SpaceUsage, error)
	// DetectOSType 不挂载、只读分区表区分 Windows 和 Linux，用于从池恢复目录时库里已无系统类型记录。
	DetectOSType(ctx context.Context, datasetID string) (domain.OSType, bool)

	SuperStart(context.Context, ClientReq) (LUNInfo, error)
	// PublishDataDisk 把运行中超管机的一块数据盘存为配置的还原点。系统盘没有这条路：没法从运行中的 Windows 拿走。
	PublishDataDisk(context.Context, PublishDataDiskReq) (PublishDataDiskResult, error)
	// SuperDiskWritten 是超管机磁盘自克隆基点以来写入的量：即发布要保存的大小，采样两次可判断是否已静止。
	SuperDiskWritten(ctx context.Context, mac string, lun int) (int64, error)
	SuperStop(context.Context, SuperStopReq) (SuperStopResult, error)
	// PrepareSuperAdaptation 离线向超管机持久克隆放入驱动包和组策略开机脚本，下次开机自动适配。
	PrepareSuperAdaptation(context.Context, SuperAdaptationReq) error
	// ReadSuperAdaptationResult 读回脚本自动关机后写的结果。活动盘被 iSCSI 导出占着，
	// 所以经临时快照加克隆读取。脚本未完成时 Done 为 false。
	ReadSuperAdaptationResult(context.Context, string) (SuperAdaptationResult, error)

	// ExportImage 把镜像以 ZFS 裸流写入 req.W，产物可被 ImportImage 导回。导出期间用临时快照冻结，
	// 不创建也不留下任何操作者可见的东西。
	ExportImage(context.Context, ExportImageReq) error
	// ExportImageSize 估算 ExportImage 的输出字节数（池上 referenced），用于导出前查空间。
	ExportImageSize(context.Context, string) (int64, error)

	// InspectImage 对还原点做离线只读可启动检查（临时克隆、只读挂载、检查文件树、清理），绝不写入镜像（C-11）。
	InspectImage(context.Context, InspectImageReq) (InspectReport, error)
	// CopyReductionToImage 把还原点完整复制成带默认配置的新镜像，与源不再有关联。
	CopyReductionToImage(context.Context, CopyReductionReq) (ImportImageResult, error)
	Backup(context.Context, BackupReq) (BackupResult, error)
	// BackupPoolName 是本机备份写入的池（非数据池）；为空表示本节点尚无备份目标。
	BackupPoolName(ctx context.Context) (string, error)
	// LatestBackupMarker 是本机目录根上最新的 ndbackup-* 快照：一轮要发送的内容；在备机上即复制已送达的进度。
	LatestBackupMarker(ctx context.Context) (string, error)
	// CopiedBackupMarker 是备份池里已有的最新标记，用来区分「没有新内容」和「从未备份」。
	CopiedBackupMarker(ctx context.Context, backupPool string) (string, error)
	// SnapshotCatalogue 在目录根上打递归标记。只有写入者做，复制会带给其他节点。
	SnapshotCatalogue(ctx context.Context, name string) error
	// EnsureDBCopyDir 返回每轮备份或复制前写数据库副本的目录，首次使用时创建其数据集。
	EnsureDBCopyDir(ctx context.Context) (string, error)
}

// ClientHostAgent 是跨节点放置接缝：节点为放在它上面的客户机必须提供的 StorageAgent 子集
// （开机、回收、在线统计、超管机生命周期、读空间）。目录写操作故意不在这里：只有主节点拥有 nd/，其余都是副本。
type ClientHostAgent interface {
	CreateClientLUN(context.Context, ClientReq) (LUNInfo, error)
	CleanupClientClones(context.Context, string, []string) error
	ReclaimIdleClientClones(ctx context.Context, mac string, decidedAt time.Time) (bool, error)
	ActiveClientMACs(context.Context) ([]string, error)
	ClientCloneMACs(context.Context) ([]string, error)
	SuperStart(context.Context, ClientReq) (LUNInfo, error)
	SuperStop(context.Context, SuperStopReq) (SuperStopResult, error)
	PrepareSuperAdaptation(context.Context, SuperAdaptationReq) error
	ReadSuperAdaptationResult(context.Context, string) (SuperAdaptationResult, error)
	SpaceUsage(context.Context) (SpaceUsage, error)
}

// PoolSpec 描述建池方式：布局、每组宽度（镜像路数或每组 raidz 盘数，条带为 1）和按组排列的磁盘。
// 磁盘数必须能被 GroupWidth 整除，否则最后一组会静默变成更窄（或无镜像）的 vdev。
type PoolSpec struct {
	Layout     domain.PoolLayout
	GroupWidth int
	Disks      []string
}

// PoolAdmin 管理本机 ZFS 池拓扑：物理盘、缓存 vdev、池生命周期。磁盘只能在挂着它的机器上管，
// 所以不放在 StorageAgent 的跨节点接缝上，也不会变成远程 RPC。
type PoolAdmin interface {
	// ServerID 是本节点身份，卷和池都按它寻址。
	ServerID() string
	// DataPoolName 是本节点存放镜像、配置和克隆的池；其他池（备份、临时）都不放这些，
	// 据此区分「会销毁数据」和「只是销毁空池」。
	DataPoolName() string
	// BackupPoolName 是存放本节点备份副本的另一个池（如有）。
	BackupPoolName(context.Context) (string, error)
	ListDisks(context.Context) ([]DiskInfo, error)
	CreatePool(context.Context, string, PoolSpec) (domain.Pool, error)
	DestroyPool(context.Context, string) error
	// AddDisk 按 PoolSpec 语法添加整个 vdev（裸盘、镜像组、raidz 组）；AttachDisk 给已有数据盘
	// 加一路镜像（条带成员变镜像，n 路变 n+1 路）。
	AddDisk(context.Context, string, PoolSpec) error
	AttachDisk(ctx context.Context, name, target, disk string) error
	// RemoveDisk 迁走数据后移除顶层 vdev（裸盘或组名）；DetachDisk 从镜像组摘掉一路。
	RemoveDisk(context.Context, string, string) error
	DetachDisk(ctx context.Context, name, disk string) error
	// AddSpecial 添加镜像的 special（元数据）vdev，RemoveSpecial 按组名移除；
	// AddSpare 添加热备并打开 autoreplace，RemoveSpare 移除一块。
	AddSpecial(ctx context.Context, name string, disks []string) error
	RemoveSpecial(ctx context.Context, name, group string) error
	AddSpare(ctx context.Context, name string, disks []string) error
	RemoveSpare(ctx context.Context, name, disk string) error
	ReplaceDisk(context.Context, string, string, string) error
	AddReadCache(context.Context, string, []string) error
	RemoveReadCache(context.Context, string, string) error
	AddWriteCache(context.Context, string, []string) error
	RemoveWriteCache(context.Context, string, string) error
	FlushWriteCache(context.Context, string) error
	PoolStatus(context.Context, string) (domain.PoolStatus, error)
}

func NormalizeMAC(mac string) string {
	var b strings.Builder
	for _, r := range mac {
		if r == ':' || r == '-' || r == '.' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}
