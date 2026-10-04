package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tianwei/diskless/internal/control"
	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/network"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/platform"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
)

type BootScriptService interface {
	BuildBootScript(context.Context, string) (string, error)
	// DataDiskLetters 提供注入的开机脚本使用的 LUN→盘符映射，与 /boot 一样免认证。
	DataDiskLetters(context.Context, string) ([]control.DataDiskLetter, error)
	// NetConfig 提供同一脚本开机后恢复的分组网关/DNS（C-1 要求它们不进 iBFT），免认证。
	NetConfig(context.Context, string) (control.NetConfig, error)
	// RecordBootFailure 记下客户机 iPXE 脚本放弃时上报的内容。
	RecordBootFailure(ctx context.Context, mac, stage, code, platform string) error
	// CheckBootMode 在客户机固件启动不了要下发的镜像时，提前记一条失败。
	CheckBootMode(ctx context.Context, mac, platform string) error
	// RegisteredIP 是产品自己分配给该 MAC 的地址（还没有时为空）。无盘客户机拿到盘之前没有任何秘密，
	// 来源地址是 /boot 系列区分「问自己」和「问邻居」的唯一不由调用方自称的依据。
	RegisteredIP(context.Context, string) (string, error)
}

type AuthService interface {
	Login(context.Context, platform.LoginRequest) (platform.LoginResult, error)
	VerifyToken(string) (platform.UserInfo, error)
}

type ImageImportService interface {
	ImportImage(context.Context, assets.ImportImageRequest) (assets.ImportImageResult, error)
	CreateBlankImage(context.Context, assets.BlankImageRequest) (assets.ImportImageResult, error)
	ReductionExportName(context.Context, string, bool) (string, error)
	StreamReduction(ctx context.Context, reductionID string, w io.Writer, compress bool) error
	SaveReductionAsImage(context.Context, string, assets.SaveAsImageRequest) (assets.ImportImageResult, error)
	SetPurpose(context.Context, string, assets.SetPurposeRequest) (domain.Image, error)
	ListImportSources(context.Context) (assets.ImportSourceListResult, error)
	ExportImageToDir(context.Context, assets.ImageExportRequest) (assets.ExportImageResult, error)
	ExportReductionToDir(context.Context, string) (assets.ExportImageResult, error)
	ListImages(context.Context) (assets.ImageListResult, error)
	GetImage(context.Context, string) (assets.ImageDetail, error)
	DeleteImage(context.Context, string) error
	RunHealthCheck(context.Context, string) (assets.HealthCheckResult, error)
	GetHealth(context.Context, string) (domain.ImageHealthReport, error)
	ExportDownloadName(context.Context, string, bool) (string, error)
	StreamImage(context.Context, string, io.Writer, bool) error
}

type ConfigService interface {
	List(context.Context, string) (assets.ConfigListResult, error)
	CreateFromImage(context.Context, string, assets.CreateConfigRequest) (assets.ConfigOperationResult, error)
	CreateFromConfig(context.Context, string, assets.CreateConfigFromConfigRequest) (assets.ConfigOperationResult, error)
	Delete(context.Context, string) (assets.ConfigTaskResult, error)
	Merge(context.Context, string) (assets.ConfigTaskResult, error)
	OverwriteImage(context.Context, string) (assets.ConfigTaskResult, error)
}

type ReductionService interface {
	List(context.Context, string) (assets.ReductionListResult, error)
	Create(context.Context, string, assets.CreateReductionRequest) (assets.ReductionOperationResult, error)
	Delete(context.Context, string) (assets.ReductionTaskResult, error)
	Merge(context.Context, string, assets.MergeReductionsRequest) (assets.ReductionTaskResult, error)
	SetCurrent(context.Context, string) (assets.SetCurrentResult, error)
}

type GroupService interface {
	List(context.Context) (assets.GroupListResult, error)
	Get(context.Context, string) (domain.Group, error)
	GetDefault(context.Context) (domain.Group, error)
	Create(context.Context, assets.GroupRequest) (domain.Group, error)
	Update(context.Context, string, assets.GroupRequest) (domain.Group, error)
	Delete(context.Context, string) error
	SetDefault(context.Context, string) (domain.Group, error)
	PreviewNetwork(context.Context, string, assets.GroupNetworkRequest) (assets.GroupNetworkPreview, error)
}

type GroupDiskService interface {
	List(context.Context, string) (assets.GroupDiskListResult, error)
	Create(context.Context, string, assets.GroupDiskRequest) (domain.GroupDisk, error)
	Delete(context.Context, string) error
}

type TerminalService interface {
	List(context.Context, string) (assets.TerminalListResult, error)
	Get(context.Context, string) (domain.Terminal, error)
	Create(context.Context, assets.TerminalRequest) (domain.Terminal, error)
	Update(context.Context, string, assets.TerminalRequest) (domain.Terminal, error)
	Delete(context.Context, string) error
	Export(context.Context, string) ([]byte, error)
	Import(context.Context, []byte) (assets.TerminalImportResult, error)
	Move(context.Context, assets.MoveTerminalsRequest) (assets.MoveTerminalsResult, error)
	Heartbeat(context.Context, string) (domain.Terminal, error)
	EnableSuper(context.Context, string) (domain.Terminal, error)
	DisableSuper(context.Context, string) (domain.Terminal, error)
	StopSuper(context.Context, string, assets.SuperStopRequest) (assets.SuperStopResult, error)
	PublishDataDisk(context.Context, string, assets.PublishDataDiskRequest) (assets.PublishDataDiskResult, error)
	SuperDisks(context.Context, string) (assets.SuperDiskListResult, error)
}

type PoolService interface {
	List(context.Context, string) (ops.PoolListResult, error)
	ListDisks(context.Context) (ops.DiskListResult, error)
	Get(context.Context, string) (ops.PoolItem, error)
	Create(context.Context, ops.PoolRequest) (ops.PoolOperationResult, error)
	AddDisk(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	RemoveDisk(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	MirrorUpgrade(context.Context, string, ops.MirrorUpgradeRequest) (ops.PoolOperationResult, error)
	AddSpecial(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	RemoveSpecial(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	AddSpare(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	RemoveSpare(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error)
	ReplaceDisk(context.Context, string, ops.PoolReplaceDiskRequest) (ops.PoolOperationResult, error)
	AddReadCache(context.Context, string, ops.PoolReadCacheRequest) (ops.PoolOperationResult, error)
	RemoveReadCache(context.Context, string, ops.PoolReadCacheRequest) (ops.PoolOperationResult, error)
	AddWriteCache(context.Context, string, ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error)
	RemoveWriteCache(context.Context, string, ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error)
	FlushWriteCache(context.Context, string) (ops.PoolTaskResult, error)
	Destroy(context.Context, string) (ops.PoolTaskResult, error)
}

type UserService interface {
	List(context.Context) (platform.UserListResult, error)
	Create(context.Context, platform.CreateUserRequest) (platform.UserInfo, error)
	Update(context.Context, string, platform.UpdateUserRequest) (platform.UserInfo, error)
	Delete(context.Context, string) error
	ChangePassword(context.Context, string, platform.ChangePasswordRequest) error
}

type BackupService interface {
	GetConfig(context.Context) (domain.BackupConfig, error)
	SaveConfig(context.Context, ops.BackupConfigRequest) (domain.BackupConfig, error)
	Run(context.Context) (ops.BackupRunResult, error)
	Status(context.Context) (ops.BackupStatus, error)
}

type SettingsService interface {
	Get(context.Context) (platform.SystemSettingsView, error)
	Save(context.Context, platform.SystemSettingsRequest) (platform.SystemSettingsView, error)
}

// NetworkService 支撑客户机网络页：哪块网卡面向机房、分组能否不在其网段内，以及可达性探测。
type NetworkService interface {
	Get(context.Context, int) (network.View, error)
	Update(context.Context, network.Request) (network.View, error)
	Probe(context.Context, network.ProbeRequest) (network.ProbeResult, error)
}

type TaskService interface {
	GetTask(context.Context, string) (domain.Task, error)
	ListActiveTasks(context.Context) ([]domain.Task, error)
	ListTasks(context.Context, assets.TaskListQuery) (assets.TaskListResult, error)
}

type AdaptationService interface {
	InjectDriver(ctx context.Context, terminalID, bundleID string, overwrite bool) (adapt.InjectResult, error)
	CheckResult(ctx context.Context, terminalID string) (adapt.InjectResultView, error)
}

type DriverService interface {
	UploadPack(context.Context, adapt.UploadDriverPackRequest) (adapt.DriverPackItem, error)
	ListPacks(context.Context) (adapt.DriverPackListResult, error)
	SetPackStatus(context.Context, string, domain.DriverPackStatus) (domain.DriverPack, error)
	SetRecommended(context.Context, string) (domain.DriverPack, error)
	DeletePack(context.Context, string) error
	CreateBundle(context.Context, adapt.DriverBundleRequest) (adapt.DriverBundleItem, error)
	ListBundles(context.Context) (adapt.DriverBundleListResult, error)
	DeleteBundle(context.Context, string) error
	BundleArchive(context.Context, string) (string, []byte, error)
}

type RuntimeConfig struct {
	ImportDir string `json:"import_dir"`
}

type ServerService interface {
	Info(context.Context) (platform.ServerInfo, error)
	ListServices(context.Context) (platform.ServiceListResult, error)
	ServiceAction(context.Context, string, string) error
	ServiceLogs(context.Context, string, int) (string, error)
}

// UploadService 以可续传分片接收运维机器上的镜像文件。为 nil 时控制台只支持「文件已在服务器上」，
// 适合没有浏览器访问的部署。
type UploadService interface {
	Begin(context.Context, assets.BeginUploadRequest) (assets.UploadStatus, error)
	Status(context.Context, string) (assets.UploadStatus, error)
	Append(ctx context.Context, id string, offset int64, body io.Reader) (assets.UploadStatus, error)
	Abort(context.Context, string) error
}

type AuditService interface {
	Record(context.Context, platform.AuditEntry) error
	List(context.Context, platform.AuditQuery) (platform.AuditListResult, error)
}

type AlarmService interface {
	List(context.Context, platform.AlarmQuery) (platform.AlarmListResult, error)
	Acknowledge(context.Context, string) error
	Delete(context.Context, string) error
}

// ConsistencyService 报告库与池不一致之处。
type ConsistencyService interface {
	Check(context.Context) (ops.ConsistencyReport, error)
}

// HAService 是面向运维的 HA 接口：角色状态与计划切换。
type HAService interface {
	Status(ctx context.Context) (ha.APIStatus, error)
	PlannedSwitch(ctx context.Context) error
	SetRate(ctx context.Context, mbps int) error
}

// ReplicationStatusService 是 /api/replication 的读模型。
type ReplicationStatusService interface {
	Status(ctx context.Context) (ops.ReplicationStatus, error)
}

type Services struct {
	// Gate 是单写入者闸门：关闭时所有写操作和启动路径返回指向主机的 503；
	// 读、登录和健康检查保持开放，备机仍可查看。
	Gate *ha.Gate
	// HAHandler 提供 /internal/ha/ 下的集群内 HA 控制端点（对端守卫查状态、keepalived notify 脚本
	// 调 activate/standby），令牌认证在 handler 内。
	HAHandler http.Handler
	// HA 提供 /api/ha 下面向运维的角色状态与计划切换。
	HA HAService
	// ReplicationHandler 提供 /internal/replication/ 下的对端复制端点，集群令牌认证在 handler 内。
	ReplicationHandler http.Handler
	// ClusterHandler 提供 /internal/cluster/：register 兼作加入与心跳，peers 返回花名册，
	// join-config 把共享密钥交给从界面加入的节点。令牌认证在 handler 内。
	ClusterHandler http.Handler
	// ClusterJoin 提供 POST /api/cluster/join：从本机界面把独立节点加入集群。
	// 挂在需登录的 /api 下，请求里不带集群令牌。
	ClusterJoin http.Handler
	// ClusterCreate 提供 POST /api/cluster/create：把独立节点变成集群第一台。
	// 表单只有 VIP 一项，集群令牌自动生成。
	ClusterCreate http.Handler
	// AdoptHandler 提供 /internal/cluster/identity 和 /internal/cluster/adopt。故意免认证，
	// 安全前提是二者只在出厂态（无池、无集群）才做事；完整理由见 ha.AdoptHandler。
	AdoptHandler http.Handler
	// ClusterDiscover 列出客户机网段上未配置的节点，加机器只需点选，不必跑过去重输 VIP 和令牌。
	ClusterDiscover func(context.Context) ([]ha.Candidate, error)
	// ClusterAdoptNode 把一个已发现的节点纳入本集群。
	ClusterAdoptNode func(ctx context.Context, addr string) error
	// BootReadProxy 在本节点答不了只读 /boot 端点时转发给写入者。备机的在用目录不跟随写入者，
	// 而由它供盘的客户机正是向它要网关和盘符；没有转发，备机上的客户机有盘却没有网络配置。
	BootReadProxy func(ctx context.Context, path, rawQuery, clientIP, authorization string) (status int, body []byte, contentType string, err error)
	// NodeServices 提供 /internal/node/services：本节点自己的 systemd 视图，供写入者汇总集群矩阵。
	// 每个节点只能看到自己的 systemd。
	NodeServices func(context.Context) (platform.ServiceListResult, error)
	// NodePools 提供 /internal/node/pools：本节点的池及实时容量与健康。池记录随目录复制，
	// 但数字只有导入池的那台读得到。
	NodePools func(context.Context) (ops.PoolListResult, error)
	// NodeServiceAction / NodeServiceLogs 让对端操作本节点的服务。二者都带上调用方以为在对话的节点，
	// 不是本节点就拒绝：否则指向已改号机器的陈旧花名册会重启错的主机的 iSCSI。
	NodeServiceAction func(ctx context.Context, want, key, action string) error
	NodeServiceLogs   func(ctx context.Context, want, key string, lines int) (string, error)
	// NodeDisks 提供 /internal/node/disks：供要在本机建池的控制台列盘。同一个 /dev/sdb 在每台机器上是不同的盘。
	NodeDisks func(context.Context) (ops.DiskListResult, error)
	// StorageHandler 提供 /internal/storage/ 下的放置接缝，任何角色的节点都运行它：
	// 服务被放置的客户机是节点本地的工作。令牌认证在 handler 内。
	StorageHandler http.Handler
	// Replication 提供 GET /api/replication，即 HA 卡片上的备机滞后视图。
	Replication ReplicationStatusService
	// Health 是 /healthz 的报告，keepalived track_script 据此决定本节点能否持有 VIP，
	// 所以必须反映真实健康（库可达、池可读、LIO 在），不能是常量。为 nil 时恒为 ok。
	Health func(context.Context) error
	Boot   BootScriptService
	// BootPeerCheckOff 关闭 /boot 系列的来源地址检查。默认开启且应保持开启，它阻止一台客户机拿到
	// 另一台的磁盘坐标。开关存在是因为检查依据产品分配的地址，客户机若从与 PXE 不同的网卡访问 API
	// 就会全部开不了机：先关掉开关开机，再查地址为何对不上。
	BootPeerCheckOff bool
	Auth             AuthService
	Images           ImageImportService
	Uploads          UploadService
	Configs          ConfigService
	Reductions       ReductionService
	Groups           GroupService
	GroupDisks       GroupDiskService
	Terminals        TerminalService
	Pools            PoolService
	Users            UserService
	Backups          BackupService
	Consistency      ConsistencyService
	Settings         SettingsService
	Network          NetworkService
	Tasks            TaskService
	Drivers          DriverService
	Adaptations      AdaptationService
	Server           ServerService
	// ClusterNodes 为选择器列出节点花名册；为 nil 时隐藏端点（单机没得选）。
	ClusterNodes func(context.Context) (ops.NodeListResult, error)
	// ForgetNode 把已下线节点移出花名册。为 nil 时隐藏端点，成员只增不减，这正是单机需要的。
	ForgetNode func(ctx context.Context, nodeID string) error
	// ClusterToken 用于上面声明的节点间端点的认证。
	ClusterToken string
	// ClusterServices 是跨所有节点的服务矩阵；为 nil 时隐藏。
	ClusterServices func(context.Context) (platform.ClusterServicesResult, error)
	// ClusterBackups 是各节点自己池里的备份情况；为 nil 时隐藏。
	ClusterBackups func(context.Context) (platform.ClusterBackupsResult, error)
	// NodeBackup 提供 /internal/node/backup：从本节点的池读出的备份状态。
	NodeBackup func(context.Context) (ops.NodeBackupStatus, error)
	// NodePreserved 提供 /internal/node/preserved：本节点保留的目录副本。副本在保住它的那台（通常是备机），
	// 而告警在写入者上跑，所以由写入者来问。
	NodePreserved func(context.Context) ([]storage.PreservedCopy, error)
	// NodeTasks 提供 /internal/node/tasks：本节点执行过的池任务，只记在它自己的库里。
	NodeTasks func(context.Context) ([]domain.Task, error)
	// ClusterPools 是逐节点收集的全部存储；为 nil 时隐藏，控制台只显示本机的数字。
	ClusterPools func(context.Context) (platform.ClusterPoolsResult, error)
	// ClusterProxy 把一次存储写入转发给指定节点。池写入天然在磁盘所在的机器上执行，
	// 没有它控制台只能操作当前连着的那台，其余节点的存储在界面上只读。
	ClusterProxy func(ctx context.Context, node, method, suffix, query string, body io.Reader) (ProxyResponse, error)
	// NodePoolWrites 在 /internal/node/pools 下再挂一遍池路由，供对端持集群令牌调用。
	NodePoolWrites bool
	// NodeID 是本节点身份，转发来的请求在动任何东西之前要先核对它点名的节点。
	NodeID string
	// ClusterDisks 列出指定节点的盘；为 nil 时只在本机建池。
	ClusterDisks func(ctx context.Context, node string) (ops.DiskListResult, error)
	// ClusterServiceAction / ClusterServiceLogs 从本控制台操作任意节点的服务。为 nil 时页面只有本机控制，适合单机。
	ClusterServiceAction func(ctx context.Context, node, key, action string) error
	ClusterServiceLogs   func(ctx context.Context, node, key string, lines int) (string, error)
	Audit                AuditService
	Alarms               AlarmService
	Runtime              RuntimeConfig
}

// preservedList 是 /internal/node/preserved 的响应体，与其他列表同为 {items} 结构。
type preservedList struct {
	Items []storage.PreservedCopy `json:"items"`
}

type nodeTaskList struct {
	Items []domain.Task `json:"items"`
}

// nodeEndpoint 给面向对端的 /internal/node/ 只读端点包上集群令牌认证。这些问题只有节点自己答得了，
// 所以不论角色每台都运行，且不能回答只是连上了端口的人。
func nodeEndpoint[T any](token string, read func(context.Context) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if token == "" || subtle.ConstantTimeCompare(
			[]byte(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")),
			[]byte(token)) != 1 {
			http.Error(w, "cluster token required", http.StatusUnauthorized)
			return
		}
		res, err := read(req.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

// mountPoolRoutes 在指定前缀下注册池 API，让同一套 handler 同时服务控制台（/api/pools，会话认证）
// 和对端（/internal/node/pools，集群令牌认证）。池写入是节点本地的工作，复制十几条路由会让两份逐渐走样。
func mountPoolRoutes(r chi.Router, prefix string, pools PoolService, wrap func(http.HandlerFunc) http.HandlerFunc) {
	if wrap == nil {
		wrap = func(h http.HandlerFunc) http.HandlerFunc { return h }
	}
	post := func(path string, h http.HandlerFunc) { r.Post(prefix+path, wrap(h)) }
	accepted := status(http.StatusAccepted)
	r.Post(prefix, wrap(handleIn(pools.Create, accepted)))
	r.Get(prefix+"/{id}", wrap(handleID(pools.Get, notFound("存储池不存在"))))
	post("/{id}/disks", handleIDIn(pools.AddDisk, accepted))
	post("/{id}/disks/remove", handleIDIn(pools.RemoveDisk, accepted))
	post("/{id}/disks/replace", handleIDIn(pools.ReplaceDisk, accepted))
	post("/{id}/mirror-upgrade", handleIDIn(pools.MirrorUpgrade, accepted))
	post("/{id}/special", handleIDIn(pools.AddSpecial, accepted))
	post("/{id}/special/remove", handleIDIn(pools.RemoveSpecial, accepted))
	post("/{id}/spares", handleIDIn(pools.AddSpare, accepted))
	post("/{id}/spares/remove", handleIDIn(pools.RemoveSpare, accepted))
	post("/{id}/read-cache", handleIDIn(pools.AddReadCache, accepted))
	post("/{id}/read-cache/remove", handleIDIn(pools.RemoveReadCache, accepted))
	post("/{id}/write-cache", handleIDIn(pools.AddWriteCache, accepted))
	post("/{id}/write-cache/remove", handleIDIn(pools.RemoveWriteCache, accepted))
	post("/{id}/write-cache/flush", handleID(pools.FlushWriteCache, accepted))
	r.Delete(prefix+"/{id}", wrap(handleID(pools.Destroy, accepted)))
}

func clusterProxyHandler(forward func(ctx context.Context, node, method, suffix, query string, body io.Reader) (ProxyResponse, error), param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		suffix := ""
		if param != "" {
			suffix = chi.URLParam(r, param)
		}
		res, err := forward(r.Context(), chi.URLParam(r, "node"), r.Method, suffix, r.URL.RawQuery, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if res.ContentType != "" {
			w.Header().Set("Content-Type", res.ContentType)
		}
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
	}
}

// ProxyResponse 是对端的原样答复：运维需要那台节点自己说明为什么加不了盘，而不是本节点的猜测。
type ProxyResponse struct {
	Status      int
	Body        []byte
	ContentType string
}

// nodeGuard 是 nodeEndpoint 的令牌检查去掉 JSON 包装，用于带参数或请求体的对端端点。
func nodeGuard(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if token == "" || subtle.ConstantTimeCompare(
			[]byte(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")),
			[]byte(token)) != 1 {
			http.Error(w, "cluster token required", http.StatusUnauthorized)
			return
		}
		next(w, req)
	}
}

// targetNodeGuard 拒绝点名其他节点的转发写入。花名册条目可能比它指向的机器活得久，
// 在错的主机上销毁池不会失败，而是在错的主机上成功；核对名字是最后能拦住的地方。
func targetNodeGuard(self string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := platform.AssertSelf(self, r.URL.Query().Get("node")); err != nil {
			nodeOpError(w, err)
			return
		}
		next(w, r)
	}
}

// nodeOpError 区分「问错了机器」和「机器试过但失败了」：前者说明调用方花名册过期、重试无用，后者才是真失败。
func nodeOpError(w http.ResponseWriter, err error) {
	if errors.Is(err, platform.ErrWrongNode) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func NewRouter(logger *slog.Logger, assets fs.FS, services ...Services) http.Handler {
	var svc Services
	if len(services) > 0 {
		svc = services[0]
	}
	r := chi.NewRouter()
	r.Use(requestLogger(logger))
	if svc.Gate != nil {
		r.Use(gateMiddleware(svc.Gate))
	}

	if svc.AdoptHandler != nil {
		// 免认证，且只在出厂态才做事；必须挂在令牌门（ClusterHandler）之前，否则还没有令牌的新机器无从被发现。
		r.Handle("/internal/cluster/identity", svc.AdoptHandler)
		r.Handle("/internal/cluster/adopt", svc.AdoptHandler)
	}
	if svc.ClusterHandler != nil {
		r.Handle("/internal/cluster/*", svc.ClusterHandler)
	}
	if svc.StorageHandler != nil {
		r.Handle("/internal/storage/*", svc.StorageHandler)
	}
	if svc.NodePreserved != nil {
		r.Get("/internal/node/preserved", nodeEndpoint(svc.ClusterToken, func(ctx context.Context) (preservedList, error) {
			items, err := svc.NodePreserved(ctx)
			return preservedList{Items: items}, err
		}))
	}
	if svc.NodeTasks != nil {
		r.Get("/internal/node/tasks", nodeEndpoint(svc.ClusterToken, func(ctx context.Context) (nodeTaskList, error) {
			items, err := svc.NodeTasks(ctx)
			return nodeTaskList{Items: items}, err
		}))
	}
	if svc.NodeServices != nil {
		r.Get("/internal/node/services", nodeEndpoint(svc.ClusterToken, svc.NodeServices))
		if svc.NodeBackup != nil {
			r.Get("/internal/node/backup", nodeEndpoint(svc.ClusterToken, svc.NodeBackup))
		}
	}
	if svc.NodePools != nil {
		r.Get("/internal/node/pools", nodeEndpoint(svc.ClusterToken, svc.NodePools))
	}
	if svc.NodeDisks != nil {
		r.Get("/internal/node/disks", nodeEndpoint(svc.ClusterToken, svc.NodeDisks))
	}
	if svc.NodeServiceAction != nil {
		r.Post("/internal/node/services/{id}/action", nodeGuard(svc.ClusterToken, func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Action string `json:"action"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := svc.NodeServiceAction(req.Context(), req.URL.Query().Get("node"), chi.URLParam(req, "id"), body.Action); err != nil {
				nodeOpError(w, err)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}))
	}
	if svc.NodeServiceLogs != nil {
		r.Get("/internal/node/services/{id}/logs", nodeGuard(svc.ClusterToken, func(w http.ResponseWriter, req *http.Request) {
			lines, _ := strconv.Atoi(req.URL.Query().Get("lines"))
			logs, err := svc.NodeServiceLogs(req.Context(), req.URL.Query().Get("node"), chi.URLParam(req, "id"), lines)
			if err != nil {
				nodeOpError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"logs": logs})
		}))
	}
	// 给对端再挂一遍池 API，必须放在会话认证组外面：调用方是持集群令牌的节点，不是登录的浏览器。
	// 放在组里单测照样过（很少接认证服务），真实跨节点写入却全部 401，报的还是登录凭据问题。
	if svc.Pools != nil && svc.NodePoolWrites {
		mountPoolRoutes(r, "/internal/node/pools", svc.Pools, func(h http.HandlerFunc) http.HandlerFunc {
			return nodeGuard(svc.ClusterToken, targetNodeGuard(svc.NodeID, h))
		})
	}
	if svc.ReplicationHandler != nil {
		r.Handle("/internal/replication/*", svc.ReplicationHandler)
	}
	if svc.HAHandler != nil {
		r.Handle("/internal/ha/*", svc.HAHandler)
	}
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if svc.Health != nil {
			if err := svc.Health(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "reason": err.Error()})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	tickets := newExportTickets(nil)
	if svc.Images != nil {
		// 故意放在认证组外：浏览器下载是不带 Authorization 头的 <a href>，URL 里的一次性票据就是凭据，
		// 而签发票据（下面）需要登录。
		r.Get("/api/images/export-download", exportDownloadHandler(svc.Images, tickets, logger))
	}
	if svc.Boot != nil {
		r.Get("/boot", bootHandler(svc.ClusterToken, svc.Boot, svc.Auth, svc.BootPeerCheckOff, logger))
		r.Get("/boot/failed", bootFailedHandler(svc, svc.Boot, svc.Auth, svc.BootPeerCheckOff, logger))
		r.Get("/boot/data-disks", dataDisksHandler(svc, svc.Boot, svc.Auth, svc.BootPeerCheckOff, logger))
		r.Get("/boot/net-config", netConfigHandler(svc, svc.Boot, svc.Auth, svc.BootPeerCheckOff, logger))
	}
	if svc.Auth != nil {
		r.Post("/api/login", loginHandler(svc.Auth, svc.Audit))
		r.Group(func(api chi.Router) {
			api.Use(authMiddleware(svc.Auth))
			if svc.Audit != nil {
				api.Use(auditMiddleware(svc.Audit))
			}
			registerAPIRoutes(api, svc, tickets)
		})
	} else {
		registerAPIRoutes(r, svc, tickets)
	}

	if assets != nil {
		r.NotFound(spaFallback(assets))
	}

	return r
}

func registerAPIRoutes(r chi.Router, svc Services, tickets *exportTickets) {
	r.Get("/api/runtime", runtimeHandler(svc.Runtime, svc.Settings))
	if svc.Audit != nil {
		r.Get("/api/logs", logsHandler(svc.Audit))
	}
	// 用一条通配而不是十几条代理路由：对端本来就讲池 API，原样转发方法、路径后缀和请求体，增加路由时两边不会走样。
	if svc.ClusterProxy != nil {
		r.Handle("/api/cluster/nodes/{node}/pools", clusterProxyHandler(svc.ClusterProxy, ""))
		r.Handle("/api/cluster/nodes/{node}/pools/*", clusterProxyHandler(svc.ClusterProxy, "*"))
	}
	// 上传独立于镜像服务注册：它只把文件搬到导入目录，之后才轮到导入。
	if svc.Uploads != nil {
		r.Post("/api/images/uploads", handleIn(svc.Uploads.Begin, status(http.StatusCreated)))
		r.Get("/api/images/uploads/{id}", handleID(svc.Uploads.Status, notFound("上传会话不存在")))
		r.Patch("/api/images/uploads/{id}", uploadChunkHandler(svc.Uploads))
		r.Delete("/api/images/uploads/{id}", handleErrID(svc.Uploads.Abort))
	}
	// 不挂在 svc.Server 下：集群存储是存储问题，概览页不论有没有接服务管理都需要它。
	if svc.ClusterPools != nil {
		r.Get("/api/cluster/pools", handle(svc.ClusterPools))
	}
	if svc.ClusterDisks != nil {
		r.Get("/api/cluster/nodes/{node}/disks", handleReq(func(r *http.Request) (ops.DiskListResult, error) {
			return svc.ClusterDisks(r.Context(), chi.URLParam(r, "node"))
		}))
	}
	if svc.Alarms != nil {
		r.Get("/api/alarms", listAlarmsHandler(svc.Alarms))
		r.Post("/api/alarms/{id}/ack", handleErrID(svc.Alarms.Acknowledge, status(http.StatusOK)))
		r.Delete("/api/alarms/{id}", handleErrID(svc.Alarms.Delete, status(http.StatusOK)))
	}
	if svc.Server != nil {
		if svc.Replication != nil {
			r.Get("/api/replication", handle(svc.Replication.Status))
		}
		if svc.HA != nil {
			r.Get("/api/ha", handle(svc.HA.Status))
			r.Post("/api/ha/rate", func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					MBPS *int `json:"mbps"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.MBPS == nil {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "请提供 mbps（0 表示不限速）"})
					return
				}
				if err := svc.HA.SetRate(req.Context(), *body.MBPS); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			r.Post("/api/ha/planned-switch", func(w http.ResponseWriter, req *http.Request) {
				if err := svc.HA.PlannedSwitch(req.Context()); err != nil {
					// 两种拒绝的处置相反：指错机器要换一台重发，同步没完成要留在这台等复制追平；都回 409 现场就分不清。
					status := http.StatusConflict
					if ha.IsNotActive(err) {
						status = http.StatusMisdirectedRequest // 421：请求发到了错的机器
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
					return
				}
				w.WriteHeader(http.StatusAccepted)
			})
		}
		r.Get("/api/server", handle(svc.Server.Info))
		if svc.ClusterNodes != nil {
			r.Get("/api/cluster/nodes", handle(svc.ClusterNodes))
		}
		if svc.ClusterBackups != nil {
			r.Get("/api/cluster/backups", handle(svc.ClusterBackups))
		}
		if svc.ClusterServices != nil {
			r.Get("/api/cluster/services", handle(svc.ClusterServices))
		}
		if svc.ForgetNode != nil {
			// 这段参数叫 {node}（与同前缀的跨节点路由一致），不能用取 {id} 的通用处理器：
			// 那会拿到空串，把「移除某台」变成「移除没指定的那台」。
			r.Delete("/api/cluster/nodes/{node}", func(w http.ResponseWriter, req *http.Request) {
				finishEmpty(w, newResponder(http.StatusNoContent, nil), svc.ForgetNode(req.Context(), chi.URLParam(req, "node")))
			})
		}
		if svc.ClusterJoin != nil {
			r.Post("/api/cluster/join", svc.ClusterJoin.ServeHTTP)
		}
		if svc.ClusterCreate != nil {
			r.Post("/api/cluster/create", svc.ClusterCreate.ServeHTTP)
		}
		if svc.ClusterDiscover != nil {
			r.Get("/api/cluster/discover", handle(func(ctx context.Context) (discoverResult, error) {
				found, err := svc.ClusterDiscover(ctx)
				return discoverResult{Items: found, Total: len(found)}, err
			}))
		}
		if svc.ClusterAdoptNode != nil {
			r.Post("/api/cluster/adopt", adoptNodeHandler(svc.ClusterAdoptNode))
		}
		if svc.ClusterServiceAction != nil {
			r.Post("/api/cluster/nodes/{node}/services/{id}/action", clusterServiceActionHandler(svc.ClusterServiceAction))
		}
		if svc.ClusterServiceLogs != nil {
			r.Get("/api/cluster/nodes/{node}/services/{id}/logs", clusterServiceLogsHandler(svc.ClusterServiceLogs))
		}
		r.Get("/api/services", handle(svc.Server.ListServices))
		r.Post("/api/services/{id}/action", serviceActionHandler(svc.Server))
		r.Get("/api/services/{id}/logs", serviceLogsHandler(svc.Server))
	}
	if svc.Images != nil {
		r.Get("/api/images", handle(svc.Images.ListImages))
		r.Get("/api/images/import-sources", handle(svc.Images.ListImportSources))
		// 导出到本机目录：盘对盘，不经网络。落点是此刻在服务的那台，结果里带着是哪台。
		r.Post("/api/images/{id}/export", handleID(func(ctx context.Context, id string) (assets.ExportImageResult, error) {
			return svc.Images.ExportImageToDir(ctx, assets.ImageExportRequest{ImageID: id})
		}, status(http.StatusAccepted), notFound("镜像不存在")))
		r.Get("/api/images/{id}", handleID(svc.Images.GetImage, notFound("镜像不存在")))
		r.Post("/api/images/import", handleIn(svc.Images.ImportImage, status(http.StatusAccepted)))
		r.Post("/api/images/blank", handleIn(svc.Images.CreateBlankImage, status(http.StatusAccepted)))
		r.Patch("/api/images/{id}", handleIDIn(svc.Images.SetPurpose, notFound("镜像不存在")))
		r.Delete("/api/images/{id}", handleErrID(svc.Images.DeleteImage, notFound("镜像不存在")))
		r.Post("/api/images/{id}/export-ticket", exportTicketHandler(svc.Images, tickets))
		r.Post("/api/reductions/{id}/export-ticket", reductionExportTicketHandler(svc.Images, tickets))
		r.Post("/api/reductions/{id}/export", handleID(svc.Images.ExportReductionToDir, status(http.StatusAccepted), notFound("还原点不存在")))
		r.Post("/api/reductions/{id}/save-as-image", handleIDIn(svc.Images.SaveReductionAsImage, status(http.StatusAccepted), notFound("还原点不存在")))
		r.Post("/api/images/{id}/health-check", handleID(svc.Images.RunHealthCheck, status(http.StatusAccepted), notFound("镜像不存在")))
		r.Get("/api/images/{id}/health", handleID(svc.Images.GetHealth, notFound("体检报告不存在")))
	}
	if svc.Configs != nil {
		r.Get("/api/images/{id}/configs", handleID(svc.Configs.List, notFound("镜像不存在")))
		r.Post("/api/images/{id}/configs", handleIDIn(svc.Configs.CreateFromImage, status(http.StatusAccepted), notFound("镜像不存在")))
		r.Post("/api/configs/{id}/fork", handleIDIn(svc.Configs.CreateFromConfig, status(http.StatusAccepted), notFound("源配置不存在")))
		r.Post("/api/configs/{id}/merge", handleID(svc.Configs.Merge, status(http.StatusAccepted), notFound("配置不存在")))
		r.Post("/api/reductions/{id}/overwrite-image", handleID(svc.Configs.OverwriteImage, status(http.StatusAccepted), notFound("还原点不存在")))
		r.Delete("/api/configs/{id}", handleID(svc.Configs.Delete, status(http.StatusAccepted), notFound("配置不存在")))
	}
	if svc.Reductions != nil {
		r.Get("/api/configs/{id}/reductions", handleID(svc.Reductions.List, notFound("配置不存在")))
		r.Post("/api/configs/{id}/reductions", handleIDIn(svc.Reductions.Create, status(http.StatusAccepted), notFound("配置不存在")))
		r.Post("/api/configs/{id}/reductions/merge", handleIDIn(svc.Reductions.Merge, status(http.StatusAccepted), notFound("还原点不存在")))
		r.Delete("/api/reductions/{id}", handleID(svc.Reductions.Delete, status(http.StatusAccepted), notFound("还原点不存在")))
		r.Post("/api/reductions/{id}/apply", handleID(svc.Reductions.SetCurrent, notFound("还原点不存在")))
	}
	if svc.Groups != nil {
		r.Get("/api/groups", handle(svc.Groups.List))
		r.Post("/api/groups", handleIn(svc.Groups.Create, status(http.StatusCreated)))
		r.Get("/api/groups/default", handle(svc.Groups.GetDefault, notFound("默认分组不存在")))
		r.Get("/api/groups/{id}", handleID(svc.Groups.Get, notFound("分组不存在")))
		r.Put("/api/groups/{id}", handleIDIn(svc.Groups.Update))
		r.Post("/api/groups/{id}/network-preview", handleIDIn(svc.Groups.PreviewNetwork, notFound("分组不存在")))
		r.Delete("/api/groups/{id}", handleErrID(svc.Groups.Delete))
		r.Post("/api/groups/{id}/default", handleID(svc.Groups.SetDefault))
	}
	if svc.GroupDisks != nil {
		r.Get("/api/groups/{id}/disks", handleID(svc.GroupDisks.List, notFound("分组不存在")))
		r.Post("/api/groups/{id}/disks", handleIDIn(svc.GroupDisks.Create, status(http.StatusCreated)))
		r.Delete("/api/group-disks/{id}", handleErrID(svc.GroupDisks.Delete))
	}
	if svc.Terminals != nil {
		r.Get("/api/terminals", listTerminalsHandler(svc.Terminals))
		r.Post("/api/terminals", handleIn(svc.Terminals.Create, status(http.StatusCreated)))
		r.Get("/api/terminals/template", terminalTemplateHandler())
		r.Get("/api/terminals/export", exportTerminalsHandler(svc.Terminals))
		r.Post("/api/terminals/import", importTerminalsHandler(svc.Terminals))
		r.Post("/api/terminals/move", handleIn(svc.Terminals.Move))
		r.Post("/api/terminals/heartbeat", terminalHeartbeatHandler(svc.Terminals))
		r.Post("/api/terminals/{id}/super", handleID(svc.Terminals.EnableSuper))
		r.Delete("/api/terminals/{id}/super", handleID(svc.Terminals.DisableSuper))
		r.Post("/api/terminals/{id}/super/stop", stopTerminalSuperHandler(svc.Terminals))
		// 数据盘在线发布：机器不关，只有这块盘脱机几秒。系统盘没有这条路。
		r.Post("/api/terminals/{id}/super/publish", handleIDIn(svc.Terminals.PublishDataDisk, status(http.StatusAccepted)))
		r.Get("/api/terminals/{id}/super/disks", handleID(svc.Terminals.SuperDisks))
		r.Get("/api/terminals/{id}", handleID(svc.Terminals.Get, notFound("终端不存在")))
		r.Put("/api/terminals/{id}", handleIDIn(svc.Terminals.Update))
		r.Delete("/api/terminals/{id}", handleErrID(svc.Terminals.Delete))
	}
	if svc.Pools != nil {
		r.Get("/api/storage/disks", handle(svc.Pools.ListDisks))
		r.Get("/api/pools", listPoolsHandler(svc.Pools))
		mountPoolRoutes(r, "/api/pools", svc.Pools, nil)
	}
	if svc.Users != nil {
		r.Get("/api/users", handle(svc.Users.List))
		r.Post("/api/users", handleIn(svc.Users.Create, status(http.StatusCreated)))
		r.Put("/api/users/{id}", handleIDIn(svc.Users.Update))
		r.Delete("/api/users/{id}", handleErrID(svc.Users.Delete))
		r.Post("/api/users/{id}/password", handleErrIDIn(svc.Users.ChangePassword))
	}
	if svc.Consistency != nil {
		r.Get("/api/system/consistency", handle(svc.Consistency.Check))
	}
	if svc.Backups != nil {
		r.Get("/api/backup/config", handle(svc.Backups.GetConfig))
		r.Put("/api/backup/config", handleIn(svc.Backups.SaveConfig))
		r.Post("/api/backup/run", handle(svc.Backups.Run, status(http.StatusAccepted)))
		r.Get("/api/backup/status", handle(svc.Backups.Status))
	}
	if svc.Network != nil {
		r.Get("/api/network", handleReq(func(r *http.Request) (network.View, error) {
			clientMax, _ := strconv.Atoi(r.URL.Query().Get("client_max"))
			if clientMax <= 0 {
				clientMax = 30
			}
			return svc.Network.Get(r.Context(), clientMax)
		}))
		r.Put("/api/network", handleIn(svc.Network.Update))
		r.Post("/api/network/probe", handleIn(svc.Network.Probe))
	}
	if svc.Settings != nil {
		r.Get("/api/settings", handle(svc.Settings.Get))
		r.Put("/api/settings", handleIn(svc.Settings.Save))
	}
	if svc.Tasks != nil {
		r.Get("/api/tasks", activeTasksHandler(svc.Tasks))
		r.Get("/api/tasks/history", tasksHistoryHandler(svc.Tasks))
		r.Get("/api/tasks/{id}", handleID(svc.Tasks.GetTask, notFound("任务不存在")))
	}
	if svc.Adaptations != nil {
		r.Post("/api/terminals/{id}/inject-driver", injectDriverHandler(svc.Adaptations))
		r.Get("/api/terminals/{id}/inject-result", handleID(svc.Adaptations.CheckResult, notFound("终端不存在")))
	}
	if svc.Drivers != nil {
		r.Get("/api/driver-packs", handle(svc.Drivers.ListPacks))
		r.Post("/api/driver-packs", uploadDriverPackHandler(svc.Drivers))
		r.Post("/api/driver-packs/{id}/status", driverPackStatusHandler(svc.Drivers))
		r.Post("/api/driver-packs/{id}/recommend", handleID(svc.Drivers.SetRecommended))
		r.Delete("/api/driver-packs/{id}", handleErrID(svc.Drivers.DeletePack))
		r.Get("/api/driver-bundles", handle(svc.Drivers.ListBundles))
		r.Post("/api/driver-bundles", handleIn(svc.Drivers.CreateBundle, status(http.StatusCreated)))
		r.Get("/api/driver-bundles/{id}/archive", driverBundleArchiveHandler(svc.Drivers))
		r.Delete("/api/driver-bundles/{id}", handleErrID(svc.Drivers.DeleteBundle))
	}
}

// 下面这些具名 handler 的胶水不止一个方法值（查询参数、传输层请求类型或结果规整），
// 都建在同一套适配器上，保证错误统一经 writeError 出去。

func logsHandler(audit AuditService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (platform.AuditListResult, error) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("size"))
		return audit.List(r.Context(), platform.AuditQuery{
			Type: q.Get("type"), Username: q.Get("user"), Module: q.Get("module"),
			Status: q.Get("status"), Search: q.Get("q"), From: q.Get("from"), To: q.Get("to"),
			Page: page, Size: size,
		})
	})
}

func listAlarmsHandler(service AlarmService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (platform.AlarmListResult, error) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("size"))
		return service.List(r.Context(), platform.AlarmQuery{
			Status: q.Get("status"), Severity: q.Get("severity"), Page: page, Size: size,
		})
	})
}

func serviceActionHandler(service ServerService) http.HandlerFunc {
	type actionRequest struct {
		Action string `json:"action"`
	}
	return handleErrIDIn(func(ctx context.Context, key string, req actionRequest) error {
		return service.ServiceAction(ctx, key, req.Action)
	}, status(http.StatusAccepted))
}

// uploadChunkHandler 把一个分片追加到 Content-Range 指定的偏移处。
// 偏移必填、不设默认：把缺失当作从 0 开始，会让每次续传都变成覆盖（浏览器重连后把尾部写到头上）。
func uploadChunkHandler(up UploadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		offset, err := parseContentRangeStart(r.Header.Get("Content-Range"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st, err := up.Append(r.Context(), chi.URLParam(r, "id"), offset, r.Body)
		finish(w, newResponder(http.StatusOK, nil), st, err)
	}
}

// parseContentRangeStart 从 "bytes <start>-<end>/<total>" 中读出起始字节位置。
func parseContentRangeStart(h string) (int64, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, fmt.Errorf("缺少 Content-Range：分片必须说明自己从第几字节开始，否则续传会变成覆盖")
	}
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, fmt.Errorf("Content-Range 格式应为 bytes <起>-<止>/<总>，收到 %q", h)
	}
	span, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, fmt.Errorf("Content-Range 缺少总长度：%q", h)
	}
	startStr, _, ok := strings.Cut(span, "-")
	if !ok {
		return 0, fmt.Errorf("Content-Range 缺少结束位置：%q", h)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil || start < 0 {
		return 0, fmt.Errorf("Content-Range 的起点不是合法数字：%q", h)
	}
	return start, nil
}

func clusterServiceActionHandler(act func(ctx context.Context, node, key, action string) error) http.HandlerFunc {
	type actionRequest struct {
		Action string `json:"action"`
	}
	return handleReq(func(r *http.Request) (struct{}, error) {
		var body actionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, act(r.Context(), chi.URLParam(r, "node"), chi.URLParam(r, "id"), body.Action)
	})
}

func clusterServiceLogsHandler(read func(ctx context.Context, node, key string, lines int) (string, error)) http.HandlerFunc {
	return handleReq(func(r *http.Request) (map[string]string, error) {
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		logs, err := read(r.Context(), chi.URLParam(r, "node"), chi.URLParam(r, "id"), lines)
		return map[string]string{"logs": logs}, err
	})
}

func serviceLogsHandler(service ServerService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (map[string]string, error) {
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		logs, err := service.ServiceLogs(r.Context(), chi.URLParam(r, "id"), lines)
		return map[string]string{"logs": logs}, err
	})
}

func listTerminalsHandler(service TerminalService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (assets.TerminalListResult, error) {
		return service.List(r.Context(), r.URL.Query().Get("group_id"))
	})
}

func terminalHeartbeatHandler(service TerminalService) http.HandlerFunc {
	return handleIn(func(ctx context.Context, req terminalHeartbeatRequest) (domain.Terminal, error) {
		return service.Heartbeat(ctx, req.MAC)
	})
}

func activeTasksHandler(service TaskService) http.HandlerFunc {
	return handle(func(ctx context.Context) ([]domain.Task, error) {
		tasks, err := service.ListActiveTasks(ctx)
		if tasks == nil {
			tasks = []domain.Task{}
		}
		return tasks, err
	})
}

func tasksHistoryHandler(service TaskService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (assets.TaskListResult, error) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("size"))
		return service.ListTasks(r.Context(), assets.TaskListQuery{
			Status: q.Get("status"), Page: page, Size: size,
		})
	})
}

func listPoolsHandler(service PoolService) http.HandlerFunc {
	return handleReq(func(r *http.Request) (ops.PoolListResult, error) {
		return service.List(r.Context(), r.URL.Query().Get("server_id"))
	})
}

func injectDriverHandler(service AdaptationService) http.HandlerFunc {
	type injectRequest struct {
		BundleID  string `json:"bundle_id"`
		Overwrite bool   `json:"overwrite"`
	}
	return handleIDIn(func(ctx context.Context, id string, req injectRequest) (adapt.InjectResult, error) {
		return service.InjectDriver(ctx, id, req.BundleID, req.Overwrite)
	}, notFound("终端或驱动集不存在"))
}

func driverPackStatusHandler(service DriverService) http.HandlerFunc {
	return handleIDIn(func(ctx context.Context, id string, req driverPackStatusRequest) (domain.DriverPack, error) {
		return service.SetPackStatus(ctx, id, req.Status)
	})
}

// 转发的只读 /boot 请求带三种独立身份，分开才能让两类调用方都正常：
//
//	Authorization    原始调用方带的内容（管理员会话，或普通客户机什么都不带），原样透传，
//	                 对备机 curl 与对写入者结果一致；混入转发节点的令牌会吞掉运维的会话
//	X-ND-Cluster     转发节点的集群令牌，证明是对端
//	X-ND-Client      真实客户机地址，只在 X-ND-Cluster 有效时采信，「只回答本机」按它判断
const (
	bootClientHeader  = "X-ND-Client"
	bootClusterHeader = "X-ND-Cluster"
)

// answersFor 判断这个请求能否得到该 MAC 的信息。
// /boot 响应里有 sanhook 的 target IQN，即那台机器系统盘的全部坐标；target 名由 MAC 推得，
// 同网段 `arp -a` 就能列出全部 MAC。替别人问到后挂上正被使用的盘，第二个写入者会写坏 NTFS。
// 能核对的只有来源地址：它是产品自己分配、写进 dnsmasq 的，冒用就会撞 IP。这不防 ARP 欺骗，
// 但把「点几下鼠标」变成「主动攻击且留痕」。未分配地址的终端放行：它正要首次开机，盘还不存在。
func answersFor(r *http.Request, service BootScriptService, auth AuthService, mac string, off bool, clusterToken string) (bool, string) {
	if off {
		return true, ""
	}
	// 已登录的放行：运维要 curl 任意一台的脚本排查，e2e 要模拟任意 MAC 开机。
	// 这道门防的是坐在客户机前的普通用户，不是管理员。
	if auth != nil {
		if token := bearerToken(r); token != "" {
			if _, err := auth.VerifyToken(token); err == nil {
				return true, ""
			}
		}
	}
	want, err := service.RegisteredIP(r.Context(), mac)
	if err != nil || want == "" {
		return true, "" // 查不到或未登记：按老行为放行，安全不该以开不了机为代价
	}
	// 集群节点替客户机转发时，要判的是客户机的地址而不是转发节点的。
	// 这个头只在对方出示集群令牌时采信，否则任何人都能拿它冒充别的机器绕过这道门。
	remote := r.RemoteAddr
	if relayedByPeer(r, clusterToken) {
		if fwd := strings.TrimSpace(r.Header.Get(bootClientHeader)); fwd != "" {
			remote = fwd
		}
	}
	host, _, splitErr := net.SplitHostPort(remote)
	if splitErr != nil {
		host = remote
	}
	if net.ParseIP(host).Equal(net.ParseIP(want)) {
		return true, ""
	}
	return false, host
}

// bearerToken 从 Authorization 头取出 token，没有时返回 ""。
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[len("Bearer "):])
}

func bootHandler(clusterToken string, service BootScriptService, auth AuthService, peerCheckOff bool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mac := q.Get("mac")
		if mac == "" {
			http.Error(w, "mac is required", http.StatusBadRequest)
			return
		}
		// 答成「没这台机器」而不是「不许你问」：403 会让这个端点变成 MAC 在册与否的枚举器。
		if ok, from := answersFor(r, service, auth, mac, peerCheckOff, clusterToken); !ok {
			// 必须留痕：返回的 404 与「没这台机器」完全一样（有意为之），现场被误挡时只能靠这条日志分辨。
			logger.Warn("refused a boot request asking for another machine's configuration",
				"mac", mac, "from", from, "path", r.URL.Path)
			http.Error(w, "unknown mac", http.StatusNotFound)
			return
		}
		script, err := service.BuildBootScript(r.Context(), mac)
		if errors.Is(err, control.ErrUnknownTerminal) {
			http.Error(w, "unknown mac", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := service.CheckBootMode(r.Context(), mac, q.Get("platform")); err != nil {
			logger.Warn("could not check the client's firmware against its image", "mac", mac, "error", err)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(script))
	}
}

// bootFailedHandler 接收客户机 iPXE 脚本在 sanhook 或 sanboot 失败时发来的报告。
func bootFailedHandler(svc Services, service BootScriptService, auth AuthService, peerCheckOff bool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mac := q.Get("mac")
		if mac == "" {
			http.Error(w, "mac is required", http.StatusBadRequest)
			return
		}
		if ok, from := answersFor(r, service, auth, mac, peerCheckOff, svc.ClusterToken); !ok {
			logger.Warn("refused a boot request asking for another machine's configuration",
				"mac", mac, "from", from, "path", r.URL.Path)
			http.Error(w, "unknown mac", http.StatusNotFound)
			return
		}
		err := service.RecordBootFailure(r.Context(), mac, q.Get("stage"), q.Get("err"), q.Get("platform"))
		switch {
		case errors.Is(err, control.ErrUnknownTerminal):
			http.Error(w, "unknown mac", http.StatusNotFound)
		case errors.Is(err, errs.ErrInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			logger.Warn("client reported a failed boot", "mac", mac, "stage", q.Get("stage"), "err", q.Get("err"), "platform", q.Get("platform"))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("recorded\n"))
		}
	}
}

// bootMACHandler 适配 /boot 系列的 JSON 端点：免认证，按 ?mac= 取键，使用固定的英文错误阶梯
// （缺 mac → 400，未知终端 → 404 "unknown mac"，其余 → 500），客户机里的 mount-disks.ps1 依赖它。
// bootHandler 返回 text/plain 且读硬件提示，所以手工实现同一阶梯。
func bootMACHandler[Res any](svc Services, service BootScriptService, auth AuthService, peerCheckOff bool, logger *slog.Logger, fn func(ctx context.Context, mac string) (Res, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mac := r.URL.Query().Get("mac")
		if mac == "" {
			http.Error(w, "mac is required", http.StatusBadRequest)
			return
		}
		// 备机本地库不跟随写入者，登记的地址和答案都可能是旧的；先交写入者按客户机地址核对并作答，问不到再走本地。
		if standby(svc) && !relayedByPeer(r, svc.ClusterToken) && forwardBootRead(w, r, svc, logger) {
			return
		}
		// 同一道门：这两个端点泄露分组网关、DNS 和盘符映射，同样不该替别人回答；调用它们的客户机脚本来源地址与 /boot 相同。
		if ok, from := answersFor(r, service, auth, mac, peerCheckOff, svc.ClusterToken); !ok {
			logger.Warn("refused a boot request asking for another machine's configuration",
				"mac", mac, "from", from, "path", r.URL.Path)
			http.Error(w, "unknown mac", http.StatusNotFound)
			return
		}
		res, err := fn(r.Context(), mac)
		if errors.Is(err, control.ErrUnknownTerminal) {
			// 本机不认得就问写入者：这台客户机的盘由本机供着，它没别处可问；备机在用库不跟随写入者，「不认得」在集群里是常态。
			if forwardBootRead(w, r, svc, logger) {
				return
			}
			http.Error(w, "unknown mac", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if t, ok := any(res).(interface{ plainText() string }); ok && r.URL.Query().Get("format") == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(t.plainText()))
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// dataDiskList 还能以文本作答，每盘一行 "LUN<TAB>target"，供只有 shell、没有 JSON 解析器的 Linux 客户机脚本使用。
type dataDiskList struct {
	Items []control.DataDiskLetter `json:"items"`
}

func (l dataDiskList) plainText() string {
	var b strings.Builder
	for _, d := range l.Items {
		fmt.Fprintf(&b, "%d\t%s\n", d.LUN, d.Letter)
	}
	return b.String()
}

func dataDisksHandler(svc Services, service BootScriptService, auth AuthService, peerCheckOff bool, logger *slog.Logger) http.HandlerFunc {
	return bootMACHandler(svc, service, auth, peerCheckOff, logger, func(ctx context.Context, mac string) (dataDiskList, error) {
		items, err := service.DataDiskLetters(ctx, mac)
		return dataDiskList{Items: items}, err
	})
}

func netConfigHandler(svc Services, service BootScriptService, auth AuthService, peerCheckOff bool, logger *slog.Logger) http.HandlerFunc {
	return bootMACHandler(svc, service, auth, peerCheckOff, logger, service.NetConfig)
}

func loginHandler(service AuthService, audit AuditService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req platform.LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "请求格式错误", http.StatusBadRequest)
			return
		}
		result, err := service.Login(r.Context(), req)
		if errors.Is(err, platform.ErrInvalidCredentials) {
			recordLogin(r, audit, req.Username, "err", "登录失败：用户名或密码错误")
			http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recordLogin(r, audit, req.Username, "ok", "登录成功")
		writeJSON(w, http.StatusOK, result)
	}
}

func recordLogin(r *http.Request, audit AuditService, username, status, msg string) {
	if audit == nil {
		return
	}
	_ = audit.Record(r.Context(), platform.AuditEntry{
		Type: "login", Username: username, Action: msg, Detail: msg,
		Module: "登录", IP: clientIP(r), UserAgent: r.UserAgent(), Status: status,
	})
}

type userCtxKeyType struct{}

var userCtxKey userCtxKeyType

func userFromContext(ctx context.Context) (platform.UserInfo, bool) {
	info, ok := ctx.Value(userCtxKey).(platform.UserInfo)
	return info, ok
}

// auditMiddleware 把每个修改性的 /api 请求记为操作审计（读不审计）。须在 authMiddleware 之后运行，上下文里才有用户。
func auditMiddleware(audit AuditService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auditableRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			username := ""
			if info, ok := userFromContext(r.Context()); ok {
				username = info.Username
			}
			module, action := deriveModuleAction(r.Method, r.URL.Path)
			status := "ok"
			if sw.status >= 400 {
				status = "err"
			}
			_ = audit.Record(r.Context(), platform.AuditEntry{
				Type: "operation", Username: username, Action: action, Module: module,
				Detail: r.Method + " " + r.URL.Path, IP: clientIP(r), Status: status,
				HTTPStatus: sw.status, CostMs: time.Since(start).Milliseconds(),
			})
		})
	}
}

func auditableRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	if r.URL.Path == "/api/terminals/heartbeat" || r.URL.Path == "/api/login" {
		return false
	}
	// 分片追加每 8MB 一次，大镜像会刷出几千条审计；开始和放弃照常审计。
	if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/images/uploads/") {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/api/")
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

var auditModules = map[string]string{
	"images": "镜像", "configs": "镜像", "reductions": "镜像",
	"terminals": "终端", "groups": "分组", "group-disks": "分组",
	"pools": "存储", "storage": "存储", "users": "用户",
	"server": "服务器", "services": "服务器", "backup": "备份",
	"settings":     "系统参数",
	"driver-packs": "驱动", "driver-bundles": "驱动",
	"logs": "日志", "alarms": "告警",
}

var auditActions = map[string]string{
	"import": "导入", "export": "导出", "export-ticket": "导出下载", "move": "移动", "merge": "合并",
	"fork": "派生", "default": "设为默认", "password": "改密码", "run": "执行",
	"health-check": "体检", "recommend": "推荐", "status": "状态变更",
	"replace": "替换", "remove": "移除", "flush": "刷新", "action": "服务操作",
	"super": "超管", "stop": "超管停机", "ack": "确认告警",
	"read-cache": "读缓存", "write-cache": "写缓存", "disks": "磁盘变更",
}

// deriveModuleAction 把方法和路径转成审计用的模块/动作名。
func deriveModuleAction(method, path string) (module, action string) {
	segs := strings.Split(strings.Trim(strings.TrimPrefix(path, "/api/"), "/"), "/")
	module = "系统"
	if len(segs) > 0 {
		if m, ok := auditModules[segs[0]]; ok {
			module = m
		}
	}
	// 末尾的动词类（非 id）路径段用于细化动作。
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		if a, ok := auditActions[last]; ok {
			return module, a
		}
	}
	switch method {
	case http.MethodPost:
		action = "新建"
	case http.MethodPut, http.MethodPatch:
		action = "更新"
	case http.MethodDelete:
		action = "删除"
	default:
		action = method
	}
	return module, action
}

// writeError 是传输层唯一的错误适配器：按控制层错误类别映射 HTTP 状态（404/409/400/401/403），未分类的一律 500。
func writeError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errs.IsNotFound(err):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errs.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errs.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errs.ErrUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, errs.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return true
}

func authMiddleware(service AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/terminals/heartbeat" {
				next.ServeHTTP(w, r)
				return
			}
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "未登录", http.StatusUnauthorized)
				return
			}
			info, err := service.VerifyToken(token)
			if err != nil {
				http.Error(w, "登录凭证无效或已过期", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userCtxKey, info)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func runtimeHandler(cfg RuntimeConfig, settings SettingsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := cfg
		if settings != nil {
			view, err := settings.Get(r.Context())
			if writeError(w, err) {
				return
			}
			resp.ImportDir = view.ImportDir
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func terminalTemplateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.TerminalTemplateXLSX()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeXLSX(w, "terminal-template.xlsx", data)
	}
}

func exportTerminalsHandler(service TerminalService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := service.Export(r.Context(), r.URL.Query().Get("group_id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeXLSX(w, "terminals.xlsx", data)
	}
}

func importTerminalsHandler(service TerminalService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := readImportFile(r)
		if err != nil {
			http.Error(w, "导入文件无效", http.StatusBadRequest)
			return
		}
		result, err := service.Import(r.Context(), data)
		if errors.Is(err, assets.ErrTerminalImportInvalid) {
			http.Error(w, "导入文件无效", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(result.Errors) > 0 {
			writeJSON(w, http.StatusBadRequest, result)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

type terminalHeartbeatRequest struct {
	MAC string `json:"mac"`
}

func stopTerminalSuperHandler(service TerminalService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req assets.SuperStopRequest
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "请求格式错误", http.StatusBadRequest)
				return
			}
		}
		result, err := service.StopSuper(r.Context(), chi.URLParam(r, "id"), req)
		if writeError(w, err) {
			return
		}
		writeJSON(w, http.StatusAccepted, result)
	}
}

func readImportFile(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		file, _, err := r.FormFile("file")
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return io.ReadAll(file)
	}
	return io.ReadAll(r.Body)
}

func writeXLSX(w http.ResponseWriter, filename string, data []byte) {
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

const maxDriverUploadBytes = 1 << 30 // 1 GiB

func uploadDriverPackHandler(service DriverService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxDriverUploadBytes)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "上传表单无效", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "缺少文件字段", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "读取上传内容失败", http.StatusBadRequest)
			return
		}
		req := adapt.UploadDriverPackRequest{
			Name:     r.FormValue("name"),
			Category: domain.DriverPackCategory(r.FormValue("category")),
			OSType:   domain.OSType(r.FormValue("os_type")),
			Arch:     r.FormValue("arch"),
			Filename: header.Filename,
			Data:     data,
		}
		item, err := service.UploadPack(r.Context(), req)
		if writeError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, item)
	}
}

type driverPackStatusRequest struct {
	Status domain.DriverPackStatus `json:"status"`
}

func driverBundleArchiveHandler(service DriverService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, data, err := service.BundleArchive(r.Context(), chi.URLParam(r, "id"))
		if writeError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", contentDisposition(name))
		_, _ = w.Write(data)
	}
}

// contentDisposition 生成下载文件名。HTTP 头是 latin-1，中文包名会变乱码；RFC 6266 的 filename*
// 携带百分号编码的真名，去掉非 ASCII 的 filename 作为不认 filename* 时的回退。
func contentDisposition(name string) string {
	var ascii strings.Builder
	for _, r := range name {
		if r < 128 && r != '"' && r != '\\' {
			ascii.WriteRune(r)
		} else {
			ascii.WriteByte('_')
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii.String(), url.PathEscape(name))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func spaFallback(assets fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(assets))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if f, err := assets.Open(name); err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				if name == "index.html" {
					w.Header().Set("Cache-Control", "no-store")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		index, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}
}

// gateMiddleware 在闸门关闭时让所有写操作（非 GET 方法和启动路径）返回 503，读、登录和健康检查不受影响。
// 登录保持开放，运维仍能登录备机查看状态，登录后的写端点在这里被拒。
func gateMiddleware(gate *ha.Gate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
			if r.URL.Path == "/api/login" || strings.HasPrefix(r.URL.Path, "/internal/") {
				mutating = false
			}
			// /boot 本身要建克隆、导出 LUN，是写操作，只有写入者能做。下面两个是纯读：客户机开机后向承载它的
			// 节点要网关/DNS 和盘符，而集群里多数客户机的盘由备机供。一并当写挡掉的话，客户机有盘没网络配置，
			// 随后没有 iSCSI 会话、克隆被回收，界面一直显示离线。
			if r.URL.Path == "/boot" || r.URL.Path == "/boot/failed" {
				mutating = true
			}
			if !mutating {
				next.ServeHTTP(w, r)
				return
			}
			if err := gate.Allow(); err != nil {
				var closed *ha.ClosedError
				if errors.As(err, &closed) && closed.ActiveURL != "" {
					w.Header().Set("Location", closed.ActiveURL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// discoverResult 按其他列表的方式包装候选列表，控制台用同一套代码读取。
type discoverResult struct {
	Items []ha.Candidate `json:"items"`
	Total int            `json:"total"`
}

// adoptNodeHandler 纳管一个已发现的节点。对端的拒绝（不在出厂态）原样透传：解决办法是去那台自己的界面添加，
// 与「它没应答」不同。
func adoptNodeHandler(adopt func(ctx context.Context, addr string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Address string `json:"address"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || strings.TrimSpace(body.Address) == "" {
			http.Error(w, "请指明要纳管的节点地址", http.StatusBadRequest)
			return
		}
		if err := adopt(req.Context(), strings.TrimSpace(body.Address)); err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, ha.ErrNotAdoptable) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "adopting",
			"detail": "已受理：该节点将重启为备机加入集群，随后可在本控制台给它建数据池",
		})
	}
}

func standby(svc Services) bool { return svc.Gate != nil && svc.Gate.Allow() != nil }

// relayedByPeer：别的节点已经转发过这个请求，再转发可能在备机之间来回弹。
func relayedByPeer(r *http.Request, clusterToken string) bool {
	return clusterToken != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get(bootClusterHeader))), []byte(clusterToken)) == 1
}

// forwardBootRead 把只读 /boot 请求交给写入者并把答复原样写回；无处可转发时返回 false。
// 客户机自己的地址随请求带过去，写入者「只回答本机」的检查按客户机判断。
func forwardBootRead(w http.ResponseWriter, r *http.Request, svc Services, logger *slog.Logger) bool {
	if svc.BootReadProxy == nil {
		return false
	}
	client, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		client = r.RemoteAddr
	}
	status, body, ctype, err := svc.BootReadProxy(r.Context(), r.URL.Path, r.URL.RawQuery, client, r.Header.Get("Authorization"))
	if err != nil {
		logger.Warn("could not ask the writer for this client's boot configuration",
			"path", r.URL.Path, "client", client, "error", err)
		return false
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}
