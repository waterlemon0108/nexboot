package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrPoolNameRequired = errs.Invalid("存储池名称必填")
	ErrPoolDiskRequired = errs.Invalid("必须选择磁盘")
	ErrPoolExists       = errs.Conflict("存储池已存在")
	ErrPoolInUse        = errs.Conflict("存储池正在使用中")
)

type PoolService struct {
	Store   store.Store
	Storage storage.PoolAdmin
	Logger  *slog.Logger
	Now     func() time.Time
	Async   bool
	// Reload 在建好数据池后调用，让无池安装的节点绑定新池（持久化池名、重绑 zfs 客户端）。
	Reload func(ctx context.Context, pool string) error
	// Restart 在新数据池绑定后调用：复制和复制流在启动时读取池名，只重绑 zfs 客户端
	// 会让节点继续往旧池里收。
	Restart func()
	// Node 是本进程的服务器记录模板（身份加管理 IP、portal、API URL），ensureServer 用它写行。
	Node domain.Server
}

// nodeRow 返回要为 serverID 写入的服务器记录：本节点带完整坐标，其他节点只有身份。
func (s PoolService) nodeRow(serverID string) domain.Server {
	if serverID == s.Node.ID && s.Node.ID != "" {
		return s.Node
	}
	return domain.Server{ID: serverID, Name: serverID, IP: serverID, Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
}

type PoolRequest struct {
	Name  string   `json:"name"`
	Disks []string `json:"disks"`
	// Layout 为空表示 stripe，兼容没有该字段的旧请求。GroupWidth 是镜像路数（2 或 3），
	// 其他布局忽略。
	Layout     string `json:"layout"`
	GroupWidth int    `json:"group_width"`
	// Role 取 PoolRoleData 或 PoolRoleBackup，每台各最多一个；为空时取本机还缺的那种。
	Role string `json:"role"`
}

type PoolItem struct {
	ID              string         `json:"ID"`
	ServerID        string         `json:"ServerID"`
	Name            string         `json:"Name"`
	Disks           []string       `json:"Disks"`
	DiskItems       []PoolDiskItem `json:"DiskItems"`
	ReadCacheDisks  []string       `json:"ReadCacheDisks"`
	WriteCacheDisks []string       `json:"WriteCacheDisks"`
	Layout          string         `json:"Layout"`
	GroupWidth      int            `json:"GroupWidth"`
	// Groups 是实时读取的 vdev 树；池读不到时为 nil。
	Groups []PoolGroupItem `json:"Groups"`
	// RaidzExpandable 表示本机能否给 raidz 组单盘扩容；不能时 RaidzExpandNote 写明原因。
	RaidzExpandable bool   `json:"RaidzExpandable"`
	RaidzExpandNote string `json:"RaidzExpandNote"`
	Capacity        int64  `json:"Capacity"`
	Used            int64  `json:"Used"`
	Health          string `json:"Health"`
	Operation       string `json:"Operation"`
	Progress        string `json:"Progress"`
	// Role 是 "data"（镜像、配置、克隆所在）、"backup"（本机备份目标）或空（未分配）。
	// 只推导不存储：数据池看节点配置的池名，备份池看本机实际情况。
	Role string `json:"Role"`
}

// PoolRoleData、PoolRoleBackup 是 PoolItem.Role 的取值。
const (
	PoolRoleData   = "data"
	PoolRoleBackup = "backup"
)

type PoolDiskItem struct {
	Path   string `json:"Path"`
	Role   string `json:"Role"`
	Status string `json:"Status"`
	// Vdev 是所在分组（如 "mirror-0"）；裸盘是自身路径；热备盘为空。
	Vdev string `json:"Vdev"`
}

// PoolGroupItem 是 zpool status 中的一个顶层 vdev：mirror/raidz 组或单独的裸盘，Role 表示所在分区。
type PoolGroupItem struct {
	Name   string         `json:"Name"`
	Kind   string         `json:"Kind"`
	Role   string         `json:"Role"`
	Status string         `json:"Status"`
	Disks  []PoolDiskItem `json:"Disks"`
}

type PoolListResult struct {
	Items []PoolItem `json:"items"`
	Total int        `json:"total"`
}

type DiskListResult struct {
	Items []storage.DiskInfo `json:"items"`
	Total int                `json:"total"`
}

type PoolOperationResult struct {
	TaskID string   `json:"task_id"`
	Pool   PoolItem `json:"pool"`
}

type PoolTaskResult struct {
	TaskID string `json:"task_id"`
}

type PoolDiskRequest struct {
	Disks []string `json:"disks"`
	Disk  string   `json:"disk"`
	// Mode 为 "expand"（默认）时按池布局整组加 vdev；为 "attach" 时用 Disks 中的一块盘
	// 给存储盘 Target 做镜像。
	Mode   string `json:"mode"`
	Target string `json:"target"`
}

// MirrorPair 是 stripe 升级为 mirror 的一步：Disk 成为存储盘 Target 的镜像。
type MirrorPair struct {
	Target string `json:"target"`
	Disk   string `json:"disk"`
}

type MirrorUpgradeRequest struct {
	Pairs []MirrorPair `json:"pairs"`
}

type PoolReplaceDiskRequest struct {
	OldDisk string `json:"old_disk"`
	NewDisk string `json:"new_disk"`
}

type PoolReadCacheRequest struct {
	Disks []string `json:"disks"`
	Disk  string   `json:"disk"`
}

type PoolWriteCacheRequest struct {
	Disks []string `json:"disks"`
	Disk  string   `json:"disk"`
}

func (s PoolService) List(ctx context.Context, serverID string) (PoolListResult, error) {
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return PoolListResult{}, err
	}
	backupPool := s.backupPoolName(ctx)
	items := make([]PoolItem, 0, len(pools))
	for _, pool := range pools {
		if serverID != "" && pool.ServerID != serverID {
			continue
		}
		// 读不到的池标为未知而不是让整个列表报错：正常的池和坏池本身都要留在页面上供操作。
		item, err := s.poolItem(ctx, pool)
		switch {
		case zfs.IsNotExist(err):
			item = missingPoolItem(pool)
		case err != nil:
			if s.Logger != nil {
				s.Logger.Warn("pool status unreadable", "pool", pool.Name, "error", err)
			}
			item = poolItem(pool, domain.PoolStatus{Name: pool.Name, Health: poolHealthUnknown})
		}
		item.Role = s.poolRole(pool.Name, backupPool)
		items = append(items, item)
	}
	return PoolListResult{Items: items, Total: len(items)}, nil
}

func (s PoolService) ListDisks(ctx context.Context) (DiskListResult, error) {
	items, err := s.Storage.ListDisks(ctx)
	if err != nil {
		return DiskListResult{}, err
	}
	if items == nil {
		items = []storage.DiskInfo{}
	}
	return DiskListResult{Items: items, Total: len(items)}, nil
}

func (s PoolService) Get(ctx context.Context, id string) (PoolItem, error) {
	pool, err := s.Store.Pools().Get(ctx, id)
	if err != nil {
		return PoolItem{}, err
	}
	item, err := s.poolItem(ctx, pool)
	switch {
	case zfs.IsNotExist(err):
		item = missingPoolItem(pool)
	case err != nil:
		return PoolItem{}, err
	}
	item.Role = s.poolRole(pool.Name, s.backupPoolName(ctx))
	return item, nil
}

// backupPoolName 返回本机的备份池：每台最多两个池，非数据池的那个就是，由各节点自己回答，
// 不从备份配置里读（那样在别的节点上会标错）。读不到时退化为不打标签，不拖垮池列表。
func (s PoolService) backupPoolName(ctx context.Context) string {
	if s.Storage == nil {
		return ""
	}
	name, err := s.Storage.BackupPoolName(ctx)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("backup pool unreadable; pool roles shown without the backup label", "error", err)
		}
		return ""
	}
	return strings.TrimSpace(name)
}

func (s PoolService) poolRole(name, backupPool string) string {
	if s.Storage != nil && name == s.Storage.DataPoolName() {
		return PoolRoleData
	}
	if backupPool != "" && name == backupPool {
		return PoolRoleBackup
	}
	return ""
}

func (s PoolService) Create(ctx context.Context, req PoolRequest) (PoolOperationResult, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return PoolOperationResult{}, ErrPoolNameRequired
	}
	if err := validatePoolName(req.Name); err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) == 0 {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	spec, err := poolSpecFor(req.Layout, req.GroupWidth, disks)
	if err != nil {
		return PoolOperationResult{}, err
	}
	if err := s.ensurePoolNameUnique(ctx, req.Name); err != nil {
		return PoolOperationResult{}, err
	}
	role, err := s.resolveCreateRole(ctx, req.Name, strings.TrimSpace(req.Role))
	if err != nil {
		return PoolOperationResult{}, err
	}
	if err := s.ensurePoolCountBelowLimit(ctx, req.Name); err != nil {
		return PoolOperationResult{}, err
	}

	var item PoolItem
	task, err := s.runner().Run(ctx, domain.TaskTypeCreatePool, req.Name, func(ctx context.Context, task domain.Task) error {
		var err error
		item, err = s.executeCreate(ctx, task, req.Name, spec, role)
		return err
	})
	if err != nil {
		return PoolOperationResult{}, err
	}
	if s.Async {
		return PoolOperationResult{TaskID: task.ID, Pool: PoolItem{Name: req.Name, Disks: disks, Layout: string(spec.Layout), GroupWidth: spec.GroupWidth, Health: string(domain.TaskStatusPending)}}, nil
	}
	return PoolOperationResult{TaskID: task.ID, Pool: item}, nil
}

// validatePoolName 提前按 zpool 的命名规则拒绝：vdev 关键字开头（"mirrors" 会报
// "name is reserved"）、c 加数字开头（Solaris 设备名）、非字母开头、[A-Za-z0-9_.:-] 以外的字符。
func validatePoolName(name string) error {
	if strings.ContainsAny(name, " \t") {
		return errs.Invalid("存储池名称不能含空格")
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return errs.Invalid("存储池名称要以字母开头，例如 tank、data-pool")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return errs.Invalid(fmt.Sprintf("存储池名称只能包含字母、数字和 _ - . :，不能有「%c」", r))
	}
	lower := strings.ToLower(name)
	for _, reserved := range []string{"mirror", "raidz", "draid", "spare"} {
		if strings.HasPrefix(lower, reserved) {
			return errs.Invalid(fmt.Sprintf("%s 是 ZFS 保留名（mirror / raidz / draid / spare 开头及 log 都不能作池名），请换一个，例如 tank、data", name))
		}
	}
	if lower == "log" {
		return errs.Invalid("log 是 ZFS 保留名（mirror / raidz / draid / spare 开头及 log 都不能作池名），请换一个")
	}
	if len(name) >= 2 && lower[0] == 'c' && lower[1] >= '0' && lower[1] <= '9' {
		return errs.Invalid(fmt.Sprintf("%s 不能作池名：ZFS 不允许 c 加数字开头的名字（会被当成设备名）", name))
	}
	if len(name) > 255 {
		return errs.Invalid("存储池名称过长")
	}
	return nil
}

// poolSpecFor 在提交时拦下 zpool 会在异步任务中途拒绝的组合，并说明还差几块盘。
// 布局建池后无法再改。
func poolSpecFor(layout string, groupWidth int, disks []string) (storage.PoolSpec, error) {
	l := domain.PoolLayout(strings.ToLower(strings.TrimSpace(layout)))
	if l == "" {
		l = domain.PoolLayoutStripe
	}
	if !l.Valid() {
		return storage.PoolSpec{}, errs.Invalid("不支持的存储池布局 " + layout + "，可选：stripe、mirror、raidz1、raidz2、raidz3")
	}
	switch l {
	case domain.PoolLayoutStripe:
		return storage.PoolSpec{Layout: l, GroupWidth: 1, Disks: disks}, nil
	case domain.PoolLayoutMirror:
		width := groupWidth
		if width == 0 {
			width = 2
		}
		if width != 2 && width != 3 {
			return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("镜像只支持 2 路或 3 路，不支持 %d 路", width))
		}
		if len(disks) < width || len(disks)%width != 0 {
			missing := width - len(disks)%width
			return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("镜像池按 %d 块一组，请再选 %d 块（已选 %d 块）", width, missing, len(disks)))
		}
		return storage.PoolSpec{Layout: l, GroupWidth: width, Disks: disks}, nil
	default: // raidz1/2/3：所有盘组成一组
		minDisks := l.RaidzParity() + 2
		if len(disks) < minDisks {
			return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("%s 至少 %d 块盘，请再选 %d 块（已选 %d 块）", l, minDisks, minDisks-len(disks), len(disks)))
		}
		return storage.PoolSpec{Layout: l, GroupWidth: len(disks), Disks: disks}, nil
	}
}

// resolveCreateRole 按本机已有的池（数据池、备份池各最多一个）校验请求的用途，未指定时自动补上。
func (s PoolService) resolveCreateRole(ctx context.Context, name, role string) (string, error) {
	if s.Storage == nil {
		return role, nil
	}
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return "", err
	}
	dataPool := strings.TrimSpace(s.Storage.DataPoolName())
	hasData, hasBackup := "", ""
	for _, p := range pools {
		if p.ServerID != s.Storage.ServerID() {
			continue
		}
		if p.Name == dataPool {
			hasData = p.Name
		} else if hasBackup == "" {
			hasBackup = p.Name
		}
	}
	if hasData == "" && s.hasUsableDataPool(ctx) {
		hasData = dataPool // 盘上在用、只是还没有记录
	}
	if role == "" {
		role = PoolRoleData
		if hasData != "" {
			role = PoolRoleBackup
		}
	}
	if hasData != "" && hasBackup != "" {
		return "", errs.Invalid(fmt.Sprintf("每台最多一个数据池和一个备份池，这台已有数据池 %s、备份池 %s，不能再创建 %s。如需更多容量，请给数据池添加磁盘", hasData, hasBackup, name))
	}
	switch role {
	case PoolRoleData:
		if hasData != "" {
			return "", errs.Conflict(fmt.Sprintf("这台已有数据池 %s，只能再建备份池", hasData))
		}
	case PoolRoleBackup:
		if hasBackup != "" {
			return "", errs.Conflict(fmt.Sprintf("这台已有备份池 %s，只能再建数据池", hasBackup))
		}
		if name == dataPool {
			return "", errs.Invalid(fmt.Sprintf("%s 是这台数据池的名字，备份池请换一个名字", name))
		}
	default:
		return "", errs.Invalid("请选择存储池类型：数据池或备份池")
	}
	return role, nil
}

func (s PoolService) executeCreate(ctx context.Context, task domain.Task, name string, spec storage.PoolSpec, role string) (PoolItem, error) {
	pool, err := s.Storage.CreatePool(ctx, name, spec)
	if err != nil {
		return PoolItem{}, err
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		_ = s.Storage.DestroyPool(context.Background(), pool.Name)
		return PoolItem{}, err
	}
	pool.Capacity = status.Capacity
	pool.Used = status.Used

	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := ensureServer(ctx, tx, s.nodeRow(pool.ServerID)); err != nil {
			return err
		}
		if err := tx.Pools().Create(ctx, pool); err != nil {
			return err
		}
		disks := status.Disks
		if len(disks) == 0 {
			disks = dataDiskStatuses(pool.Disks, "")
		}
		if err := s.replacePoolDisks(ctx, tx, pool.ID, disks); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, pool.ID)
	}); err != nil {
		cleanupErr := s.Storage.DestroyPool(context.Background(), pool.Name)
		return PoolItem{}, errors.Join(err, cleanupErr)
	}
	// 绑定新池后无池节点无需重启即可使用。Reload 失败则任务失败：进程看不到的“已建”池
	// 会让后续导入都找不到池。只有数据池绑定：备份池若被绑定，导入会落到备份盘，
	// 入盟时也会把错误的池名交给集群。
	if s.Reload != nil && role == PoolRoleData {
		if err := s.Reload(ctx, pool.Name); err != nil {
			return PoolItem{}, err
		}
		if s.Restart != nil {
			s.Restart()
		}
	}
	return poolItem(pool, status), nil
}

// hasUsableDataPool 判断进程绑定的数据池在本机是否真实存在；无池安装时配置的默认池名并不存在。
func (s PoolService) hasUsableDataPool(ctx context.Context) bool {
	name := strings.TrimSpace(s.Storage.DataPoolName())
	if name == "" {
		return false
	}
	_, err := s.Storage.PoolStatus(ctx, name)
	return err == nil
}

func (s PoolService) AddDisk(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) == 0 {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	if strings.EqualFold(strings.TrimSpace(req.Mode), "attach") {
		target := strings.TrimSpace(req.Target)
		if pool.Layout.RaidzParity() > 0 {
			// raidz 成员不能加镜像，但本机支持时组可以单盘扩容。
			if err := s.checkRaidzExpand(ctx, pool, target, disks); err != nil {
				return PoolOperationResult{}, err
			}
		} else if err := s.checkAttach(ctx, pool, target, disks); err != nil {
			return PoolOperationResult{}, err
		}
		return s.runPoolOp(ctx, domain.TaskTypeAddDisk, pool, func(ctx context.Context) error {
			return s.Storage.AttachDisk(ctx, pool.Name, target, disks[0])
		})
	}
	spec, err := addSpecFor(pool, disks)
	if err != nil {
		return PoolOperationResult{}, err
	}
	return s.runPoolOp(ctx, domain.TaskTypeAddDisk, pool, func(ctx context.Context) error {
		return s.Storage.AddDisk(ctx, pool.Name, spec)
	})
}

// MirrorUpgrade 依次给每块存储盘 attach 一块镜像盘，把 stripe 池升级为 mirror，在线进行，
// 后台 resilver。首次 attach 前先全部校验；中途失败即停，已加的镜像保留，任务信息点名失败的那对。
func (s PoolService) MirrorUpgrade(ctx context.Context, id string, req MirrorUpgradeRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	pairs, err := s.checkMirrorPairs(ctx, pool, req.Pairs)
	if err != nil {
		return PoolOperationResult{}, err
	}
	var item PoolItem
	task, err := s.runner().Run(ctx, domain.TaskTypeMirrorUpgrade, pool.ID, func(ctx context.Context, task domain.Task) error {
		for i, p := range pairs {
			t := task
			t.Status = domain.TaskStatusRunning
			t.Progress = i * 100 / len(pairs)
			t.Message = fmt.Sprintf("第 %d/%d 对：%s → %s", i+1, len(pairs), p.Target, p.Disk)
			_ = s.Store.Tasks().Update(ctx, t)
			if err := s.Storage.AttachDisk(ctx, pool.Name, p.Target, p.Disk); err != nil {
				return fmt.Errorf("第 %d 对（%s → %s）失败：%s；已完成 %d 对，已加的镜像盘保留，处理后可对剩余的盘再次升级", i+1, p.Target, p.Disk, zfs.OperatorMessage(err), i)
			}
		}
		var err error
		item, err = s.refreshPool(ctx, task, pool)
		return err
	})
	if err != nil {
		return PoolOperationResult{TaskID: task.ID}, err
	}
	if s.Async {
		return PoolOperationResult{TaskID: task.ID, Pool: poolItem(pool, domain.PoolStatus{Health: string(domain.TaskStatusPending)})}, nil
	}
	return PoolOperationResult{TaskID: task.ID, Pool: item}, nil
}

func (s PoolService) checkMirrorPairs(ctx context.Context, pool domain.Pool, in []MirrorPair) ([]MirrorPair, error) {
	if pool.Layout.RaidzParity() > 0 {
		return nil, errs.Conflict(fmt.Sprintf("%s 是 %s 布局，raidz 没有可加镜像的裸盘", pool.Name, pool.Layout))
	}
	pairs := make([]MirrorPair, 0, len(in))
	for _, p := range in {
		p.Target, p.Disk = strings.TrimSpace(p.Target), strings.TrimSpace(p.Disk)
		if p.Target == "" || p.Disk == "" {
			continue
		}
		pairs = append(pairs, p)
	}
	if len(pairs) == 0 {
		return nil, errs.Invalid("请为每块存储盘选一块镜像盘")
	}
	isData := func(path string) bool {
		for _, d := range pool.Disks {
			if storage.SameDisk(d, path) {
				return true
			}
		}
		return false
	}
	seenTarget, seenDisk := map[string]bool{}, map[string]bool{}
	for _, p := range pairs {
		if !isData(p.Target) {
			return nil, errs.Invalid(fmt.Sprintf("%s 不是 %s 的存储盘", p.Target, pool.Name))
		}
		if isData(p.Disk) {
			return nil, errs.Invalid(fmt.Sprintf("%s 已经在 %s 里，不能再作镜像盘", p.Disk, pool.Name))
		}
		if seenTarget[p.Target] {
			return nil, errs.Invalid(fmt.Sprintf("%s 被选了两次，一块盘只加一块镜像盘", p.Target))
		}
		if seenDisk[p.Disk] {
			return nil, errs.Invalid(fmt.Sprintf("%s 被选了两次，一块空闲盘只能给一块盘做镜像", p.Disk))
		}
		seenTarget[p.Target], seenDisk[p.Disk] = true, true
	}
	if all, err := s.Storage.ListDisks(ctx); err == nil {
		for _, p := range pairs {
			ts, ds := diskSizeOf(all, p.Target), diskSizeOf(all, p.Disk)
			if ts > 0 && ds > 0 && ds < ts {
				return nil, errs.Invalid(fmt.Sprintf("%s（%s）比 %s（%s）小，镜像盘不能比原盘小", p.Disk, fmtBytes(ds), p.Target, fmtBytes(ts)))
			}
		}
	}
	return pairs, nil
}

// addSpecFor 按池自身布局把加盘请求组成整组 vdev；镜像池单加一块会成为无镜像的 vdev，
// 在这里提前拒绝并说明还差几块。
func addSpecFor(pool domain.Pool, disks []string) (storage.PoolSpec, error) {
	layout := pool.Layout
	if layout == "" {
		layout = domain.PoolLayoutStripe
	}
	width := pool.GroupWidth
	if width < 1 {
		width = 1
	}
	switch {
	case layout == domain.PoolLayoutStripe:
		return storage.PoolSpec{Layout: layout, GroupWidth: 1, Disks: disks}, nil
	case layout == domain.PoolLayoutMirror:
		if len(disks) < width || len(disks)%width != 0 {
			return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("镜像池按 %d 块一组加盘，请再选 %d 块（已选 %d 块）", width, width-len(disks)%width, len(disks)))
		}
		return storage.PoolSpec{Layout: layout, GroupWidth: width, Disks: disks}, nil
	default: // raidz：按同宽度整组加；单盘扩容走 attach
		if len(disks) != width {
			missing := width - len(disks)
			if missing < 0 {
				return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("%s 池按整组加盘，每组 %d 块，已选 %d 块，请去掉 %d 块", layout, width, len(disks), -missing))
			}
			return storage.PoolSpec{}, errs.Invalid(fmt.Sprintf("%s 池按整组加盘，每组 %d 块，请再选 %d 块（已选 %d 块）", layout, width, missing, len(disks)))
		}
		return storage.PoolSpec{Layout: layout, GroupWidth: width, Disks: disks}, nil
	}
}

// checkRaidzExpand 只允许 raidz 池的 attach 作为单盘扩容：目标必须是 raidz 组本身、只加一块盘、
// 本机支持扩容，否则点名缺的条件。
func (s PoolService) checkRaidzExpand(ctx context.Context, pool domain.Pool, target string, disks []string) error {
	if len(disks) != 1 {
		return errs.Invalid(fmt.Sprintf("raidz 扩容一次只能加一块盘，已选 %d 块", len(disks)))
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return err
	}
	if !status.RaidzExpandable {
		note := status.RaidzExpandNote
		if note == "" {
			note = "本机不支持 raidz 单盘扩容"
		}
		return errs.Conflict(fmt.Sprintf("%s 是 %s 布局，raidz 无法追加校验盘；raidz 单盘扩容在本机也不可用：%s。坏盘请用换盘", pool.Name, pool.Layout, note))
	}
	for _, v := range status.Vdevs {
		if v.Role == domain.PoolDiskRoleData && strings.HasPrefix(v.Kind, "raidz") && v.Name == target {
			return nil
		}
	}
	return errs.Invalid(fmt.Sprintf("raidz 扩容要以组为目标（如 raidz2-0），%s 不是 %s 的 raidz 组", target, pool.Name))
}

// AddSpecial 添加 special（元数据）vdev，必须 2～3 路镜像：它丢了整个池就丢。
func (s PoolService) AddSpecial(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) < 2 || len(disks) > 3 {
		return PoolOperationResult{}, errs.Invalid(fmt.Sprintf("元数据盘必须镜像，请选 2 或 3 块（已选 %d 块）：它丢了整个池就丢", len(disks)))
	}
	return s.runPoolOp(ctx, domain.TaskTypeAddSpecial, pool, func(ctx context.Context) error {
		return s.Storage.AddSpecial(ctx, pool.Name, disks)
	})
}

// RemoveSpecial 按 zpool 组名整组移除 special vdev，元数据迁回数据 vdev。
func (s PoolService) RemoveSpecial(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	group := strings.TrimSpace(req.Disk)
	if group == "" && len(req.Disks) > 0 {
		group = strings.TrimSpace(req.Disks[0])
	}
	if group == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return PoolOperationResult{}, err
	}
	found := false
	for _, v := range status.Vdevs {
		if v.Role == domain.PoolDiskRoleSpecial && v.Name == group {
			found = true
			break
		}
	}
	if !found {
		return PoolOperationResult{}, errs.Invalid(fmt.Sprintf("%s 不是 %s 的元数据盘组，元数据盘要按整组移除（如 mirror-3）", group, pool.Name))
	}
	return s.runPoolOp(ctx, domain.TaskTypeRemoveSpecial, pool, func(ctx context.Context) error {
		return s.Storage.RemoveSpecial(ctx, pool.Name, group)
	})
}

// AddSpare 添加热备盘，池会自动启用它们。
func (s PoolService) AddSpare(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) == 0 {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeAddSpare, pool, func(ctx context.Context) error {
		return s.Storage.AddSpare(ctx, pool.Name, disks)
	})
}

func (s PoolService) RemoveSpare(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disk := strings.TrimSpace(req.Disk)
	if disk == "" && len(req.Disks) > 0 {
		disk = strings.TrimSpace(req.Disks[0])
	}
	if disk == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return PoolOperationResult{}, err
	}
	found := false
	for _, v := range status.Vdevs {
		if v.Role == domain.PoolDiskRoleSpare && storage.SameDisk(v.Name, disk) {
			found = true
			break
		}
	}
	if !found {
		return PoolOperationResult{}, errs.Invalid(fmt.Sprintf("%s 不是 %s 的热备盘", disk, pool.Name))
	}
	return s.runPoolOp(ctx, domain.TaskTypeRemoveSpare, pool, func(ctx context.Context) error {
		return s.Storage.RemoveSpare(ctx, pool.Name, disk)
	})
}

// checkAttach 在提交时拦下 zpool attach 之后会失败的情况：无目标、多于一块新盘、目标不是存储盘、
// raidz 布局、新盘比原盘小。
func (s PoolService) checkAttach(ctx context.Context, pool domain.Pool, target string, disks []string) error {
	if target == "" {
		return errs.Invalid("请指定要加镜像的盘（target）")
	}
	if len(disks) != 1 {
		return errs.Invalid(fmt.Sprintf("一次只能给一块盘加一块镜像盘，已选 %d 块", len(disks)))
	}
	if pool.Layout.RaidzParity() > 0 {
		return errs.Conflict(fmt.Sprintf("%s 是 %s 布局，raidz 无法追加校验盘；坏盘请用换盘", pool.Name, pool.Layout))
	}
	isData := false
	for _, d := range pool.Disks {
		if storage.SameDisk(d, target) {
			isData = true
			break
		}
	}
	if !isData {
		return errs.Invalid(fmt.Sprintf("%s 不是 %s 的存储盘，只有存储盘可以加镜像", target, pool.Name))
	}
	if storage.SameDisk(disks[0], target) {
		return errs.Invalid("镜像盘不能是原盘自己")
	}
	// 容量来自 lsblk 仅作提前提示；未知时不拒绝，zpool 会真正校验。
	if all, err := s.Storage.ListDisks(ctx); err == nil {
		targetSize, newSize := diskSizeOf(all, target), diskSizeOf(all, disks[0])
		if targetSize > 0 && newSize > 0 && newSize < targetSize {
			return errs.Invalid(fmt.Sprintf("%s（%s）比 %s（%s）小，镜像盘不能比原盘小", disks[0], fmtBytes(newSize), target, fmtBytes(targetSize)))
		}
	}
	return nil
}

// diskSizeOf 按磁盘的任一别名查容量，未知返回 0。
func diskSizeOf(all []storage.DiskInfo, path string) int64 {
	for _, d := range all {
		if storage.SameDisk(d.Path, path) {
			return d.Size
		}
	}
	return 0
}

func fmtBytes(b int64) string {
	const gib = 1 << 30
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}

func (s PoolService) executePoolRefresh(ctx context.Context, task domain.Task, pool domain.Pool, op func(context.Context) error) (PoolItem, error) {
	if err := op(ctx); err != nil {
		return PoolItem{}, err
	}
	return s.refreshPool(ctx, task, pool)
}

// RemoveDisk 从池中移出一块盘或整组（按 zpool 组名），方式由池结构决定：stripe 成员或整组
// 迁走数据后 remove；mirror 组成员 detach；raidz 池不能移盘。
func (s PoolService) RemoveDisk(ctx context.Context, id string, req PoolDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disk := strings.TrimSpace(req.Disk)
	if disk == "" && len(req.Disks) > 0 {
		disk = strings.TrimSpace(req.Disks[0])
	}
	if disk == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return PoolOperationResult{}, err
	}
	kind, err := removalFor(pool, status, disk)
	if err != nil {
		return PoolOperationResult{}, err
	}
	if kind == removalDetach {
		return s.runPoolOp(ctx, domain.TaskTypeDetachDisk, pool, func(ctx context.Context) error {
			return s.Storage.DetachDisk(ctx, pool.Name, disk)
		})
	}
	return s.runPoolOp(ctx, domain.TaskTypeMigrateDisk, pool, func(ctx context.Context) error {
		return s.Storage.RemoveDisk(ctx, pool.Name, disk)
	})
}

type removalKind int

const (
	removalRemove removalKind = iota // 顶层 vdev：zpool remove，数据迁走
	removalDetach                    // mirror 组的一路：zpool detach
)

// removalFor 决定 disk 如何移出或为何不能。含 raidz 数据组的池不能移除任何设备（ZFS 禁用
// device removal，raidz 成员也不能 detach）。读不到 vdev 树时按 stripe 成员处理。
func removalFor(pool domain.Pool, status domain.PoolStatus, disk string) (removalKind, error) {
	if pool.Layout.RaidzParity() > 0 {
		return 0, errs.Conflict(fmt.Sprintf("%s 是 %s 布局，raidz 不能移盘；坏盘请用换盘", pool.Name, pool.Layout))
	}
	if len(status.Vdevs) == 0 {
		return removalRemove, nil
	}
	for _, v := range status.Vdevs {
		if v.Role == domain.PoolDiskRoleData && strings.HasPrefix(v.Kind, "raidz") {
			return 0, errs.Conflict(fmt.Sprintf("%s 含有 raidz 组 %s，含 raidz 的池不能移盘；坏盘请用换盘", pool.Name, v.Name))
		}
	}
	for _, v := range status.Vdevs {
		if v.Role != domain.PoolDiskRoleData {
			continue
		}
		if v.Name == disk || (v.Kind == "disk" && storage.SameDisk(v.Name, disk)) {
			return removalRemove, nil // 裸盘，或按名字指定的整组
		}
		for _, d := range v.Disks {
			if storage.SameDisk(d.Path, disk) {
				if v.Kind == "mirror" && len(v.Disks) > 1 {
					return removalDetach, nil
				}
				return removalRemove, nil
			}
		}
	}
	return 0, errs.Invalid(fmt.Sprintf("%s 不是 %s 的存储盘或分组，无法移除", disk, pool.Name))
}

func (s PoolService) ReplaceDisk(ctx context.Context, id string, req PoolReplaceDiskRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	oldDisk := strings.TrimSpace(req.OldDisk)
	newDisk := strings.TrimSpace(req.NewDisk)
	if oldDisk == "" || newDisk == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeReplaceDisk, pool, func(ctx context.Context) error {
		return s.Storage.ReplaceDisk(ctx, pool.Name, oldDisk, newDisk)
	})
}

func (s PoolService) AddReadCache(ctx context.Context, id string, req PoolReadCacheRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) == 0 {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeAddReadCache, pool, func(ctx context.Context) error {
		return s.Storage.AddReadCache(ctx, pool.Name, disks)
	})
}

func (s PoolService) RemoveReadCache(ctx context.Context, id string, req PoolReadCacheRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disk := strings.TrimSpace(req.Disk)
	if disk == "" && len(req.Disks) > 0 {
		disk = strings.TrimSpace(req.Disks[0])
	}
	if disk == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeRemoveReadCache, pool, func(ctx context.Context) error {
		return s.Storage.RemoveReadCache(ctx, pool.Name, disk)
	})
}

func (s PoolService) AddWriteCache(ctx context.Context, id string, req PoolWriteCacheRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disks := cleanStrings(req.Disks)
	if len(disks) == 0 {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeAddWriteCache, pool, func(ctx context.Context) error {
		return s.Storage.AddWriteCache(ctx, pool.Name, disks)
	})
}

func (s PoolService) RemoveWriteCache(ctx context.Context, id string, req PoolWriteCacheRequest) (PoolOperationResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolOperationResult{}, err
	}
	disk := strings.TrimSpace(req.Disk)
	if disk == "" && len(req.Disks) > 0 {
		disk = strings.TrimSpace(req.Disks[0])
	}
	if disk == "" {
		return PoolOperationResult{}, ErrPoolDiskRequired
	}
	return s.runPoolOp(ctx, domain.TaskTypeRemoveWriteCache, pool, func(ctx context.Context) error {
		return s.Storage.RemoveWriteCache(ctx, pool.Name, disk)
	})
}

func (s PoolService) FlushWriteCache(ctx context.Context, id string) (PoolTaskResult, error) {
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolTaskResult{}, err
	}
	task, err := s.runner().Run(ctx, domain.TaskTypeFlushCache, id, func(ctx context.Context, task domain.Task) error {
		return s.executeFlushWriteCache(ctx, task, pool)
	})
	if err != nil {
		return PoolTaskResult{}, err
	}
	return PoolTaskResult{TaskID: task.ID}, nil
}

func (s PoolService) executeFlushWriteCache(ctx context.Context, task domain.Task, pool domain.Pool) error {
	if err := s.Storage.FlushWriteCache(ctx, pool.Name); err != nil {
		return err
	}
	return s.Store.Tx(ctx, func(tx store.Store) error {
		return s.runner().Finish(ctx, tx, task, pool.ID)
	})
}

// localPool 只返回本节点可写的池。下面的写操作都按池名作用于本机存储，别的节点的记录会误伤
// 本机同名池（比如销毁掉本机的备份池）。别的节点的池走 /api/cluster/nodes/{node}/pools。
func (s PoolService) localPool(ctx context.Context, id string) (domain.Pool, error) {
	pool, err := s.Store.Pools().Get(ctx, id)
	if err != nil {
		return pool, err
	}
	self := ""
	if s.Storage != nil {
		self = s.Storage.ServerID()
	}
	if pool.ServerID == "" || self == "" || pool.ServerID == self {
		return pool, nil
	}
	where := pool.ServerID
	if srv, err := s.Store.Servers().Get(ctx, pool.ServerID); err == nil && srv.IP != "" {
		where = srv.IP
	}
	return domain.Pool{}, errs.Conflict(fmt.Sprintf("存储池 %s 在节点 %s 上，不在本机：请在「存储池管理」里对那台节点操作",
		pool.Name, where))
}

// ownerGone 判断池属于另一台已停止心跳（或已不在名册中）的节点。
func (s PoolService) ownerGone(ctx context.Context, pool domain.Pool) bool {
	if pool.ServerID == "" || s.Storage == nil || s.Storage.ServerID() == "" || pool.ServerID == s.Storage.ServerID() {
		return false
	}
	srv, err := s.Store.Servers().Get(ctx, pool.ServerID)
	if err != nil {
		return errs.IsNotFound(err)
	}
	return srv.LastSeenAt == nil || time.Since(*srv.LastSeenAt) > place.Freshness
}

func (s PoolService) Destroy(ctx context.Context, id string) (PoolTaskResult, error) {
	if pool, err := s.Store.Pools().Get(ctx, id); err == nil && s.ownerGone(ctx, pool) {
		// 节点已经不在：它的池只能在这里清记录，否则「移出集群」永远卡在
		// 「名下还有存储池」。不碰任何磁盘；节点回来时心跳会把池重新报上来。
		task, err := s.runner().Run(ctx, domain.TaskTypeDestroyPool, id, func(ctx context.Context, task domain.Task) error {
			return s.dropPoolRecord(ctx, task, pool)
		})
		if err != nil {
			return PoolTaskResult{}, err
		}
		return PoolTaskResult{TaskID: task.ID}, nil
	}
	pool, err := s.localPool(ctx, id)
	if err != nil {
		return PoolTaskResult{}, err
	}
	if err := s.ensurePoolNotInUse(ctx, pool); err != nil {
		return PoolTaskResult{}, err
	}
	task, err := s.runner().Run(ctx, domain.TaskTypeDestroyPool, id, func(ctx context.Context, task domain.Task) error {
		return s.executeDestroy(ctx, task, pool)
	})
	if err != nil {
		return PoolTaskResult{}, err
	}
	return PoolTaskResult{TaskID: task.ID}, nil
}

func (s PoolService) executeDestroy(ctx context.Context, task domain.Task, pool domain.Pool) error {
	// 池已不存在就是目标状态：记录可能比池活得久（建池失败、拔盘、手动 export），
	// 这里报错会留下一行永远删不掉的记录。
	if err := s.Storage.DestroyPool(ctx, pool.Name); err != nil && !zfs.IsNotExist(err) {
		return err
	}
	return s.dropPoolRecord(ctx, task, pool)
}

func (s PoolService) dropPoolRecord(ctx context.Context, task domain.Task, pool domain.Pool) error {
	return s.Store.Tx(ctx, func(tx store.Store) error {
		disks, err := tx.PoolDisks().List(ctx)
		if err != nil {
			return err
		}
		for _, disk := range disks {
			if disk.PoolID == pool.ID {
				if err := tx.PoolDisks().Delete(ctx, disk.ID); err != nil {
					return err
				}
			}
		}
		if err := tx.Pools().Delete(ctx, pool.ID); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, pool.ID)
	})
}

// poolHealthUnknown 表示节点完全读不到池，区别于 ZFS 自己的 DEGRADED/FAULTED（池有应答）。
const poolHealthUnknown = "UNKNOWN"

// PoolHealthMissing 表示节点明确回答没有这个池，只剩记录。与 UNKNOWN 不同，这不是猜测，
// 没有东西需要保护或计入容量。
const PoolHealthMissing = "MISSING"

func (s PoolService) poolItem(ctx context.Context, pool domain.Pool) (PoolItem, error) {
	// 别的节点的池常与本机同名，按池名读本机会把本机状态写进它的记录并随复制扩散；实时状态走 ClusterPools。
	if self := s.Storage.ServerID(); pool.ServerID != "" && self != "" && pool.ServerID != self {
		return poolItem(pool, domain.PoolStatus{Name: pool.Name, Health: poolHealthUnknown}), nil
	}
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return PoolItem{}, err
	}
	if status.Capacity != pool.Capacity || status.Used != pool.Used || len(status.Disks) > 0 ||
		(status.Layout != "" && (status.Layout != pool.Layout || status.GroupWidth != pool.GroupWidth)) {
		pool = applyPoolStatus(pool, status)
		if err := s.Store.Pools().Update(ctx, pool); err != nil {
			return PoolItem{}, err
		}
	}
	return poolItem(pool, status), nil
}

// missingPoolItem 用于池已不存在的记录，清零容量以免计入不存在的空间。
func missingPoolItem(pool domain.Pool) PoolItem {
	pool.Capacity, pool.Used = 0, 0
	return poolItem(pool, domain.PoolStatus{Name: pool.Name, Health: PoolHealthMissing})
}

func poolItem(pool domain.Pool, status domain.PoolStatus) PoolItem {
	var diskItems []PoolDiskItem
	if len(status.Disks) > 0 {
		diskItems = poolDiskItems(status.Disks)
	}
	var groups []PoolGroupItem
	for _, v := range status.Vdevs {
		groups = append(groups, PoolGroupItem{Name: v.Name, Kind: v.Kind, Role: string(v.Role), Status: v.Status, Disks: poolDiskItems(v.Disks)})
	}
	return PoolItem{
		ID:              pool.ID,
		ServerID:        pool.ServerID,
		Name:            pool.Name,
		Disks:           pool.Disks,
		DiskItems:       diskItems,
		ReadCacheDisks:  pool.ReadCacheDisks,
		WriteCacheDisks: pool.WriteCacheDisks,
		Layout:          string(pool.Layout),
		GroupWidth:      pool.GroupWidth,
		Groups:          groups,
		RaidzExpandable: status.RaidzExpandable,
		RaidzExpandNote: status.RaidzExpandNote,
		Capacity:        pool.Capacity,
		Used:            pool.Used,
		Health:          status.Health,
		Operation:       status.Operation,
		Progress:        status.Progress,
	}
}

func (s PoolService) refreshPool(ctx context.Context, task domain.Task, pool domain.Pool) (PoolItem, error) {
	status, err := s.Storage.PoolStatus(ctx, pool.Name)
	if err != nil {
		return PoolItem{}, err
	}
	pool = applyPoolStatus(pool, status)
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Pools().Update(ctx, pool); err != nil {
			return err
		}
		if len(status.Disks) > 0 {
			if err := s.replacePoolDisks(ctx, tx, pool.ID, status.Disks); err != nil {
				return err
			}
		}
		return s.runner().Finish(ctx, tx, task, pool.ID)
	}); err != nil {
		return PoolItem{}, err
	}
	return poolItem(pool, status), nil
}

func (s PoolService) replacePoolDisks(ctx context.Context, st store.Store, poolID string, disks []domain.PoolDiskStatus) error {
	return replacePoolDisksIn(ctx, st, poolID, disks)
}

func replacePoolDisksIn(ctx context.Context, st store.Store, poolID string, disks []domain.PoolDiskStatus) error {
	existing, err := st.PoolDisks().List(ctx)
	if err != nil {
		return err
	}
	for _, disk := range existing {
		if disk.PoolID == poolID {
			if err := st.PoolDisks().Delete(ctx, disk.ID); err != nil {
				return err
			}
		}
	}
	for i, disk := range disks {
		if strings.TrimSpace(disk.Path) == "" {
			continue
		}
		role := disk.Role
		if role == "" {
			role = domain.PoolDiskRoleData
		}
		if err := st.PoolDisks().Create(ctx, domain.PoolDisk{
			ID:     fmt.Sprintf("%s-disk-%d", poolID, i+1),
			PoolID: poolID,
			Path:   disk.Path,
			Role:   role,
			Vdev:   disk.Vdev,
		}); err != nil {
			return err
		}
	}
	return nil
}

func applyPoolStatus(pool domain.Pool, status domain.PoolStatus) domain.Pool {
	pool.Capacity = status.Capacity
	pool.Used = status.Used
	if len(status.Disks) > 0 {
		pool.Disks = diskPathsByRole(status.Disks, domain.PoolDiskRoleData)
		pool.ReadCacheDisks = diskPathsByRole(status.Disks, domain.PoolDiskRoleReadCache)
		pool.WriteCacheDisks = diskPathsByRole(status.Disks, domain.PoolDiskRoleWriteCache)
	}
	// 布局以池为准，记录只是缓存；没读到 vdev 树时保留原值。
	if status.Layout != "" {
		pool.Layout = status.Layout
		pool.GroupWidth = status.GroupWidth
	}
	return pool
}

func poolDiskItems(disks []domain.PoolDiskStatus) []PoolDiskItem {
	items := make([]PoolDiskItem, 0, len(disks))
	for _, disk := range disks {
		items = append(items, PoolDiskItem{
			Path:   disk.Path,
			Role:   string(disk.Role),
			Status: disk.Status,
			Vdev:   disk.Vdev,
		})
	}
	return items
}

func diskPathsByRole(disks []domain.PoolDiskStatus, role domain.PoolDiskRole) []string {
	paths := make([]string, 0, len(disks))
	for _, disk := range disks {
		if disk.Path != "" && disk.Role == role {
			paths = append(paths, disk.Path)
		}
	}
	return paths
}

func dataDiskStatuses(disks []string, status string) []domain.PoolDiskStatus {
	out := make([]domain.PoolDiskStatus, 0, len(disks))
	for _, disk := range disks {
		out = append(out, domain.PoolDiskStatus{Path: disk, Role: domain.PoolDiskRoleData, Status: status})
	}
	return out
}

// ensurePoolNameUnique 拒绝本节点已用的池名。按节点而非全集群判断：目录库整体复制，
// 各节点的池可以同名（都叫 tank），只看名字会让先登记的节点挡住其余节点。
// 与表上的 UNIQUE (server_id, name) 口径一致。
func (s PoolService) ensurePoolNameUnique(ctx context.Context, name string) error {
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return err
	}
	self := ""
	if s.Storage != nil {
		self = s.Storage.ServerID()
	}
	for _, pool := range pools {
		if pool.Name == name && (self == "" || pool.ServerID == self) {
			return fmt.Errorf("%w: %s", ErrPoolExists, name)
		}
	}
	return nil
}

// ensurePoolCountBelowLimit 拒绝在本节点建第三个池：只有数据池和备份池有用途，
// 第三个池只会分走数据池能用的容量。
func (s PoolService) ensurePoolCountBelowLimit(ctx context.Context, name string) error {
	if s.Storage == nil {
		return nil
	}
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, pool := range pools {
		if pool.ServerID == s.Storage.ServerID() {
			count++
		}
	}
	if count >= 2 {
		return errs.Invalid("每个节点最多两个存储池（一个数据池、一个备份池），不能再创建 " + name + "。如需更多容量，请给数据池添加磁盘")
	}
	return nil
}

// ensurePoolNotInUse 只拦数据池的销毁（镜像、配置、克隆都在上面）；其他池不放这些东西，
// 不能因为“存在任何镜像”就拦。
func (s PoolService) ensurePoolNotInUse(ctx context.Context, pool domain.Pool) error {
	if s.Storage == nil || pool.Name != s.Storage.DataPoolName() {
		return nil
	}
	// 池已经不在了，没有东西可毁；拦着只会让这行记录永远删不掉
	if _, err := s.Storage.PoolStatus(ctx, pool.Name); zfs.IsNotExist(err) {
		return nil
	}
	images, err := s.Store.Images().List(ctx)
	if err != nil {
		return err
	}
	if len(images) > 0 {
		return ErrPoolInUse
	}
	clones, err := s.Store.ClientClones().List(ctx)
	if err != nil {
		return err
	}
	if len(clones) > 0 {
		return ErrPoolInUse
	}
	return nil
}

func (s PoolService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now, Async: s.Async}
}

// runPoolOp 以任务方式执行 op，成功后刷新池状态；Async 时立即返回 pending 状态。
func (s PoolService) runPoolOp(ctx context.Context, typ domain.TaskType, pool domain.Pool, op func(context.Context) error) (PoolOperationResult, error) {
	var item PoolItem
	task, err := s.runner().Run(ctx, typ, pool.ID, func(ctx context.Context, task domain.Task) error {
		var err error
		item, err = s.executePoolRefresh(ctx, task, pool, op)
		return err
	})
	if err != nil {
		return PoolOperationResult{}, err
	}
	if s.Async {
		return PoolOperationResult{TaskID: task.ID, Pool: poolItem(pool, domain.PoolStatus{Health: string(domain.TaskStatusPending)})}, nil
	}
	return PoolOperationResult{TaskID: task.ID, Pool: item}, nil
}

// AdoptDataPool 为已存在但没有记录的数据池（--pool，预先手工建好的）补登记，否则概览容量为 0、
// 向导卡在建池、池也无法管理。本机看不到的池不登记，那正是向导要处理的全新环境。
func (s PoolService) AdoptDataPool(ctx context.Context) error {
	if s.Storage == nil {
		return nil
	}
	name := strings.TrimSpace(s.Storage.DataPoolName())
	if name == "" {
		return nil
	}
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return err
	}
	// 按 (节点, 池名) 判断，原因见 ensurePoolNameUnique。
	self := s.Storage.ServerID()
	for _, pool := range pools {
		if pool.Name == name && pool.ServerID == self {
			return nil
		}
	}
	status, err := s.Storage.PoolStatus(ctx, name)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Info("data pool not present yet; nothing to adopt", "pool", name, "error", err)
		}
		return nil
	}
	pool := domain.Pool{
		ID:       storage.PoolID(s.Storage.ServerID(), name),
		ServerID: s.Storage.ServerID(),
		Name:     name,
		Capacity: status.Capacity,
		Used:     status.Used,
	}
	pool = applyPoolStatus(pool, status)
	if s.Logger != nil {
		s.Logger.Info("registering the data pool prepared outside the product", "pool", name)
	}
	return s.Store.Tx(ctx, func(tx store.Store) error {
		if err := ensureServer(ctx, tx, s.nodeRow(pool.ServerID)); err != nil {
			return err
		}
		if err := tx.Pools().Create(ctx, pool); err != nil {
			return err
		}
		return s.replacePoolDisks(ctx, tx, pool.ID, status.Disks)
	})
}

// ensureServer 以稳定身份登记本节点并刷新坐标。若本机还有旧身份的记录，就地改键并带走其下的
// 池和克隆记录，而不是另插一行把它们留成孤儿。
func ensureServer(ctx context.Context, st store.Store, srv domain.Server) error {
	if existing, err := st.Servers().Get(ctx, srv.ID); err == nil {
		srv.Epoch, srv.HAState = existing.Epoch, existing.HAState
		srv.LastSeenAt, srv.Heartbeat = existing.LastSeenAt, existing.Heartbeat
		return st.Servers().Update(ctx, srv)
	} else if !errs.IsNotFound(err) {
		return err
	}
	servers, err := st.Servers().List(ctx)
	if err != nil {
		return err
	}
	// 重置 machine-id、重装系统、换主板都会让同一地址的机器拿到新 ID；servers.ip 是 UNIQUE，
	// 不处理旧行会导致插入失败、进程启动即退出。这里用管理地址认机器（不用 PortalIP：
	// HA 下各节点都报 VIP）。
	var legacy *domain.Server
	for i := range servers {
		if servers[i].ID != srv.ID && srv.IP != "" && servers[i].IP == srv.IP {
			legacy = &servers[i]
			break
		}
	}
	// 身份拆分前的部署在 ip 列里存的是 ID 字符串，只有这种“ip 不是地址”的唯一一行才算本机旧记录。
	// 必须校验这个形态：对新加入的节点，第一台也是“唯一一行且不是我”，误判会把它改名并删掉。
	if legacy == nil && len(servers) == 1 && servers[0].ID != srv.ID &&
		net.ParseIP(strings.TrimSpace(servers[0].IP)) == nil {
		legacy = &servers[0]
	}
	if legacy != nil {
		legacy := *legacy
		// 直接用传入的 store，不另开事务：调用方都已在事务中，目录库只有一个 SQLite 连接，
		// 嵌套开事务会无声地卡死启动。
		tx := st
		{
			// 顺序固定：servers.ip 是 UNIQUE 且新行常用同一地址，pools 外键 RESTRICT 删除旧行。
			// 所以先挪开旧行 ip，再插新行、迁移依赖，最后删旧行。
			parked := legacy
			parked.IP = legacy.ID + "-superseded"
			if err := tx.Servers().Update(ctx, parked); err != nil {
				return err
			}
			if err := tx.Servers().Create(ctx, srv); err != nil {
				return err
			}
			pools, err := tx.Pools().List(ctx)
			if err != nil {
				return err
			}
			for _, pool := range pools {
				if pool.ServerID != legacy.ID {
					continue
				}
				pool.ServerID = srv.ID
				if err := tx.Pools().Update(ctx, pool); err != nil {
					return err
				}
			}
			clones, err := tx.ClientClones().List(ctx)
			if err != nil {
				return err
			}
			for _, clone := range clones {
				if clone.ServerID != legacy.ID {
					continue
				}
				clone.ServerID = srv.ID
				if err := tx.ClientClones().Update(ctx, clone); err != nil {
					return err
				}
			}
			return tx.Servers().Delete(ctx, legacy.ID)
		}
	}
	return st.Servers().Create(ctx, srv)
}

// EnsureLocalServer 在启动时登记本节点，不依赖池操作；否则早已有池的部署不会迁移旧服务器记录，
// 心跳也没有行可写。
func EnsureLocalServer(ctx context.Context, st store.Store, srv domain.Server) error {
	return ensureServer(ctx, st, srv)
}

// TouchServerHeartbeat 刷新节点的 last_seen_at，用来区分在线节点和残留记录。
func TouchServerHeartbeat(ctx context.Context, st store.Store, nodeID string, now time.Time) error {
	srv, err := st.Servers().Get(ctx, nodeID)
	if err != nil {
		return err
	}
	srv.LastSeenAt = &now
	srv.Status = domain.ServerStatusUp
	return st.Servers().Update(ctx, srv)
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

var _ interface {
	List(context.Context, string) (PoolListResult, error)
	ListDisks(context.Context) (DiskListResult, error)
	Get(context.Context, string) (PoolItem, error)
	Create(context.Context, PoolRequest) (PoolOperationResult, error)
	AddDisk(context.Context, string, PoolDiskRequest) (PoolOperationResult, error)
	RemoveDisk(context.Context, string, PoolDiskRequest) (PoolOperationResult, error)
	ReplaceDisk(context.Context, string, PoolReplaceDiskRequest) (PoolOperationResult, error)
	AddReadCache(context.Context, string, PoolReadCacheRequest) (PoolOperationResult, error)
	RemoveReadCache(context.Context, string, PoolReadCacheRequest) (PoolOperationResult, error)
	AddWriteCache(context.Context, string, PoolWriteCacheRequest) (PoolOperationResult, error)
	RemoveWriteCache(context.Context, string, PoolWriteCacheRequest) (PoolOperationResult, error)
	FlushWriteCache(context.Context, string) (PoolTaskResult, error)
	Destroy(context.Context, string) (PoolTaskResult, error)
} = PoolService{}

// PoolReport 向写入者上报本节点的池（见 ha.PoolReport）：names 中哪些已导入且可读（含磁盘），
// 以及本机数据库里属于本节点的池记录。
func (s PoolService) PoolReport(ctx context.Context, names []string) (ha.PoolReport, error) {
	self := s.Storage.ServerID()
	report := ha.PoolReport{Present: []ha.ReportedPool{}, Known: []string{}}
	for _, name := range names {
		status, err := s.Storage.PoolStatus(ctx, name)
		if err != nil {
			continue // 读不到就不上报

		}
		pool := applyPoolStatus(domain.Pool{ID: storage.PoolID(self, name), ServerID: self, Name: name}, status)
		report.Present = append(report.Present, ha.ReportedPool{Pool: pool, Disks: status.Disks})
	}
	pools, err := s.Store.Pools().List(ctx)
	if err != nil {
		return ha.PoolReport{}, err
	}
	for _, p := range pools {
		if p.ServerID == self {
			report.Known = append(report.Known, p.Name)
		}
	}
	return report, nil
}
