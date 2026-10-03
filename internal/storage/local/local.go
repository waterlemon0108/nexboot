package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/config"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
	"github.com/tianwei/diskless/internal/storage/qemu"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// ZFS 是 Agent 依赖的 zfs 客户端接口。生产用 *zfs.Client，测试用内存假实现，
// 让 Agent 测试断言意图（某数据集被销毁）而不是 zfs 命令行语法。
type ZFS interface {
	Snapshot(configID, reductionID string) string
	Dataset(name string) string
	PoolName() string
	VolumePath(dataset string) string
	Clone(ctx context.Context, snapshot, dataset string) error
	CreateVolume(ctx context.Context, dataset string, sizeBytes, volblockBytes int64) error
	SnapshotRecursive(ctx context.Context, root, name string) error
	Replicate(ctx context.Context, root, target, base, snapshot string) error
	ListGUIDs(ctx context.Context, root string) (zfs.GUIDInventory, error)
	PruneSnapshots(ctx context.Context, root, prefix string, keep int) error
	EnsureFilesystem(ctx context.Context, dataset string) error
	EnsureDBCopyDataset(ctx context.Context) (string, error)
	Rename(ctx context.Context, source, dataset string) error
	ReceiveFile(ctx context.Context, filePath, dataset string, onProgress func(percent int)) error
	SendSnapshot(ctx context.Context, snapshot string, w io.Writer) error
	// ReceiveStream 把完整流收成一个尚不存在的数据集。
	ReceiveStream(ctx context.Context, dataset string, r io.Reader) error
	RollbackVolume(ctx context.Context, dataset, snapshot string) error
	SendSize(ctx context.Context, snapshot string) (int64, error)
	Referenced(ctx context.Context, dataset string) (int64, error)
	Written(ctx context.Context, dataset string) (int64, error)
	Promote(ctx context.Context, dataset string) error
	SnapshotVolume(ctx context.Context, dataset, snapshot string) error
	DestroySnapshot(ctx context.Context, dataset, snapshot string) error
	Destroy(ctx context.Context, dataset string) error
	ListDependentClones(ctx context.Context, dataset string) ([]string, error)
	ListSnapshotClones(ctx context.Context, snapshot string) ([]string, error)
	ListDatasets(ctx context.Context) ([]string, error)
	ListSnapshots(ctx context.Context) ([]string, error)
	ListAsideCopies(ctx context.Context, root string) ([]string, error)
	Used(ctx context.Context, dataset string) (int64, error)
	SnapshotsOf(ctx context.Context, dataset string) ([]string, error)
	ListPoolNames(ctx context.Context) ([]string, error)
	ListOrigins(ctx context.Context) (map[string]string, error)
	CreatePool(ctx context.Context, name string, spec storage.PoolSpec) error
	EnsureLayout(ctx context.Context) error
	DestroyPool(ctx context.Context, name string) error
	LabelClear(ctx context.Context, device string) error
	AddDisk(ctx context.Context, name string, spec storage.PoolSpec) error
	AttachDisk(ctx context.Context, name, target, disk string) error
	RemoveDisk(ctx context.Context, name, disk string) error
	DetachDisk(ctx context.Context, name, disk string) error
	AddSpecial(ctx context.Context, name string, disks []string) error
	RemoveSpecial(ctx context.Context, name, group string) error
	AddSpare(ctx context.Context, name string, disks []string) error
	RemoveSpare(ctx context.Context, name, disk string) error
	RaidzExpansion(ctx context.Context, name string) (bool, string)
	ReplaceDisk(ctx context.Context, name, oldDisk, newDisk string) error
	AddReadCache(ctx context.Context, name string, disks []string) error
	RemoveReadCache(ctx context.Context, name, disk string) error
	AddWriteCache(ctx context.Context, name string, disks []string) error
	RemoveWriteCache(ctx context.Context, name, disk string) error
	FlushWriteCache(ctx context.Context, name string) error
	PoolStatus(ctx context.Context, name string) (zfs.PoolStatus, error)
	SpaceUsage(ctx context.Context) (zfs.SpaceUsage, error)
	VolumeSize(ctx context.Context, dataset string) (int64, error)
}

var _ ZFS = (*zfs.Client)(nil)

type Agent struct {
	server   string
	zfs      ZFS
	exporter Exporter
	runner   runner
	qemu     *qemu.Client
	// 每个 MAC 一把锁：同一客户机的供给与回收不能交错，否则回收会销毁开机
	// 路径刚克隆的数据集。不同客户机互不阻塞，开机风暴不受影响。
	cloneLocks sync.Map
	// 每个 MAC 最近一次开机供给的时间。回收依据的是更早的普查，此后有过开机
	// 就作废该决定。读写都在该客户机的锁内。
	provisionedAt sync.Map
	// 测试替换点：zvol 设备节点解析。
	resolveNodeFn func(context.Context, string) (string, error)
	// 测试替换点：离线合并挂载脚本。
	injectMountScriptFn func(context.Context, string, []byte) error
	// 测试替换点：Linux 导入适配。
	adaptLinuxFn func(context.Context, string, []byte) error
	// 测试替换点：等待分区设备节点。
	waitForNodeFn func(context.Context, string) error
	// 测试替换点：枚举已挂载的 zvol。
	zvolMountsFn func() []zfs.ZvolMount
	// 测试替换点：读 /proc/self/mounts。
	procMountsFn func() []byte
	// 非空时每个导出的 target 都要求由它派生的按 MAC CHAP 登录。启动脚本生成方
	// 用同一密钥派生凭据，两边一致且密钥不经网络。空为演示模式。
	chapSecret string
}

// SetCHAPSecret 为本 Agent 的导出开启按 MAC 的 iSCSI CHAP；空为演示模式（默认）。
// 启动装配时设置一次，须与启动脚本生成方持有的密钥相同。
func (a *Agent) SetCHAPSecret(secret string) { a.chapSecret = secret }

// lockClient 串行化同一客户机的克隆生命周期操作。导出入口加锁后调用无锁实现，
// 内部回滚路径复用无锁实现才不会死锁。
func (a *Agent) lockClient(mac string) func() {
	mu, _ := a.cloneLocks.LoadOrStore(storage.NormalizeMAC(mac), &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// Agent 同时实现跨节点生命周期接口与本节点池管理接口。
var (
	_ storage.StorageAgent = (*Agent)(nil)
	_ storage.PoolAdmin    = (*Agent)(nil)
)

type runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

type Exporter interface {
	// ExportBlocks 直接写 LIO configfs，把一台客户机的全部 LUN 导出到同一 target；
	// 开机路径上与 TeardownTarget 成对使用。
	ExportBlocks(context.Context, []iscsi.ExportRequest) ([]storage.LUN, error)
	// TeardownTarget 删除整个 target（连同 LUN 映射）及指定 LUN 的 block backstore。
	TeardownTarget(context.Context, string, []int) error
	// DeleteLUNs 删除 LUN 映射和 backstore，保留 target 本身。
	DeleteLUNs(context.Context, string, []int) error
	DeleteTarget(context.Context, string) error
	ActiveSessions(context.Context) ([]string, error)
}

func New(server string, zfsClient ZFS, exporter ...Exporter) *Agent {
	if server == "" {
		if host, err := os.Hostname(); err == nil && host != "" {
			server = host
		} else {
			server = "local"
		}
	}
	var exp Exporter
	if len(exporter) > 0 {
		exp = exporter[0]
	}
	return &Agent{
		server:   server,
		zfs:      zfsClient,
		exporter: exp,
		runner:   execRunner{},
		qemu:     qemu.New(nil, nil),
	}
}

// cleanupContext 与调用方脱钩（请求取消后清理仍要跑），但带超时，防止 zfs/targetcli
// 卡死时永久阻塞。
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

// destroyDetached 用于回滚路径，主错误已在返回，故忽略自身错误。走 destroyClone
// 是为了重试短暂 busy 的 zvol，不让数据集静默泄漏。
func (a *Agent) destroyDetached(dataset string) {
	ctx, cancel := cleanupContext()
	defer cancel()
	_ = a.destroyClone(ctx, dataset)
}

// cleanupClonesDetached 用于 target 建到一半后的回滚。调用方已持有该客户机的锁，
// 所以调用无锁实现。
func (a *Agent) cleanupClonesDetached(mac string) error {
	ctx, cancel := cleanupContext()
	defer cancel()
	return a.cleanupClientClones(ctx, mac, nil)
}

func (a *Agent) CreateClientLUN(ctx context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	if req.MAC == "" {
		return storage.LUNInfo{}, fmt.Errorf("mac is required")
	}
	defer a.lockClient(req.MAC)()
	a.provisionedAt.Store(storage.NormalizeMAC(req.MAC), time.Now())
	return a.createClientLUN(ctx, req)
}

func (a *Agent) createClientLUN(ctx context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	// 一次开机只列一次数据集：拆除、销毁集合和超管克隆复用都基于它。
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return storage.LUNInfo{}, err
	}
	if err := a.teardownForBoot(ctx, req, datasets); err != nil {
		return storage.LUNInfo{}, err
	}
	info, err := a.createClientDisks(ctx, req, datasets)
	if err != nil && !req.Super {
		// 普通克隆是一次性的，要么全成要么全无；残缺的 target 不符合分组约定，整体回滚。
		if cleanupErr := a.cleanupClonesDetached(req.MAC); cleanupErr != nil {
			return storage.LUNInfo{}, errors.Join(err, cleanupErr)
		}
	}
	return info, err
}

func (a *Agent) SuperStart(ctx context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	req.Super = true
	return a.CreateClientLUN(ctx, req)
}

// teardownForBoot 一次批量释放该客户机上次会话可能残留的 target 和 backstore。
// backstore 会占住 zvol，不先释放，后面的销毁和挂载脚本注入都会 EBUSY。
// 普通开机还会丢弃残留的超管克隆；超管开机保留 SCLIENT 数据集复用。
func (a *Agent) teardownForBoot(ctx context.Context, req storage.ClientReq, datasets []string) error {
	clientClones := clientCloneDatasets(a.zfs, req.MAC, datasets)
	superClones := superClientCloneDatasets(a.zfs, req.MAC, datasets)
	if a.exporter != nil {
		// 同一 LUN 的普通与超管克隆共用 backstore 名，去重后的并集即可覆盖两者。
		seen := make(map[int]bool)
		luns := make([]int, 0, len(clientClones)+len(superClones))
		for _, dataset := range clientClones {
			if lun, ok := clientCloneLUN(a.zfs, req.MAC, dataset); ok && !seen[lun] {
				seen[lun] = true
				luns = append(luns, lun)
			}
		}
		for _, dataset := range superClones {
			if lun, ok := superClientCloneLUN(a.zfs, req.MAC, dataset); ok && !seen[lun] {
				seen[lun] = true
				luns = append(luns, lun)
			}
		}
		sort.Ints(luns)
		if err := a.exporter.TeardownTarget(ctx, iscsi.TargetForMAC(req.MAC), luns); err != nil {
			return err
		}
	}
	doomed := clientClones
	if !req.Super {
		doomed = append(doomed, superClones...)
	}
	a.releaseStaleMounts(ctx, doomed)
	for _, dataset := range doomed {
		if err := a.destroyClone(ctx, dataset); err != nil {
			return err
		}
	}
	return nil
}

// destroyClone 重试吸收短暂占用窗口（LIO 拆除后 udev 重新探测、复制发送），
// 这期间 ZFS 会报 "dataset is busy"。
func (a *Agent) destroyClone(ctx context.Context, dataset string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := a.zfs.Destroy(ctx, dataset)
		if err == nil || !zfs.IsBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// createClientDisks 克隆系统盘与数据盘、注入盘符脚本并一次性导出全部 LUN。
// datasets 是拆除前的列表：超管开机复用其中已有的持久克隆，普通开机总是重新克隆。
func (a *Agent) createClientDisks(ctx context.Context, req storage.ClientReq, datasets []string) (storage.LUNInfo, error) {
	cloneName, dataCloneName := storage.ClientCloneName, storage.ClientDataCloneName
	var existing map[string]bool
	if req.Super {
		cloneName, dataCloneName = storage.SuperClientCloneName, storage.SuperClientDataCloneName
		existing = make(map[string]bool, len(datasets))
		for _, dataset := range datasets {
			existing[dataset] = true
		}
	}
	type disk struct {
		lunNo  int
		source storage.ClientSource
		name   string
	}
	disks := []disk{{lunNo: 0, source: req.System, name: cloneName(req.MAC)}}
	for _, source := range req.DataDisks {
		// LUN 0 是系统盘，没有 LUN 的数据盘会覆盖它导致从错误的卷启动。拒绝而不重新编号：
		// 编号由调用方决定，它下发的盘符按编号对应。
		if source.LUN < 1 {
			return storage.LUNInfo{}, fmt.Errorf("data disk %s has no LUN", source.ConfigID)
		}
		disks = append(disks, disk{lunNo: source.LUN, source: source, name: dataCloneName(req.MAC, source.LUN)})
	}
	// 只按名字复用会把别的配置的盘挂给机器；所有盘先核对完再动手。
	var origins map[string]string
	for _, d := range disks {
		if d.source.ConfigID == "" || d.source.SnapshotName == "" {
			return storage.LUNInfo{}, fmt.Errorf("config and reduction are required")
		}
		dataset := a.zfs.Dataset(d.name)
		if existing == nil || !existing[dataset] {
			continue
		}
		if origins == nil {
			var err error
			if origins, err = a.zfs.ListOrigins(ctx); err != nil {
				return storage.LUNInfo{}, err
			}
		}
		if err := a.checkSuperSource(origins, req.MAC, d.lunNo, dataset, d.source.ConfigID, false); err != nil {
			return storage.LUNInfo{}, err
		}
	}
	luns := make([]storage.LUN, 0, len(disks))
	exports := make([]iscsi.ExportRequest, 0, len(disks))
	for _, d := range disks {
		dataset := a.zfs.Dataset(d.name)
		if existing == nil || !existing[dataset] {
			if err := a.zfs.Clone(ctx, a.zfs.Snapshot(d.source.ConfigID, d.source.SnapshotName), dataset); err != nil {
				return storage.LUNInfo{}, err
			}
		}
		// 只给系统盘注入盘符脚本。注入失败只是这次开机没有盘符，不能让开机失败。
		if d.lunNo == 0 && len(req.MountScript) > 0 {
			if err := a.injectMountScript(ctx, a.zfs.VolumePath(dataset), req.MountScript); err != nil {
				slog.Warn("mount-script injection skipped", "dataset", dataset, "error", err)
			}
		}
		luns = append(luns, storage.LUN{LUN: d.lunNo, MountTarget: d.source.MountTarget, VolPath: a.zfs.VolumePath(dataset)})
		export := iscsi.ExportRequest{MAC: req.MAC, VolPath: a.zfs.VolumePath(dataset), LUN: d.lunNo}
		if iscsi.CHAPEnabled(a.chapSecret) {
			creds := iscsi.CHAPCredentials(a.chapSecret, req.MAC)
			export.CHAP = &creds
		}
		exports = append(exports, export)
	}
	if a.exporter != nil {
		exported, err := a.exporter.ExportBlocks(ctx, exports)
		if err != nil {
			return storage.LUNInfo{}, err
		}
		for i := range exported {
			exported[i].MountTarget = luns[i].MountTarget
		}
		luns = exported
	}
	return storage.LUNInfo{
		Server:    a.server,
		Target:    luns[0].Target,
		System:    luns[0],
		DataDisks: luns[1:],
	}, nil
}

// ActiveClientMACs 返回在本节点有 iSCSI 会话的客户机 MAC，用于判定在线状态。
// 未接 exporter 时返回 ErrNotImplemented，调用方据此跳过对账，而不是把所有机器标成离线。
func (a *Agent) ActiveClientMACs(ctx context.Context) ([]string, error) {
	if a.exporter == nil {
		return nil, storage.ErrNotImplemented
	}
	return a.exporter.ActiveSessions(ctx)
}

// ClientCloneMACs 列出本节点仍有 CLIENT-* 克隆的 MAC。一次列表覆盖整个节点，
// 离线回收不必逐台 shell out。持久的超管克隆（SCLIENT-*）不在其中。
func (a *Agent) ClientCloneMACs(ctx context.Context) ([]string, error) {
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	macs := []string{}
	seen := map[string]bool{}
	for _, dataset := range datasets {
		mac, ok := orphanClientCloneMAC(datasetName(dataset))
		if !ok || seen[mac] {
			continue
		}
		seen[mac] = true
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs, nil
}

func (a *Agent) CleanupClientClones(ctx context.Context, mac string, keep []string) error {
	defer a.lockClient(mac)()
	return a.cleanupClientClones(ctx, mac, keep)
}

// ReclaimIdleClientClones 持锁后再确认客户机自 decidedAt 起未开机且无 iSCSI 会话，
// 才回收其克隆。普查到此刻之间这两者都可能变化，此时销毁的就是正在开机机器的盘。
// 返回是否回收。
func (a *Agent) ReclaimIdleClientClones(ctx context.Context, mac string, decidedAt time.Time) (bool, error) {
	key := storage.NormalizeMAC(mac)
	defer a.lockClient(key)()
	if at, ok := a.provisionedAt.Load(key); ok && !at.(time.Time).Before(decidedAt) {
		return false, nil
	}
	if a.exporter != nil {
		live, err := a.exporter.ActiveSessions(ctx)
		if err != nil {
			return false, err
		}
		for _, m := range live {
			if storage.NormalizeMAC(m) == key {
				return false, nil
			}
		}
	}
	return true, a.cleanupClientClones(ctx, key, nil)
}

// SuperCloneMACs 列出本节点上有持久超管克隆的 MAC。超管机放在写入者上，切换后
// 旧写入者的副本会滞留（run/ 不复制）；该节点再成为写入者时会被 createClientDisks
// 复用，盘的内容随 VIP 归属在不同版本间跳，所以要能列出来清理。
func (a *Agent) SuperCloneMACs(ctx context.Context) ([]string, error) {
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	macs := []string{}
	seen := map[string]bool{}
	for _, dataset := range datasets {
		name, ok := a.poolRelative(dataset)
		if !ok {
			continue
		}
		carrier := storage.Classify(name)
		if carrier.Kind != storage.CarrierSuper || carrier.MAC == "" || seen[carrier.MAC] {
			continue
		}
		seen[carrier.MAC] = true
		macs = append(macs, carrier.MAC)
	}
	sort.Strings(macs)
	return macs, nil
}

// CleanupSuperClones 先删 backstore 再销毁本节点上该机器的超管克隆。超管克隆是
// 操作者的真实工作，只能在本节点不提供服务时调用（见 quiesceStandby）。
func (a *Agent) CleanupSuperClones(ctx context.Context, mac string) error {
	defer a.lockClient(mac)()
	if err := a.deleteSuperBackstores(ctx, mac); err != nil {
		return err
	}
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	doomed := superClientCloneDatasets(a.zfs, mac, datasets)
	a.releaseStaleMounts(ctx, doomed)
	for _, dataset := range doomed {
		if err := a.destroyClone(ctx, dataset); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) cleanupClientClones(ctx context.Context, mac string, keep []string) error {
	keepDatasets := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		keepDatasets[a.zfs.Dataset(name)] = struct{}{}
	}
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	targets := clientCloneDatasets(a.zfs, mac, datasets)
	target := iscsi.TargetForMAC(mac)
	doomed := make([]string, 0, len(targets))
	luns := make([]int, 0, len(targets))
	for _, dataset := range targets {
		if _, ok := keepDatasets[dataset]; ok {
			continue
		}
		doomed = append(doomed, dataset)
		if lun, ok := clientCloneLUN(a.zfs, mac, dataset); ok {
			luns = append(luns, lun)
		}
	}
	// backstore 会占住 zvol，必须先删，且合并成一次调用。
	if a.exporter != nil {
		if err := a.exporter.DeleteLUNs(ctx, target, luns); err != nil {
			return err
		}
	}
	a.releaseStaleMounts(ctx, doomed)
	for _, dataset := range doomed {
		// 用 destroyClone：回滚和清理路径同样会撞上拆除后的释放窗口。
		if err := a.destroyClone(ctx, dataset); err != nil {
			return err
		}
	}
	if len(keepDatasets) == 0 {
		if err := a.deleteTarget(ctx, mac); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) existingDatasets(ctx context.Context) (map[string]bool, error) {
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(datasets))
	for _, dataset := range datasets {
		existing[dataset] = true
	}
	return existing, nil
}

func clientCloneDatasets(z ZFS, mac string, datasets []string) []string {
	return cloneDatasets(datasets, func(dataset string) bool {
		_, ok := clientCloneLUN(z, mac, dataset)
		return ok
	})
}

func clientCloneLUN(z ZFS, mac string, dataset string) (int, bool) {
	return cloneLUN(z, storage.CarrierClient, mac, dataset)
}

func superClientCloneDatasets(z ZFS, mac string, datasets []string) []string {
	return cloneDatasets(datasets, func(dataset string) bool {
		_, ok := superClientCloneLUN(z, mac, dataset)
		return ok
	})
}

func superClientCloneLUN(z ZFS, mac string, dataset string) (int, bool) {
	return cloneLUN(z, storage.CarrierSuper, mac, dataset)
}

// cloneLUN 从数据集路径反推它是该机器的哪块盘：系统盘为 0，数据盘为其 LUN。
// 其它池、其它机器或其它类型的数据集不算。
func cloneLUN(z ZFS, kind storage.CarrierKind, mac, dataset string) (int, bool) {
	name := datasetName(dataset)
	if z.Dataset(name) != dataset {
		return 0, false // 其它池的数据集
	}
	carrier := storage.Classify(name)
	if carrier.Kind != kind || carrier.MAC != storage.NormalizeMAC(mac) {
		return 0, false
	}
	return carrier.LUN, true
}

func cloneDatasets(datasets []string, owned func(string) bool) []string {
	targets := make([]string, 0, len(datasets))
	for _, dataset := range datasets {
		if owned(dataset) {
			targets = append(targets, dataset)
		}
	}
	sort.Strings(targets)
	return targets
}

func orphanClientCloneMAC(name string) (string, bool) {
	carrier := storage.Classify(name)
	if carrier.Kind != storage.CarrierClient || carrier.MAC == "" {
		return "", false
	}
	return carrier.MAC, true
}

// poolRelative 把完整数据集路径还原成库里使用的名字："tank/nd/win11" 即镜像
// "win11"，"tank/run/CLIENT-X" 即克隆 "CLIENT-X"。其它池、容器本身及其保留子项
// （如 DB 副本）都不算。
func (a *Agent) poolRelative(full string) (string, bool) {
	for _, root := range []string{storage.CatalogueRoot, storage.RunRoot} {
		prefix := a.zfs.PoolName() + "/" + root + "/"
		if trimmed, ok := strings.CutPrefix(full, prefix); ok {
			if strings.Contains(trimmed, "/") {
				return "", false // 保留子项之下
			}
			if root == storage.CatalogueRoot && storage.ReservedCatalogueChild(trimmed) {
				return "", false
			}
			return trimmed, true
		}
	}
	return "", false
}

func datasetName(dataset string) string {
	if idx := strings.LastIndexByte(dataset, '/'); idx >= 0 {
		return dataset[idx+1:]
	}
	return dataset
}

// 导入进度节点集中在此。复制阶段占 pgCopyStart..pgCopyEnd，其余为单点；
// pgConfigSnap 之后由 control/image.go 接续（98/99/100）。
const (
	pgPrepare    = 1
	pgProbe      = 2
	pgCreateVol  = 5
	pgCopyStart  = 6
	pgCopyEnd    = 85
	pgSnapshot   = 88
	pgClone      = 93
	pgConfigSnap = 97
)

// copyPercent 把 0-100 的复制子进度映射到整体导入进度。
func copyPercent(p int) int { return pgCopyStart + (pgCopyEnd-pgCopyStart)*p/100 }

func (a *Agent) ImportImage(ctx context.Context, req storage.ImportImageReq) (storage.ImportImageResult, error) {
	if req.Name == "" || req.SourcePath == "" || req.OSType == "" {
		return storage.ImportImageResult{}, fmt.Errorf("name, source path and os type are required")
	}
	sourcePath, err := validateImportFilePath(req.SourcePath, importDir(req.ImportDir))
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	datasets, err := a.existingDatasets(ctx)
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	imageID := storage.ImageName(req.Name, func(id string) bool { return datasets[a.zfs.Dataset(id)] })
	imageDataset := a.zfs.Dataset(imageID)
	configID := storage.DefaultConfigName(imageID)
	configDataset := a.zfs.Dataset(configID)
	if datasets[imageDataset] || datasets[configDataset] {
		return storage.ImportImageResult{}, fmt.Errorf("target dataset already exists")
	}
	if req.OnTarget != nil {
		req.OnTarget(imageID)
	}
	report := func(percent int, message string) {
		if req.OnProgress != nil {
			req.OnProgress(percent, message)
		}
	}
	report(pgPrepare, "准备导入")

	var imageSize int64
	if isDiskImageFile(strings.ToLower(sourcePath)) {
		size, created, err := a.importDiskImage(ctx, sourcePath, imageDataset, report)
		if err != nil {
			if created {
				a.destroyDetached(imageDataset)
			}
			return storage.ImportImageResult{}, err
		}
		imageSize = size
	} else {
		if err := a.zfs.ReceiveFile(ctx, sourcePath, imageDataset, func(p int) {
			report(copyPercent(p), fmt.Sprintf("接收镜像流 %d%%", p))
		}); err != nil {
			if !targetTaken(err) {
				a.destroyDetached(imageDataset)
			}
			return storage.ImportImageResult{}, err
		}
		a.stripExportSnapshots(ctx, imageDataset)
		// 卷大小由流决定，需回读；仅供展示，读失败不能让已成功的导入作废。
		if size, err := a.zfs.VolumeSize(ctx, imageDataset); err != nil {
			slog.Warn("image volsize read failed", "image", imageID, "error", err)
		} else {
			imageSize = size
		}
	}
	injected := false
	// 在首个快照前烘焙启动脚本，下游配置、还原点和客户机克隆都免费继承。失败只是
	// 退回开机时逐台注入，绝不能让导入失败。数据盘没有系统，不烘焙。
	if req.Purpose != domain.ImagePurposeData && len(req.MountScript) > 0 {
		bake := map[domain.OSType]func(context.Context, string, []byte) error{
			domain.OSTypeWindows: a.bakeMountScript,
			domain.OSTypeLinux:   a.bakeLinux,
		}[req.OSType]
		if bake != nil {
			report(pgSnapshot, "写入启动脚本")
			if err := bake(ctx, a.zfs.VolumePath(imageDataset), req.MountScript); err != nil {
				slog.Warn("image mount-script bake skipped", "image", imageID, "os", req.OSType, "error", err)
			} else {
				injected = true
			}
		}
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = domain.ImagePurposeSystem
	}
	return a.finishImage(ctx, imageID, configID, report, domain.Image{
		Name: req.Name, OSType: req.OSType, Size: imageSize, Purpose: purpose, Origin: domain.ImageOriginImported,
	}, injected)
}

// stripExportSnapshots 删掉导出流自带的 ndexport- 临时快照，让重新导入与全新导入
// 一致。只删自己的前缀，手工流里的其它快照不碰；失败只告警。
func (a *Agent) stripExportSnapshots(ctx context.Context, imageDataset string) {
	snaps, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		slog.Warn("import: snapshot listing failed; export markers not stripped", "dataset", imageDataset, "error", err)
		return
	}
	for _, snap := range snaps {
		dataset, name, ok := strings.Cut(snap, "@")
		if !ok || dataset != imageDataset || !strings.HasPrefix(name, storage.ExportSnapshotPrefix) {
			continue
		}
		if err := a.zfs.DestroySnapshot(ctx, dataset, name); err != nil {
			slog.Warn("import: export marker snapshot not stripped", "snapshot", snap, "error", err)
		}
	}
}

// ExportImage 把镜像整盘以裸流导出到 req.W，可重新导入。期间用 ndexport- 临时快照
// 冻结镜像；无论成败都用新 context 删除该快照，下载被取消也能清理。
func (a *Agent) ExportImage(ctx context.Context, req storage.ExportImageReq) error {
	if req.ConfigID != "" {
		if req.W == nil {
			return fmt.Errorf("writer is required")
		}
		return a.exportRestorePoint(ctx, req)
	}
	if req.ImageID == "" || req.W == nil {
		return fmt.Errorf("image id and writer are required")
	}
	dataset := a.zfs.Dataset(req.ImageID)
	snapName := fmt.Sprintf("%s%d", storage.ExportSnapshotPrefix, time.Now().UTC().UnixNano())
	if err := a.zfs.SnapshotVolume(ctx, dataset, snapName); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.zfs.DestroySnapshot(cleanupCtx, dataset, snapName); err != nil {
			slog.Warn("export snapshot not cleaned up", "snapshot", dataset+"@"+snapName, "error", err)
		}
	}()
	snapshot := dataset + "@" + snapName
	var estimated int64
	if req.OnProgress != nil {
		size, err := a.zfs.SendSize(ctx, snapshot)
		if err != nil {
			slog.Warn("export size estimate unavailable", "snapshot", snapshot, "error", err)
		} else {
			estimated = size
		}
	}
	w := req.W
	if req.OnProgress != nil {
		w = &exportProgressWriter{w: req.W, estimated: estimated, onProgress: req.OnProgress}
	}
	return a.zfs.SendSnapshot(ctx, snapshot, w)
}

// ExportImageSize 用镜像的 referenced（压缩后，与裸流一致）估算导出大小。不建快照，
// 足够便宜，可用于提交前的空间预检。
func (a *Agent) ExportImageSize(ctx context.Context, imageID string) (int64, error) {
	if imageID == "" {
		return 0, fmt.Errorf("image id is required")
	}
	return a.zfs.Referenced(ctx, a.zfs.Dataset(imageID))
}

// exportProgressWriter 按预估总量报告已流过的字节。
type exportProgressWriter struct {
	w          io.Writer
	written    int64
	estimated  int64
	onProgress func(written, estimated int64)
}

func (p *exportProgressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.written += int64(n)
		p.onProgress(p.written, p.estimated)
	}
	return n, err
}

// finishImage 在卷写满数据后建镜像 @0、克隆默认配置并建其 @0，返回待入库的记录。
// 失败时销毁镜像数据集。
func (a *Agent) finishImage(ctx context.Context, imageID, configID string, report func(int, string), img domain.Image, injected bool) (storage.ImportImageResult, error) {
	imageDataset := a.zfs.Dataset(imageID)
	configDataset := a.zfs.Dataset(configID)
	rollback := func() {
		a.destroyDetached(imageDataset)
	}
	report(pgSnapshot, "创建快照")
	if err := a.zfs.SnapshotVolume(ctx, imageDataset, "0"); err != nil {
		rollback()
		return storage.ImportImageResult{}, err
	}
	report(pgClone, "克隆默认配置")
	if err := a.zfs.Clone(ctx, imageDataset+"@0", configDataset); err != nil {
		a.destroyDetached(imageDataset + "@0")
		rollback()
		return storage.ImportImageResult{}, err
	}
	report(pgConfigSnap, "创建配置快照")
	if err := a.zfs.SnapshotVolume(ctx, configDataset, "0"); err != nil {
		a.destroyDetached(configDataset)
		rollback()
		return storage.ImportImageResult{}, err
	}
	now := time.Now().UTC()
	reductionID := imageID + "_0"
	img.ID = imageID
	img.State = domain.ImageStateNormal
	img.CreatedAt = now
	return storage.ImportImageResult{
		Image: img,
		Config: domain.Config{
			ID:                 configID,
			ImageID:            imageID,
			Name:               "default",
			DefaultReductionID: &reductionID,
			CreatedAt:          now,
		},
		Reduction: domain.Reduction{
			ID:        reductionID,
			ConfigID:  configID,
			Name:      "@0",
			CreatedAt: now,
			Status:    domain.ReductionStatusReady,
		},
		MountScriptInjected: injected,
	}, nil
}

const defaultVolblockBytes = 16 * 1024 // 16K 卷块，与 ZFS 默认一致

// importDiskImage 用 qemu-img 把外部磁盘镜像转换写入新建 zvol，返回虚拟大小。
// created 表示卷是本次建的；出错时只有它为真，调用方才能销毁 imageDataset。
func (a *Agent) importDiskImage(ctx context.Context, sourcePath, imageDataset string, report func(percent int, message string)) (size int64, created bool, err error) {
	if a.qemu == nil {
		return 0, false, fmt.Errorf("qemu-img support is not configured")
	}
	report(pgProbe, "探测镜像")
	info, err := a.qemu.Info(ctx, sourcePath)
	if err != nil {
		if !qemu.Available() {
			return 0, false, fmt.Errorf("qemu-img is required to import disk images; install qemu-utils")
		}
		return 0, false, err
	}
	report(pgCreateVol, "创建卷")
	// zfs create 是原子的：失败就什么都没建，已存在的那个属于别人（如并发的同名导入）。
	if err := a.zfs.CreateVolume(ctx, imageDataset, info.VirtualSize, defaultVolblockBytes); err != nil {
		return 0, false, err
	}
	// 直接写真实的 /dev/zdN，不依赖 udev 软链或事件队列 settle。
	node, err := a.resolveNode(ctx, a.zfs.VolumePath(imageDataset))
	if err != nil {
		return 0, true, err
	}
	if err := a.qemu.ConvertToRaw(ctx, sourcePath, info.Format, node, func(p int) {
		report(copyPercent(p), fmt.Sprintf("转换写入 %d%%", p))
	}); err != nil {
		return 0, true, err
	}
	return info.VirtualSize, true, nil
}

// targetTaken 判断建卷或接收失败是因为目标已存在：那个数据集不是本次调用建的，回滚不能删。
func targetTaken(err error) bool {
	if zfs.IsExists(err) {
		return true
	}
	var cmdErr storage.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	return strings.Contains(cmdErr.Output, "destination") &&
		(strings.Contains(cmdErr.Output, "exists") || strings.Contains(cmdErr.Output, "has snapshots"))
}

// resolveNode 经内核映射把 zvol 路径解析为 /dev/zdN（见 zfs.ResolveNode），并等待创建延迟。
func (a *Agent) resolveNode(ctx context.Context, dev string) (string, error) {
	if a.resolveNodeFn != nil {
		return a.resolveNodeFn(ctx, dev)
	}
	return zfs.ResolveNode(ctx, dev)
}

func validateImportFilePath(sourcePath, rootPath string) (string, error) {
	rootPath = filepath.Clean(strings.TrimSpace(rootPath))
	lower := strings.ToLower(sourcePath)
	if !isSupportedImportImageFile(lower) {
		return "", fmt.Errorf("image file must be a .zfs/.gz/.gzip stream or a .vmdk/.vhd/.vhdx/.qcow2/.vdi/.raw disk image")
	}
	// zfs receive 能识别 gzip，qemu-img 不能且不报错：会把压缩数据当作几 KB 的裸盘，
	// 导入一个空镜像。
	if isDiskImageFile(lower) && fileLooksGzipped(sourcePath) {
		return "", fmt.Errorf("%s 是 gzip 压缩文件，磁盘镜像不支持压缩导入：请先解压，或改用 .zfs.gz 镜像流",
			filepath.Base(sourcePath))
	}
	if !filepath.IsAbs(sourcePath) {
		return "", fmt.Errorf("image file path must be absolute")
	}
	if !filepath.IsAbs(rootPath) {
		return "", fmt.Errorf("import directory must be absolute")
	}
	root, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image file must be under %s", rootPath)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("image file must be a regular file")
	}
	return resolved, nil
}

var diskImageExts = []string{".vmdk", ".vhd", ".vhdx", ".qcow2", ".qcow", ".vdi", ".raw", ".img"}

// isZFSStreamFile 只按扩展名分流：非磁盘镜像都交给 zfs receive（它自己识别 gzip）。
// 因此裸 .gz 视为流；压缩的磁盘镜像必须保留自身扩展名才会被认出。
func isZFSStreamFile(path string) bool {
	return strings.HasSuffix(path, ".gzip") ||
		strings.HasSuffix(path, ".zfs") ||
		strings.HasSuffix(path, ".gz")
}

// fileLooksGzipped 检查 gzip 魔数。读不了按未压缩处理，由调用方后续检查报出更明确的错误。
func fileLooksGzipped(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x1f && magic[1] == 0x8b
}

func isDiskImageFile(path string) bool {
	for _, ext := range diskImageExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func isSupportedImportImageFile(path string) bool {
	return isZFSStreamFile(path) || isDiskImageFile(path)
}

func importDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return config.DefaultImportDir
	}
	return dir
}

func (a *Agent) DeleteImage(ctx context.Context, imageID string, configIDs []string) error {
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return err
	}
	// 镜像和配置一起按依赖顺序清扫（见 destroyInDependencyOrder）：中断的合并可能让
	// 镜像挂在配置下，"先配置后镜像" 并不总成立。已不存在视为成功，否则数据集先没了的
	// 记录就永远删不掉。
	return a.destroyInDependencyOrder(ctx, top, append(append([]string{}, configIDs...), imageID), "")
}

func (a *Agent) RollbackImportImage(ctx context.Context, req storage.ImportImageReq, imported storage.ImportImageResult) error {
	imageID := strings.TrimSpace(imported.Image.ID)
	if imageID == "" {
		imageID = strings.TrimSpace(req.Name)
	}
	if imageID == "" {
		return fmt.Errorf("image is required")
	}
	configID := strings.TrimSpace(imported.Config.ID)
	if configID == "" && imageID != "" {
		configID = storage.DefaultConfigName(imageID)
	}
	imageDataset := a.zfs.Dataset(imageID)
	var errs []error
	if configID != "" {
		errs = append(errs, a.zfs.Destroy(ctx, a.zfs.Dataset(configID)))
	}
	errs = append(errs, a.zfs.DestroySnapshot(ctx, imageDataset, "0"))
	errs = append(errs, a.zfs.Destroy(ctx, imageDataset))
	return errors.Join(errs...)
}

func (a *Agent) CreateConfig(ctx context.Context, sourceID string, name string, fromReduction *string) (storage.CreateConfigResult, error) {
	sourceID = strings.TrimSpace(sourceID)
	name = strings.TrimSpace(name)
	if sourceID == "" || name == "" {
		return storage.CreateConfigResult{}, fmt.Errorf("source and name are required")
	}
	reduction := storage.BaselineReduction
	if fromReduction != nil {
		reduction = strings.TrimSpace(*fromReduction)
	}
	if reduction == "" {
		return storage.CreateConfigResult{}, fmt.Errorf("reduction is required")
	}

	cfgID := storage.NewConfigName(sourceID, name)
	snapshot := a.zfs.Snapshot(sourceID, reduction)
	createdAt := time.Now().UTC()
	if err := a.zfs.Clone(ctx, snapshot, a.zfs.Dataset(cfgID)); err != nil {
		return storage.CreateConfigResult{}, err
	}
	// 客户机克隆和派生都按 "<config>@<reduction>" 寻址，没有基线快照的配置无法启动。
	if err := a.zfs.SnapshotVolume(ctx, a.zfs.Dataset(cfgID), storage.BaselineReduction); err != nil {
		a.destroyDetached(a.zfs.Dataset(cfgID))
		return storage.CreateConfigResult{}, err
	}
	baseline := domain.Reduction{
		ID:        storage.ReductionID(cfgID, storage.BaselineReduction),
		ConfigID:  cfgID,
		Name:      "@" + storage.BaselineReduction,
		CreatedAt: createdAt,
		Status:    domain.ReductionStatusReady,
	}
	return storage.CreateConfigResult{
		Config: domain.Config{
			ID:                 cfgID,
			ImageID:            sourceID,
			Name:               name,
			DefaultReductionID: &baseline.ID,
			CreatedAt:          createdAt,
		},
		Reduction: baseline,
	}, nil
}
func (a *Agent) DeleteConfig(ctx context.Context, configID string) error {
	if configID == "" {
		return fmt.Errorf("config is required")
	}
	dataset := a.zfs.Dataset(configID)
	clones, err := a.zfs.ListDependentClones(ctx, dataset)
	if err != nil {
		return err
	}
	for _, clone := range clones {
		if mac, ok := orphanClientCloneMAC(datasetName(clone)); ok {
			if err := a.CleanupClientClones(ctx, mac, nil); err != nil {
				return err
			}
		}
	}
	// 复制正在发送的快照会报 busy，用 destroyClone 重试。
	if err := a.destroyClone(ctx, dataset); err != nil && !zfs.IsNotExist(err) {
		return err
	}
	return nil
}

// MergeConfig 把保留的配置提升为镜像并删除其它配置（FR-B2），结果与全新导入一致：
// 镜像为链根且带 @0，保留配置是它的克隆并有自己的基线。步骤顺序都在真实 ZFS 上验证过。
func (a *Agent) MergeConfig(ctx context.Context, req storage.MergeConfigReq) (storage.MergeConfigResult, error) {
	if req.ImageID == "" || req.ConfigID == "" {
		return storage.MergeConfigResult{}, fmt.Errorf("image and config are required")
	}
	configDataset := a.zfs.Dataset(req.ConfigID)
	imageDataset := a.zfs.Dataset(req.ImageID)
	// ZFS 没有事务，中断的合并只能重跑；盲目重放会销毁上次已改名到位的镜像，连同合并
	// 结果一起丢掉。所以先读池的实际形态，从所处阶段继续。
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return storage.MergeConfigResult{}, err
	}
	switch {
	case !top.has(configDataset) && !top.has(imageDataset):
		return storage.MergeConfigResult{}, fmt.Errorf("merge cannot start: neither %s nor %s exists", imageDataset, configDataset)
	case top.has(configDataset) && top.has(imageDataset):
		// 可能是初始形态，也可能是 promote 后、销毁旧镜像前中断的重试。必须区分：
		// 后者再折叠配置快照会和镜像对它们的依赖冲突。
		promoted, err := top.promotedOver(ctx, configDataset, imageDataset)
		if err != nil {
			return storage.MergeConfigResult{}, err
		}
		if !promoted {
			// 先删兄弟配置：从保留配置派生的配置依赖其快照，否则折叠会报
			// "snapshot has dependent clones"。
			if err := a.destroyInDependencyOrder(ctx, top, req.DeleteConfigIDs, req.ConfigID); err != nil {
				return storage.MergeConfigResult{}, err
			}
			if err := a.rollbackBeforeFold(ctx, configDataset, req.RollbackTo); err != nil {
				return storage.MergeConfigResult{}, err
			}
			// 折叠保留配置的历史：克隆与父数据集有同名快照时 promote 会直接拒绝，而两者都有 "@0"。
			for _, name := range req.ReductionNames {
				snapshot := strings.TrimPrefix(strings.TrimSpace(name), "@")
				if snapshot == "" {
					continue
				}
				if err := top.ensureSnapshotDestroyed(ctx, configDataset, snapshot); err != nil {
					return storage.MergeConfigResult{}, err
				}
			}
			if err := top.ensurePromoted(ctx, configDataset, imageDataset); err != nil {
				return storage.MergeConfigResult{}, err
			}
		}
		if err := top.ensureDestroyed(ctx, imageDataset); err != nil {
			return storage.MergeConfigResult{}, err
		}
		// promote 把镜像的基线移到了配置上；兄弟配置和旧镜像都删掉后它才能删。
		if err := top.ensureSnapshotDestroyed(ctx, configDataset, storage.BaselineReduction); err != nil {
			return storage.MergeConfigResult{}, err
		}
	}
	// 此时只剩配置（待改名）或只剩镜像（已改名），收尾相同且幂等。
	if err := a.finalizeMerge(ctx, top, req.ImageID, req.ConfigID); err != nil {
		return storage.MergeConfigResult{}, err
	}
	return storage.MergeConfigResult{Reduction: domain.Reduction{
		ID:        storage.ReductionID(req.ConfigID, storage.BaselineReduction),
		ConfigID:  req.ConfigID,
		Name:      "@" + storage.BaselineReduction,
		CreatedAt: time.Now().UTC(),
		Status:    domain.ReductionStatusReady,
	}}, nil
}
func (a *Agent) CreateReduction(ctx context.Context, configID string, name string) (domain.Reduction, error) {
	configID = strings.TrimSpace(configID)
	snapshot := strings.TrimPrefix(strings.TrimSpace(name), "@")
	if configID == "" || snapshot == "" {
		return domain.Reduction{}, fmt.Errorf("config and reduction are required")
	}
	if err := a.zfs.SnapshotVolume(ctx, a.zfs.Dataset(configID), snapshot); err != nil {
		return domain.Reduction{}, err
	}
	now := time.Now().UTC()
	return domain.Reduction{
		ID:        storage.ReductionID(configID, snapshot),
		ConfigID:  configID,
		Name:      "@" + snapshot,
		CreatedAt: now,
		Status:    domain.ReductionStatusReady,
	}, nil
}
func (a *Agent) DeleteReduction(ctx context.Context, configID string, name string) error {
	configID = strings.TrimSpace(configID)
	snapshot := strings.TrimPrefix(strings.TrimSpace(name), "@")
	if configID == "" || snapshot == "" {
		return fmt.Errorf("config and reduction are required")
	}
	if err := a.zfs.DestroySnapshot(ctx, a.zfs.Dataset(configID), snapshot); err != nil && !zfs.IsNotExist(err) {
		return err
	}
	return nil
}

// MergeReduction 逐个销毁要合并掉的快照并返回实际删掉的那些。ZFS 无法回退，
// 中途失败时前面的已删，调用方需按准确列表同步库记录。
func (a *Agent) MergeReduction(ctx context.Context, configID string, deleteNames []string) ([]string, error) {
	var destroyed []string
	for _, name := range deleteNames {
		if err := a.DeleteReduction(ctx, configID, name); err != nil {
			return destroyed, err
		}
		destroyed = append(destroyed, name)
	}
	return destroyed, nil
}

// rollbackBeforeFold 把保留配置回滚到镜像要采用的还原点。回滚只删其后的还原点，
// 它们的派生作为兄弟配置已删。还原点已不存在说明上次已过折叠步骤，无需重做。
func (a *Agent) rollbackBeforeFold(ctx context.Context, configDataset, to string) error {
	to = strings.TrimPrefix(strings.TrimSpace(to), "@")
	if to == "" {
		return nil
	}
	snaps, err := a.zfs.SnapshotsOf(ctx, configDataset)
	if err != nil {
		return err
	}
	if !slices.Contains(snaps, to) {
		return nil
	}
	return a.zfs.RollbackVolume(ctx, configDataset, to)
}

// finalizeMerge 把合并的最后四步（改名、建快照、克隆回配置、再建快照）推进到终态。
// 中断在改名之后时库里的配置指向已不存在的数据集，且无法回退，所以每步都写成
// "确保如此"，整个合并可直接重跑。
func (a *Agent) finalizeMerge(ctx context.Context, top *topology, imageID, configID string) error {
	imageDataset, configDataset := a.zfs.Dataset(imageID), a.zfs.Dataset(configID)
	if err := top.ensureRenamed(ctx, configDataset, imageDataset); err != nil {
		return err
	}
	if err := top.ensureSnapshot(ctx, imageDataset, storage.BaselineReduction); err != nil {
		return err
	}
	// 用原 ID 重建被合并消耗的配置：库仍指向它，客户机也从配置启动。
	if err := top.ensureCloned(ctx, a.zfs.Snapshot(imageID, storage.BaselineReduction), configDataset); err != nil {
		return err
	}
	return top.ensureSnapshot(ctx, configDataset, storage.BaselineReduction)
}

// destroyInDependencyOrder 删除一组可能互相依赖的数据集（配置派生配置、中断合并后
// 镜像挂在配置下）。依赖方向随池的历史变化，所以反复清扫无依赖者，整轮无进展才停。
func (a *Agent) destroyInDependencyOrder(ctx context.Context, top *topology, ids []string, keep string) error {
	remaining := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && id != keep {
			remaining = append(remaining, id)
		}
	}
	for len(remaining) > 0 {
		var stuck []string
		var lastErr error
		for _, id := range remaining {
			// 已不存在算完成：重试的合并会再次列出上次已删的配置。
			if err := top.ensureDestroyed(ctx, a.zfs.Dataset(id)); err != nil {
				stuck = append(stuck, id)
				lastErr = err
			}
		}
		if len(stuck) == len(remaining) {
			return lastErr
		}
		remaining = stuck
	}
	return nil
}

// Inventory 返回去掉池前缀的池内实际内容，便于直接与库记录比对。
func (a *Agent) Inventory(ctx context.Context) (storage.PoolInventory, error) {
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return storage.PoolInventory{}, err
	}
	snapshots, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		return storage.PoolInventory{}, err
	}
	origins, err := a.zfs.ListOrigins(ctx)
	if err != nil {
		return storage.PoolInventory{}, err
	}
	strip := func(names []string) []string {
		out := make([]string, 0, len(names))
		for _, name := range names {
			if trimmed, ok := a.poolRelative(name); ok {
				out = append(out, trimmed)
			}
		}
		return out
	}
	lineage := make(map[string]string, len(origins))
	for dataset, origin := range origins {
		child, okChild := a.poolRelative(dataset)
		parent, okParent := a.poolRelative(origin)
		if okChild && okParent {
			lineage[child] = parent
		}
	}
	return storage.PoolInventory{Datasets: strip(datasets), Snapshots: strip(snapshots), Origins: lineage}, nil
}

// SpaceUsage 列一次数据池，按库里的名字（去掉池前缀）返回各卷用量。
func (a *Agent) SpaceUsage(ctx context.Context) (storage.SpaceUsage, error) {
	usage, err := a.zfs.SpaceUsage(ctx)
	if err != nil {
		return storage.SpaceUsage{}, err
	}
	volumes := make(map[string]storage.VolumeUsage, len(usage.Volumes))
	for name, v := range usage.Volumes {
		if id, ok := a.poolRelative(name); ok {
			volumes[id] = storage.VolumeUsage{Size: v.Size, Used: v.Used}
		}
	}
	return storage.SpaceUsage{PoolUsed: usage.PoolUsed, PoolAvailable: usage.PoolAvailable, Volumes: volumes}, nil
}

// ConfigDependents 返回从配置任一快照克隆出的数据集（派生配置和客户机克隆），
// 它们都会阻止删除该配置。
func (a *Agent) ConfigDependents(ctx context.Context, configID string) ([]string, error) {
	configID = strings.TrimSpace(configID)
	if configID == "" {
		return nil, fmt.Errorf("config is required")
	}
	return a.zfs.ListDependentClones(ctx, a.zfs.Dataset(configID))
}

// ReductionDependents 返回从还原点快照克隆出的数据集。有依赖时 ZFS 拒绝删快照，
// 调用方先查并用自己的话拒绝，而不是透出 zfs 原文（它会建议 `destroy -R`）。
func (a *Agent) ReductionDependents(ctx context.Context, configID string, name string) ([]string, error) {
	configID = strings.TrimSpace(configID)
	snapshot := strings.TrimPrefix(strings.TrimSpace(name), "@")
	if configID == "" || snapshot == "" {
		return nil, fmt.Errorf("config and reduction are required")
	}
	clones, err := a.zfs.ListSnapshotClones(ctx, a.zfs.Snapshot(configID, snapshot))
	if err != nil && zfs.IsNotExist(err) {
		// 行还在、快照没了。上层要能把这一种和「池读不出来」分开：前者是一个
		// 已经损坏、必须能被清掉的还原点，后者是故障。
		return nil, fmt.Errorf("%w: %s", storage.ErrSnapshotMissing, a.zfs.Snapshot(configID, snapshot))
	}
	return clones, err
}

// Backup 把整个目录容器（镜像、配置、还原点、DB 副本）以一个一致快照的复制流送入备份池，
// 客户机克隆不在其中。增量基准每轮从两个池读取；promote 导致增量收不下时整体重建，
// 而不是每轮失败（见 docs/手测/递归复制-spike.md）。
func (a *Agent) Backup(ctx context.Context, req storage.BackupReq) (storage.BackupResult, error) {
	backupPool := strings.TrimSpace(req.BackupPool)
	if backupPool == "" {
		return storage.BackupResult{}, fmt.Errorf("backup_pool is required")
	}
	if backupPool == a.zfs.PoolName() {
		return storage.BackupResult{}, fmt.Errorf("backup_pool must differ from source pool")
	}
	snapshot := strings.TrimSpace(req.Snapshot)
	if snapshot == "" {
		return storage.BackupResult{}, fmt.Errorf("snapshot is required")
	}
	root := path.Join(a.zfs.PoolName(), storage.CatalogueRoot)
	target := path.Join(backupPool, "ndiskless", storage.CatalogueRoot)

	if !req.Reuse {
		if err := a.zfs.SnapshotRecursive(ctx, root, snapshot); err != nil {
			return storage.BackupResult{}, err
		}
	}
	base, targetExists, err := a.backupBase(ctx, root, target)
	if err != nil {
		return storage.BackupResult{}, err
	}
	if base == snapshot {
		base = "" // 本轮的半截副本：不能从自身做增量
	}
	if err := a.zfs.EnsureFilesystem(ctx, path.Dir(target)); err != nil {
		return storage.BackupResult{}, err
	}
	if base == "" && targetExists {
		// 没有共同快照的旧副本收不下完整流，挪开后整份重建。
		if err := a.rebuildBackupPreserving(ctx, root, target, snapshot); err != nil {
			return storage.BackupResult{}, err
		}
		return a.finishBackup(ctx, req, root, storage.BackupResult{})
	}
	if err := a.zfs.Replicate(ctx, root, target, base, snapshot); err != nil {
		if base == "" {
			return storage.BackupResult{}, err
		}
		// 增量收不下：基准之后合并或超管保存做过 promote。整体重建。
		slog.Warn("incremental backup not receivable; rebuilding the copy from scratch",
			"base", base, "error", err)
		if err := a.rebuildBackupPreserving(ctx, root, target, snapshot); err != nil {
			return storage.BackupResult{}, err
		}
		return a.finishBackup(ctx, req, root, storage.BackupResult{Base: "", FellBack: true})
	}
	return a.finishBackup(ctx, req, root, storage.BackupResult{Base: base})
}

func (a *Agent) finishBackup(ctx context.Context, req storage.BackupReq, root string, result storage.BackupResult) (storage.BackupResult, error) {
	if !req.Reuse {
		if err := a.zfs.PruneSnapshots(ctx, root, storage.BackupSnapshotPrefix, backupMarkersKept); err != nil {
			return result, err
		}
	}
	return result, nil
}

// rebuildBackupPreserving 先把旧副本改名挪开再收全量，成功后才删旧副本，失败则改回原名。
// 不能先 destroy 再收：备份池满、busy 或中途断开时会一份备份都不剩（同 Replicator.rebuildPreserving）。
func (a *Agent) rebuildBackupPreserving(ctx context.Context, root, target, snapshot string) error {
	aside := fmt.Sprintf("%s-rebuilding-%d", target, time.Now().UTC().UnixNano())
	if err := a.zfs.Rename(ctx, target, aside); err != nil {
		if !zfs.IsNotExist(err) {
			return err
		}
		aside = "" // 旧副本已不在，没有可保住的
	}
	if err := a.zfs.Replicate(ctx, root, target, "", snapshot); err != nil {
		if aside == "" {
			return err
		}
		// 失败的接收会在原名下留残片（recv -s），挡住改回原名；残片不能用，删掉放回旧副本。
		if drop := a.zfs.Destroy(ctx, target); drop != nil && !zfs.IsNotExist(drop) {
			return fmt.Errorf("备份重建失败，收到一半的残片挡住了原名：请人工删除 %s 后把 %s 改回 %s（原因：%w）",
				target, aside, target, err)
		}
		if back := a.zfs.Rename(ctx, aside, target); back != nil {
			return fmt.Errorf("备份重建失败且旧备份未能改回原名：请人工把 %s 改回 %s（原因：%w）", aside, target, err)
		}
		return err
	}
	// 新副本已完整落地；这一份和更早崩溃留下的挪开副本都不再需要。
	leftovers, err := a.zfs.ListAsideCopies(ctx, target)
	if err != nil {
		slog.Warn("backup: moved-aside copies not listed; they keep using space", "target", target, "error", err)
	}
	if aside != "" && !slices.Contains(leftovers, aside) {
		leftovers = append(leftovers, aside)
	}
	for _, old := range leftovers {
		if !strings.HasPrefix(old, target+"-rebuilding-") {
			continue
		}
		if err := a.zfs.Destroy(ctx, old); err != nil {
			slog.Warn("backup rebuilt but the old copy could not be removed", "leftover", old, "error", err)
		}
	}
	return nil
}

// BackupPoolName 返回本机数据池以外的那个池；只有数据池时返回空，不算错误。
func (a *Agent) BackupPoolName(ctx context.Context) (string, error) {
	names, err := a.zfs.ListPoolNames(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if n != a.zfs.PoolName() {
			return n, nil
		}
	}
	return "", nil
}

// LatestBackupMarker 返回本机目录根上最新的 ndbackup-* 快照。备机上的标记随复制到达，
// 不在本地创建。
func (a *Agent) LatestBackupMarker(ctx context.Context) (string, error) {
	snaps, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		return "", err
	}
	root := path.Join(a.zfs.PoolName(), storage.CatalogueRoot) + "@"
	newest := ""
	for _, s := range snaps {
		if !strings.HasPrefix(s, root) {
			continue
		}
		name := strings.TrimPrefix(s, root)
		if !strings.HasPrefix(name, storage.BackupSnapshotPrefix) {
			continue
		}
		if markerNewer(name, newest) {
			newest = name
		}
	}
	return newest, nil
}

// CopiedBackupMarker 返回备份池里已完整落地的最新标记，与本地标记比较决定本轮是否
// 要送。不依赖库记录：切换、恢复或手工 zfs send 之后，只有池本身可信。
func (a *Agent) CopiedBackupMarker(ctx context.Context, backupPool string) (string, error) {
	if strings.TrimSpace(backupPool) == "" {
		return "", nil
	}
	target := path.Join(backupPool, "ndiskless", storage.CatalogueRoot)
	inv, err := a.zfs.ListGUIDs(ctx, target)
	if err != nil {
		if zfs.IsNotExist(err) {
			return "", nil // 还没备份过
		}
		return "", err
	}
	var marks []string
	prefix := target + "@"
	for _, e := range inv.Entries {
		if name, ok := strings.CutPrefix(e.Name, prefix); ok && strings.HasPrefix(name, storage.BackupSnapshotPrefix) {
			marks = append(marks, name)
		}
	}
	sort.Slice(marks, func(i, j int) bool { return markerNewer(marks[i], marks[j]) })
	// 根数据集最先落地，所以要等源端打了该标记的每个数据集在副本里都有，这一轮才算数。
	root := path.Join(a.zfs.PoolName(), storage.CatalogueRoot)
	source, err := a.zfs.ListGUIDs(ctx, root)
	if err != nil {
		return "", err
	}
	for _, m := range marks {
		if roundInCopy(source, root, inv, target, m) {
			return m, nil
		}
	}
	return "", nil
}

// roundInCopy 判断某轮标记是否已完整到达副本；源端已清掉的标记无法核对，算作到达。
func roundInCopy(source zfs.GUIDInventory, root string, copyInv zfs.GUIDInventory, target, marker string) bool {
	have := map[string]bool{}
	for _, e := range copyInv.Entries {
		if rel, ok := strings.CutPrefix(e.Name, target); ok {
			have[rel] = true
		}
	}
	for _, e := range source.Entries {
		rel, ok := strings.CutPrefix(e.Name, root)
		if !ok || !strings.HasSuffix(rel, "@"+marker) {
			continue
		}
		if !have[rel] {
			return false
		}
	}
	return true
}

// SnapshotCatalogue 打本轮递归标记并清掉最旧的，限制备份池保留的轮数。
func (a *Agent) SnapshotCatalogue(ctx context.Context, name string) error {
	root := path.Join(a.zfs.PoolName(), storage.CatalogueRoot)
	if err := a.zfs.SnapshotRecursive(ctx, root, name); err != nil {
		return err
	}
	return a.zfs.PruneSnapshots(ctx, root, storage.BackupSnapshotPrefix, backupMarkersKept)
}

// markerNewer 按数字比较 ndbackup-<n>。按字符串比较在进位时出错，选错基准会让每轮
// 都静默变成全量。
func markerNewer(a, b string) bool {
	if b == "" {
		return true
	}
	ai, aok := markerSeq(a)
	bi, bok := markerSeq(b)
	if aok && bok {
		return ai > bi
	}
	return a > b
}

func markerSeq(name string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(name, storage.BackupSnapshotPrefix), 10, 64)
	return n, err == nil
}

// backupMarkersKept 是数据池保留的 ndbackup-* 数量。副本跟随发送端删除（-R 配 -F），
// 只需修剪发送端；3 个够覆盖进行中的一轮外加一轮被手工删掉的情况。
const backupMarkersKept = 3

// backupBase 按 GUID 选两池共有的最新快照；没有则为空，走全量。targetExists 区分
// 首次备份与必须先清掉才能收全量流的旧副本。
func (a *Agent) backupBase(ctx context.Context, root, target string) (base string, targetExists bool, err error) {
	source, err := a.zfs.ListGUIDs(ctx, root)
	if err != nil {
		return "", false, err
	}
	copyInv, err := a.zfs.ListGUIDs(ctx, target)
	if err != nil {
		if zfs.IsNotExist(err) {
			return "", false, nil // 首次备份
		}
		return "", false, err
	}
	return source.LatestCommonSnapshot(copyInv, root, target), true, nil
}

// EnsureDBCopyDir 返回每轮之前写入 DB 副本的目录，见 zfs.EnsureDBCopyDataset。
func (a *Agent) EnsureDBCopyDir(ctx context.Context) (string, error) {
	return a.zfs.EnsureDBCopyDataset(ctx)
}

func (a *Agent) SuperStop(ctx context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	mac := strings.TrimSpace(req.MAC)
	if mac == "" {
		return storage.SuperStopResult{}, fmt.Errorf("mac is required")
	}
	// 同一台机器的两次保存（双击，或与数据盘发布并发）会经同一个固定暂存名 promote 并改名
	// 同一配置，交错执行可能把配置改名丢失。
	defer a.lockClient(mac)()
	if err := a.deleteTarget(ctx, mac); err != nil {
		return storage.SuperStopResult{}, err
	}
	// 数据集销毁或改名前先删 backstore：DeleteTarget 只删 target，残留的 backstore
	// 既占住 zvol，又会被之后的导出静默复用，而它指向的已是改名后的卷。
	if err := a.deleteSuperBackstores(ctx, mac); err != nil {
		return storage.SuperStopResult{}, err
	}
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return storage.SuperStopResult{}, err
	}
	// 先核对全部要保存的盘：任何一块来源不对都整次拒绝，不能先把系统盘存进去。
	if err := a.checkSuperStopSources(ctx, top, mac, req); err != nil {
		return storage.SuperStopResult{}, err
	}
	var result storage.SuperStopResult
	// 先系统盘，再逐块数据盘，共用一份拓扑：上次中断的从断点继续，已完成的是空操作。
	if strings.TrimSpace(req.ReductionName) != "" {
		result.System, err = a.saveSuperClone(ctx, top, 0, storage.SuperClientCloneName(mac), req.ConfigID, req.ReductionName)
		if err != nil {
			return storage.SuperStopResult{}, err
		}
	}
	for _, disk := range req.DataDisks {
		if strings.TrimSpace(disk.ReductionName) == "" {
			continue
		}
		reduction, err := a.saveSuperClone(ctx, top, disk.LUN, storage.SuperClientDataCloneName(mac, disk.LUN), disk.ConfigID, disk.ReductionName)
		if err != nil {
			return storage.SuperStopResult{}, fmt.Errorf("data disk lun %d: %w", disk.LUN, err)
		}
		result.DataDisks = append(result.DataDisks, storage.SuperStopSavedDisk{LUN: disk.LUN, Reduction: reduction})
	}
	// 剩下的是未保存的盘，丢弃。
	for _, dataset := range superClientCloneDatasets(a.zfs, mac, top.datasets()) {
		if err := top.ensureDestroyed(ctx, dataset); err != nil {
			return storage.SuperStopResult{}, err
		}
	}
	return result, nil
}

func (a *Agent) deleteSuperBackstores(ctx context.Context, mac string) error {
	if a.exporter == nil {
		return nil
	}
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	luns := []int{}
	for _, dataset := range superClientCloneDatasets(a.zfs, mac, datasets) {
		if lun, ok := superClientCloneLUN(a.zfs, mac, dataset); ok {
			luns = append(luns, lun)
		}
	}
	return a.exporter.DeleteLUNs(ctx, iscsi.TargetForMAC(mac), luns)
}

// newerRestorePoints 列出超管机克隆之后该配置新建的还原点，即保存会销毁的那些。
// 系统快照不算，不能阻止保存。
func (a *Agent) newerRestorePoints(ctx context.Context, origins map[string]string, cloneDataset, configDataset string) ([]string, error) {
	dataset, snapshot, ok := strings.Cut(origins[cloneDataset], "@")
	if !ok || dataset != configDataset {
		return nil, nil // 已 promote，基准随之消失；来源由 checkSuperSource 把关
	}
	snaps, err := a.zfs.SnapshotsOf(ctx, configDataset)
	if err != nil {
		return nil, err
	}
	after := snaps
	for i, name := range snaps {
		if name == snapshot {
			after = snaps[i+1:]
			break
		}
	}
	var newer []string
	for _, name := range after {
		if storage.SystemSnapshot(name) {
			continue
		}
		newer = append(newer, "@"+name)
	}
	return newer, nil
}

// saveSuperClone 用超管机的持久克隆替换它所克隆的配置，并打成该配置的还原点。
// 系统盘与各数据盘走同样的步骤。
func (a *Agent) saveSuperClone(ctx context.Context, top *topology, lun int, cloneName, configID, reductionName string) (domain.Reduction, error) {
	configID = strings.TrimSpace(configID)
	snapshot := strings.TrimPrefix(strings.TrimSpace(reductionName), "@")
	if configID == "" || snapshot == "" {
		return domain.Reduction{}, fmt.Errorf("config and reduction are required")
	}
	superDataset := a.zfs.Dataset(cloneName)
	configDataset := a.zfs.Dataset(configID)
	// 用固定名而非带时间戳：配置挪开后中断，下次要能找回同一个数据集。
	oldDataset := a.zfs.Dataset(storage.SupersededConfigName(configID))

	// 替换是六步 ZFS 无法回滚的破坏性操作，中断在两次改名之间时配置会挂在库不认识的名字下。
	// 所以按池的实际阶段进入，已达目标的步骤视为完成，关机可直接重跑。
	// 池里没有这块盘的任何痕迹（克隆、暂存配置、已完成的快照）说明机器从未持有它
	// （如开机后才加入分组的数据盘），拒绝而不是凭空打还原点。
	if !top.has(superDataset) && !top.has(oldDataset) {
		saved, err := a.snapshotExists(ctx, configDataset, snapshot)
		if err != nil {
			return domain.Reduction{}, err
		}
		if !saved {
			return domain.Reduction{}, fmt.Errorf("%s does not exist: %w", superDataset, storage.SuperSessionMissing{LUN: lun})
		}
	}
	// 只在全新保存时检查：替换开始后（配置已挪开）再拒绝会让池停在半途无法继续。
	if top.has(superDataset) && !top.has(oldDataset) {
		origins, err := a.zfs.ListOrigins(ctx)
		if err != nil {
			return domain.Reduction{}, err
		}
		// 必须在 promote 之前：来源不对时后面的改名和 destroy -r 会删掉这个配置的历史。
		if err := a.checkSuperSource(origins, storage.Classify(cloneName).MAC, lun, superDataset, configID, true); err != nil {
			return domain.Reduction{}, err
		}
		newer, err := a.newerRestorePoints(ctx, origins, superDataset, configDataset)
		if err != nil {
			return domain.Reduction{}, err
		}
		if len(newer) > 0 {
			return domain.Reduction{}, storage.SuperBaseOutdated{LUN: lun, Newer: newer}
		}
	}
	if top.has(superDataset) {
		// 被替换的配置可能已在暂存名下（上次中断在两次改名之间）。
		if err := top.ensurePromoted(ctx, superDataset, configDataset, oldDataset); err != nil {
			return domain.Reduction{}, err
		}
		if top.has(configDataset) {
			// 更早一次关机残留的暂存数据集挡住了改名；它反正要在下面删掉。
			if err := top.ensureDestroyed(ctx, oldDataset); err != nil {
				return domain.Reduction{}, err
			}
			if err := top.ensureRenamed(ctx, configDataset, oldDataset); err != nil {
				return domain.Reduction{}, err
			}
		}
		if err := top.ensureRenamed(ctx, superDataset, configDataset); err != nil {
			return domain.Reduction{}, err
		}
	}
	// 先打快照再删暂存配置：快照出现前，暂存数据集是"正在保存"的唯一证据，
	// 靠它区分中断的保存与从未持有的盘。
	if err := top.ensureSnapshot(ctx, configDataset, snapshot); err != nil {
		return domain.Reduction{}, err
	}
	// 删掉暂存配置（可能是更早一次留下的）。
	if err := top.ensureDestroyed(ctx, oldDataset); err != nil {
		return domain.Reduction{}, err
	}
	return domain.Reduction{
		ID:        storage.ReductionID(configID, snapshot),
		ConfigID:  configID,
		Name:      "@" + snapshot,
		CreatedAt: time.Now().UTC(),
		Status:    domain.ReductionStatusReady,
	}, nil
}

func (a *Agent) snapshotExists(ctx context.Context, dataset, snapshot string) (bool, error) {
	snapshots, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		return false, err
	}
	want := dataset + "@" + snapshot
	for _, name := range snapshots {
		if name == want {
			return true, nil
		}
	}
	return false, nil
}

func (a *Agent) deleteTarget(ctx context.Context, mac string) error {
	if a.exporter == nil {
		return nil
	}
	return a.exporter.DeleteTarget(ctx, iscsi.TargetForMAC(mac))
}
func (a *Agent) CreatePool(ctx context.Context, name string, spec storage.PoolSpec) (domain.Pool, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(spec.Disks) == 0 {
		return domain.Pool{}, fmt.Errorf("pool name and disks are required")
	}
	cleanDisks := make([]string, 0, len(spec.Disks))
	for _, disk := range spec.Disks {
		if disk = strings.TrimSpace(disk); disk != "" {
			cleanDisks = append(cleanDisks, disk)
		}
	}
	if len(cleanDisks) == 0 {
		return domain.Pool{}, fmt.Errorf("pool disks are required")
	}
	if spec.Layout == "" {
		spec.Layout = domain.PoolLayoutStripe
	}
	if spec.GroupWidth < 1 {
		spec.GroupWidth = 1
	}
	spec.Disks = cleanDisks
	if err := a.zfs.CreatePool(ctx, name, spec); err != nil {
		return domain.Pool{}, err
	}
	// 数据池承载目录，建好即创建 layout-v2 容器，赶在任何导入之前。其它池不动。
	if name == a.zfs.PoolName() {
		if err := a.zfs.EnsureLayout(ctx); err != nil {
			return domain.Pool{}, err
		}
	}
	return domain.Pool{
		ID:         storage.PoolID(a.server, name),
		ServerID:   a.server,
		Name:       name,
		Layout:     spec.Layout,
		GroupWidth: spec.GroupWidth,
		Disks:      cleanDisks,
	}, nil
}
func (a *Agent) DestroyPool(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("pool name is required")
	}
	// 销毁前先读成员盘：之后无从查询。盘上残留的 ZFS 标签会让它们显示为占用，
	// 销毁后重建的操作者将无盘可选。
	var members []string
	if status, err := a.zfs.PoolStatus(ctx, name); err == nil {
		for _, disk := range status.Disks {
			members = append(members, disk.Path)
		}
	}
	if err := a.zfs.DestroyPool(ctx, name); err != nil {
		return err
	}
	for _, device := range members {
		if err := a.zfs.LabelClear(ctx, device); err != nil {
			slog.Warn("pool destroyed but its disk still carries a label", "device", device, "error", err)
		}
	}
	return nil
}
func (a *Agent) AddDisk(ctx context.Context, name string, spec storage.PoolSpec) error {
	name = strings.TrimSpace(name)
	spec.Disks = cleanDisks(spec.Disks)
	if name == "" || len(spec.Disks) == 0 {
		return fmt.Errorf("pool name and disks are required")
	}
	if spec.Layout == "" {
		spec.Layout = domain.PoolLayoutStripe
	}
	return a.zfs.AddDisk(ctx, name, spec)
}

func (a *Agent) AddSpecial(ctx context.Context, name string, disks []string) error {
	name = strings.TrimSpace(name)
	clean := cleanDisks(disks)
	if name == "" || len(clean) < 2 {
		return fmt.Errorf("pool name and at least two special disks are required")
	}
	return a.zfs.AddSpecial(ctx, name, clean)
}

func (a *Agent) RemoveSpecial(ctx context.Context, name, group string) error {
	name, group = strings.TrimSpace(name), strings.TrimSpace(group)
	if name == "" || group == "" {
		return fmt.Errorf("pool name and special group are required")
	}
	return a.zfs.RemoveSpecial(ctx, name, group)
}

func (a *Agent) AddSpare(ctx context.Context, name string, disks []string) error {
	name = strings.TrimSpace(name)
	clean := cleanDisks(disks)
	if name == "" || len(clean) == 0 {
		return fmt.Errorf("pool name and spare disks are required")
	}
	return a.zfs.AddSpare(ctx, name, clean)
}

func (a *Agent) RemoveSpare(ctx context.Context, name, disk string) error {
	name, disk = strings.TrimSpace(name), strings.TrimSpace(disk)
	if name == "" || disk == "" {
		return fmt.Errorf("pool name and spare disk are required")
	}
	members := a.memberPaths(ctx, name, disk)
	if err := a.zfs.RemoveSpare(ctx, name, disk); err != nil {
		return err
	}
	a.clearLabels(ctx, members)
	return nil
}

func (a *Agent) DetachDisk(ctx context.Context, name, disk string) error {
	name, disk = strings.TrimSpace(name), strings.TrimSpace(disk)
	if name == "" || disk == "" {
		return fmt.Errorf("pool name and disk are required")
	}
	members := a.memberPaths(ctx, name, disk)
	if err := a.zfs.DetachDisk(ctx, name, disk); err != nil {
		return err
	}
	a.clearLabels(ctx, members)
	return nil
}

func (a *Agent) AttachDisk(ctx context.Context, name, target, disk string) error {
	name, target, disk = strings.TrimSpace(name), strings.TrimSpace(target), strings.TrimSpace(disk)
	if name == "" || target == "" || disk == "" {
		return fmt.Errorf("pool name, target and disk are required")
	}
	return a.zfs.AttachDisk(ctx, name, target, disk)
}
func (a *Agent) RemoveDisk(ctx context.Context, name, disk string) error {
	name = strings.TrimSpace(name)
	disk = strings.TrimSpace(disk)
	if name == "" || disk == "" {
		return fmt.Errorf("pool name and disk are required")
	}
	return a.zfs.RemoveDisk(ctx, name, disk)
}
func (a *Agent) ReplaceDisk(ctx context.Context, name, oldDisk, newDisk string) error {
	name = strings.TrimSpace(name)
	oldDisk = strings.TrimSpace(oldDisk)
	newDisk = strings.TrimSpace(newDisk)
	if name == "" || oldDisk == "" || newDisk == "" {
		return fmt.Errorf("pool name and disks are required")
	}
	return a.zfs.ReplaceDisk(ctx, name, oldDisk, newDisk)
}
func (a *Agent) AddReadCache(ctx context.Context, name string, disks []string) error {
	name = strings.TrimSpace(name)
	cleanDisks := cleanDisks(disks)
	if name == "" || len(cleanDisks) == 0 {
		return fmt.Errorf("pool name and read cache disks are required")
	}
	return a.zfs.AddReadCache(ctx, name, cleanDisks)
}
func (a *Agent) RemoveReadCache(ctx context.Context, name, disk string) error {
	name = strings.TrimSpace(name)
	disk = strings.TrimSpace(disk)
	if name == "" || disk == "" {
		return fmt.Errorf("pool name and read cache disk are required")
	}
	members := a.memberPaths(ctx, name, disk)
	if err := a.zfs.RemoveReadCache(ctx, name, disk); err != nil {
		return err
	}
	a.clearLabels(ctx, members)
	return nil
}

// memberPaths 返回池自己对该盘的命名（打标签的分区）。须在移出盘之前读取，之后无从查询。
func (a *Agent) memberPaths(ctx context.Context, name, disk string) []string {
	status, err := a.zfs.PoolStatus(ctx, name)
	if err != nil {
		return nil
	}
	var paths []string
	for _, d := range status.Disks {
		if storage.SameDisk(d.Path, disk) {
			paths = append(paths, d.Path)
		}
	}
	return paths
}

// clearLabels 清掉盘离池后残留的 ZFS 标签，否则它仍显示为占用、无法再选。只用于立即
// 移出盘的操作（detach、移除 cache/log/spare）；正在疏散的数据 vdev 在移除完成前仍是成员。
func (a *Agent) clearLabels(ctx context.Context, paths []string) {
	for _, device := range paths {
		if err := a.zfs.LabelClear(ctx, device); err != nil {
			slog.Warn("disk left the pool but still carries a label", "device", device, "error", err)
		}
	}
}
func (a *Agent) AddWriteCache(ctx context.Context, name string, disks []string) error {
	name = strings.TrimSpace(name)
	cleanDisks := cleanDisks(disks)
	if name == "" || len(cleanDisks) == 0 {
		return fmt.Errorf("pool name and write cache disks are required")
	}
	return a.zfs.AddWriteCache(ctx, name, cleanDisks)
}
func (a *Agent) RemoveWriteCache(ctx context.Context, name, disk string) error {
	name = strings.TrimSpace(name)
	disk = strings.TrimSpace(disk)
	if name == "" || disk == "" {
		return fmt.Errorf("pool name and write cache disk are required")
	}
	members := a.memberPaths(ctx, name, disk)
	if err := a.zfs.RemoveWriteCache(ctx, name, disk); err != nil {
		return err
	}
	a.clearLabels(ctx, members)
	return nil
}
func (a *Agent) FlushWriteCache(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("pool name is required")
	}
	return a.zfs.FlushWriteCache(ctx, name)
}
func (a *Agent) PoolStatus(ctx context.Context, name string) (domain.PoolStatus, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.PoolStatus{}, fmt.Errorf("pool name is required")
	}
	status, err := a.zfs.PoolStatus(ctx, name)
	if err != nil {
		return domain.PoolStatus{}, err
	}
	out := domain.PoolStatus{
		PoolID:     storage.PoolID(a.server, name),
		Name:       status.Name,
		Health:     status.Health,
		Operation:  status.Operation,
		Progress:   status.Progress,
		Capacity:   status.Capacity,
		Used:       status.Used,
		Layout:     status.Layout,
		GroupWidth: status.GroupWidth,
		Vdevs:      status.Vdevs,
		Disks:      status.Disks,
	}
	// 只有 raidz 能逐盘扩展；其它布局不查，免得每次刷新多跑两条命令。
	if status.Layout.RaidzParity() > 0 {
		out.RaidzExpandable, out.RaidzExpandNote = a.zfs.RaidzExpansion(ctx, name)
	}
	return out, nil
}

// DetectOSType 按分区判断系统类型，有 NTFS 即 Windows。不需挂载，恢复目录时也够快。
func (a *Agent) DetectOSType(ctx context.Context, datasetID string) (domain.OSType, bool) {
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	dev, err := a.resolveNode(ctx, a.zfs.VolumePath(a.zfs.Dataset(datasetID)))
	if err != nil {
		return "", false
	}
	parts := waitPartitions(ctx, r, dev, anyFilesystem, probePartitionWait)
	if len(parts) == 0 {
		return "", false
	}
	if hasNTFS(parts) {
		return domain.OSTypeWindows, true
	}
	for _, p := range parts {
		switch strings.ToLower(p.FSType) {
		case "ext4", "ext3", "ext2", "xfs", "btrfs":
			return domain.OSTypeLinux, true
		}
	}
	return "", false
}

// ServerID 返回本节点标识。
func (a *Agent) ServerID() string { return a.server }

// DataPoolName 返回本 Agent 的数据池名。
func (a *Agent) DataPoolName() string { return a.zfs.PoolName() }

func (a *Agent) ListDisks(ctx context.Context) ([]storage.DiskInfo, error) {
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	out, err := r.Run(ctx, "lsblk", "-J", "-b", "-o", "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,MOUNTPOINT,FSTYPE")
	if err != nil {
		return nil, fmt.Errorf("list disks failed: %w", err)
	}

	var payload lsblkOutput
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("invalid lsblk output: %w", err)
	}

	disks := make([]storage.DiskInfo, 0, len(payload.Blockdevices))
	for _, disk := range payload.Blockdevices {
		if strings.ToLower(strings.TrimSpace(disk.Type)) != "disk" {
			continue
		}
		// lsblk 会把池自己的 zvol 报成 disk，列为建池候选就等于允许覆盖其中的镜像或克隆。
		if zfs.IsZvolNode(disk.Name) {
			continue
		}
		size, err := disk.Size.Int64()
		if err != nil {
			return nil, fmt.Errorf("invalid disk size %q for %q: %w", disk.Size, disk.Name, err)
		}
		// 零字节的"盘"（未连接的 nbd、空读卡器）无法建池，不列出。
		if size == 0 {
			continue
		}
		inUse, usedBy := diskInUse(disk)
		if !inUse {
			usedBy = ""
		}
		disks = append(disks, storage.DiskInfo{
			Path:   disk.Path,
			Name:   disk.Name,
			Type:   disk.Type,
			Size:   size,
			Model:  disk.Model,
			Serial: disk.Serial,
			InUse:  inUse,
			UsedBy: usedBy,
		})
	}
	return disks, nil
}

type lsblkOutput struct {
	Blockdevices []lsblkDisk `json:"blockdevices"`
}

type lsblkDisk struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Type       string      `json:"type"`
	Size       json.Number `json:"size"`
	Model      string      `json:"model"`
	Serial     string      `json:"serial"`
	FSType     string      `json:"fstype"`
	Mountpoint string      `json:"mountpoint"`
	Children   []lsblkDisk `json:"children"`
}

func diskInUse(disk lsblkDisk) (bool, string) {
	inUse := false
	usedBy := make([]string, 0, 4)
	if mountpoint := strings.TrimSpace(disk.Mountpoint); mountpoint != "" {
		inUse = true
		if mountpoint == "/" {
			usedBy = append(usedBy, "system:/")
		} else {
			usedBy = append(usedBy, mountpoint)
		}
	}
	if isZFSMember(disk.FSType) {
		inUse = true
		usedBy = append(usedBy, "zfs_member:"+usedByPath(disk))
	}
	for _, child := range disk.Children {
		childInUse, childUsedBy := diskInUse(child)
		if childInUse {
			inUse = true
		}
		if childUsedBy != "" {
			usedBy = append(usedBy, childUsedBy)
		}
	}
	return inUse, strings.Join(uniqueStrings(usedBy), ",")
}

func usedByPath(disk lsblkDisk) string {
	if strings.TrimSpace(disk.Path) != "" {
		return strings.TrimSpace(disk.Path)
	}
	return strings.TrimSpace(disk.Name)
}

func isZFSMember(fstype string) bool {
	return strings.EqualFold(strings.TrimSpace(fstype), "zfs_member")
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cleanDisks(disks []string) []string {
	cleanDisks := make([]string, 0, len(disks))
	for _, disk := range disks {
		if disk = strings.TrimSpace(disk); disk != "" {
			cleanDisks = append(cleanDisks, disk)
		}
	}
	return cleanDisks
}
