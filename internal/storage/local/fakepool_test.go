package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// fakePool 是 ZFS 接缝的测试替身：记录数据集、快照、克隆来源和占用，并像真 ZFS 一样拒绝非法操作
// （同名快照的 promote、销毁被克隆依赖的快照、非 ASCII 名、busy 的 zvol 等）。
// 什么都接受的替身会让测试通过而真机失败，这些规则一条都不能放宽。
type fakePool struct {
	mu                 sync.Mutex // 拷贝时 send 和 recv 在两个 goroutine 里同时记操作
	layoutEnsured      int
	recursiveSnaps     map[string]int
	replicates         []fakeReplicate
	replicateErrs      map[string]error
	guids              map[string][]string
	prunes             []string
	ensuredFilesystems map[string]int
	pool               string
	poolNames          []string
	datasets           []string
	snaps              map[string]bool   // "tank/nd/x@0"
	origin             map[string]string // 数据集 -> 克隆来源快照
	holds              map[string]string // 数据集 -> 占用原因（挂载、导出等）
	ops                []string          // 按顺序记录尝试过的修改操作
	destroyed          []string          // 按顺序记录实际销毁的数据集
	destroyAttempts    []string          // 每次 destroy 调用，无论成败
	volumes            []volumeSpec      // 创建过的卷及请求的大小

	// failOn 让第一个描述包含它的操作失败一次，用于中断多步流程后再续跑。
	failOn string
	failed bool
	// destroyErrs 是按数据集排队的 destroy 错误，每次弹出一个，用于测瞬时 busy 重试。
	destroyErrs map[string][]error
	written     map[string]int64
	used        map[string]int64
	// renameErrs 同 destroyErrs，作用于 rename。
	renameErrs map[string][]error
	// receiveSnapshot 是收下的流携带的快照名；真 `zfs receive` 总会连同数据集落下该快照。
	receiveSnapshot string
	// snapOrder 按创建顺序记录快照，供回滚使用。
	snapOrder []string
	// beforeOp 在下一个修改操作前执行一次，用于模拟两个请求交错。
	beforeOp func(op string)
}

func newFakePool() *fakePool {
	return &fakePool{
		pool:          "tank",
		snaps:         map[string]bool{},
		origin:        map[string]string{},
		holds:         map[string]string{},
		destroyErrs:   map[string][]error{},
		guids:         map[string][]string{},
		replicateErrs: map[string]error{},
		written:       map[string]int64{},
		used:          map[string]int64{},
	}
}

func (p *fakePool) Dataset(name string) string {
	return p.pool + "/" + storage.ContainerFor(name) + "/" + name
}
func (p *fakePool) PoolName() string                 { return p.pool }
func (p *fakePool) VolumePath(dataset string) string { return "/dev/zvol/" + dataset }
func (p *fakePool) Snapshot(cfg, red string) string  { return p.Dataset(cfg) + "@" + red }

func (p *fakePool) present(dataset string) bool {
	for _, d := range p.datasets {
		if d == dataset {
			return true
		}
	}
	return false
}

func (p *fakePool) stop(op string) error {
	p.mu.Lock()
	p.ops = append(p.ops, op)
	h := p.beforeOp
	p.beforeOp = nil
	fail := p.failOn != "" && !p.failed && strings.Contains(op, p.failOn)
	if fail {
		p.failed = true
	}
	p.mu.Unlock()
	if h != nil {
		h(op)
	}
	if fail {
		return errStop
	}
	return nil
}

// ZFS 的数据集名和快照名只接受有限字符集。
func poolNameError(full string) error {
	name := full
	if idx := strings.Index(full, "@"); idx >= 0 {
		name = full[idx+1:]
	} else if idx := strings.LastIndex(full, "/"); idx >= 0 {
		name = full[idx+1:]
	}
	if strings.Contains(name, "/") {
		return zfsErr("cannot create '" + full + "': trailing slash in name")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == ':' || r == '.' || r == ' ':
		default:
			return zfsErr("cannot create '" + full + "': invalid character '" + string(r) + "' in name")
		}
	}
	return nil
}

func (p *fakePool) Destroy(_ context.Context, dataset string) error {
	p.destroyAttempts = append(p.destroyAttempts, dataset)
	if err := p.stop("destroy " + dataset); err != nil {
		return err
	}
	if errs := p.destroyErrs[dataset]; len(errs) > 0 {
		err := errs[0]
		p.destroyErrs[dataset] = errs[1:]
		return err
	}
	if !p.present(dataset) {
		return zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	if why, held := p.holds[dataset]; held {
		return zfsErr("cannot destroy '" + dataset + "': dataset is busy (" + why + ")")
	}
	for ds, o := range p.origin {
		if strings.HasPrefix(o, dataset+"@") {
			return zfsErr("cannot destroy '" + dataset + "': volume has dependent clones\n" +
				"use '-R' to destroy the following datasets:\n" + ds)
		}
	}
	kept := p.datasets[:0]
	for _, d := range p.datasets {
		if d != dataset {
			kept = append(kept, d)
		}
	}
	p.datasets = kept
	for snap := range p.snaps {
		if strings.HasPrefix(snap, dataset+"@") {
			delete(p.snaps, snap)
		}
	}
	delete(p.origin, dataset)
	delete(p.holds, dataset)
	p.destroyed = append(p.destroyed, dataset)
	return nil
}

func (p *fakePool) DestroySnapshot(_ context.Context, dataset, snapshot string) error {
	full := dataset + "@" + snapshot
	if err := p.stop("destroy-snapshot " + full); err != nil {
		return err
	}
	if !p.snaps[full] {
		return zfsErr("could not find any snapshots to destroy; check snapshot names.")
	}
	for ds, o := range p.origin {
		if o == full {
			return zfsErr("cannot destroy '" + full + "': snapshot has dependent clones\n" +
				"use '-R' to destroy the following datasets:\n" + ds)
		}
	}
	delete(p.snaps, full)
	return nil
}

func (p *fakePool) SnapshotVolume(_ context.Context, dataset, snapshot string) error {
	full := dataset + "@" + snapshot
	if err := p.stop("snapshot " + full); err != nil {
		return err
	}
	if err := poolNameError(full); err != nil {
		return err
	}
	if !p.present(dataset) {
		return zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	if p.snaps[full] {
		return zfsErr("cannot create snapshot '" + full + "': dataset already exists")
	}
	p.snaps[full] = true
	p.snapOrder = append(p.snapOrder, full)
	return nil
}

func (p *fakePool) Clone(_ context.Context, snapshot, dataset string) error {
	if err := p.stop("clone " + dataset); err != nil {
		return err
	}
	if err := poolNameError(dataset); err != nil {
		return err
	}
	if p.present(dataset) {
		return zfsErr("cannot create '" + dataset + "': dataset already exists")
	}
	if !p.snaps[snapshot] {
		return zfsErr("cannot open '" + snapshot + "': dataset does not exist")
	}
	p.datasets = append(p.datasets, dataset)
	p.origin[dataset] = snapshot
	return nil
}

// volumeSpec 记录建卷参数；大小和块大小是需要断言的决策，不是附带参数。
type volumeSpec struct {
	dataset             string
	sizeBytes, volblock int64
}

func (p *fakePool) CreateVolume(_ context.Context, dataset string, sizeBytes, volblockBytes int64) error {
	p.volumes = append(p.volumes, volumeSpec{dataset: dataset, sizeBytes: sizeBytes, volblock: volblockBytes})
	if err := p.stop("create " + dataset); err != nil {
		return err
	}
	if err := poolNameError(dataset); err != nil {
		return err
	}
	if p.present(dataset) {
		return zfsErr("cannot create '" + dataset + "': dataset already exists")
	}
	p.datasets = append(p.datasets, dataset)
	return nil
}

func (p *fakePool) Promote(_ context.Context, dataset string) error {
	if err := p.stop("promote " + dataset); err != nil {
		return err
	}
	if !p.present(dataset) {
		return zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	parentSnap, ok := p.origin[dataset]
	if !ok {
		return zfsErr("cannot promote '" + dataset + "': not a cloned filesystem")
	}
	parent := parentSnap[:strings.Index(parentSnap, "@")]
	// promote 会把父级快照挪到克隆上，两者有同名快照时 ZFS 直接拒绝。
	for snap := range p.snaps {
		if !strings.HasPrefix(snap, dataset+"@") {
			continue
		}
		name := snap[strings.Index(snap, "@")+1:]
		if p.snaps[parent+"@"+name] {
			return zfsErr("cannot promote '" + dataset + "': conflicting snapshot '" + name +
				"' from parent '" + parent + "@" + name + "'")
		}
	}
	for snap := range p.snaps {
		if strings.HasPrefix(snap, parent+"@") {
			delete(p.snaps, snap)
			p.snaps[dataset+"@"+snap[strings.Index(snap, "@")+1:]] = true
		}
	}
	// 克隆接替父级在链中的位置：继承父级的来源（父级是根则无），父级变成它的克隆。
	parentOrigin, parentWasClone := p.origin[parent]
	delete(p.origin, dataset)
	for ds, o := range p.origin {
		if strings.HasPrefix(o, parent+"@") {
			p.origin[ds] = dataset + "@" + o[strings.Index(o, "@")+1:]
		}
	}
	if parentWasClone {
		p.origin[dataset] = parentOrigin
	}
	p.origin[parent] = dataset + "@" + parentSnap[strings.Index(parentSnap, "@")+1:]
	return nil
}

func (p *fakePool) Rename(_ context.Context, source, dataset string) error {
	if err := p.stop("rename " + source); err != nil {
		return err
	}
	if errs := p.renameErrs[source]; len(errs) > 0 {
		err := errs[0]
		p.renameErrs[source] = errs[1:]
		return err
	}
	if err := poolNameError(dataset); err != nil {
		return err
	}
	if p.present(dataset) {
		return zfsErr("cannot rename '" + source + "': dataset already exists")
	}
	if !p.present(source) {
		return zfsErr("cannot open '" + source + "': dataset does not exist")
	}
	for i, d := range p.datasets {
		if d == source {
			p.datasets[i] = dataset
		}
	}
	for snap := range p.snaps {
		if strings.HasPrefix(snap, source+"@") {
			delete(p.snaps, snap)
			p.snaps[dataset+"@"+snap[strings.Index(snap, "@")+1:]] = true
		}
	}
	if o, ok := p.origin[source]; ok {
		delete(p.origin, source)
		p.origin[dataset] = o
	}
	for ds, o := range p.origin {
		if strings.HasPrefix(o, source+"@") {
			p.origin[ds] = dataset + "@" + o[strings.Index(o, "@")+1:]
		}
	}
	return nil
}

func (p *fakePool) ListDatasets(context.Context) ([]string, error) {
	return append([]string(nil), p.datasets...), nil
}

func (p *fakePool) ListOrigins(context.Context) (map[string]string, error) {
	out := make(map[string]string, len(p.origin))
	for dataset, origin := range p.origin {
		out[dataset] = origin
	}
	return out, nil
}

// SnapshotsOf 按名字排序；夹具按拍摄顺序命名快照，足够替身使用。真实现按 createtxg 排序。
func (p *fakePool) SnapshotsOf(_ context.Context, dataset string) ([]string, error) {
	var own []string
	for full := range p.snaps {
		if ds, snap, ok := strings.Cut(full, "@"); ok && ds == dataset {
			own = append(own, snap)
		}
	}
	sort.Strings(own)
	return own, nil
}

func (p *fakePool) ListSnapshots(context.Context) ([]string, error) {
	out := make([]string, 0, len(p.snaps))
	for s := range p.snaps {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func (p *fakePool) ListDependentClones(_ context.Context, dataset string) ([]string, error) {
	var out []string
	for ds, o := range p.origin {
		if strings.HasPrefix(o, dataset+"@") {
			out = append(out, ds)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *fakePool) ListSnapshotClones(_ context.Context, snapshot string) ([]string, error) {
	var out []string
	for ds, o := range p.origin {
		if o == snapshot {
			out = append(out, ds)
		}
	}
	sort.Strings(out)
	return out, nil
}

// state 以排序列表返回整个池，用于断言终态。
func (p *fakePool) state() []string {
	out := append([]string(nil), p.datasets...)
	for s := range p.snaps {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// hold 像挂载或 iSCSI 导出那样把数据集标记为 busy。
func (p *fakePool) hold(dataset, why string) { p.holds[dataset] = why }

type fakeReplicate struct {
	root, target, base, snapshot string
}

func (p *fakePool) SnapshotRecursive(_ context.Context, root, name string) error {
	if p.recursiveSnaps == nil {
		p.recursiveSnaps = map[string]int{}
	}
	p.recursiveSnaps[root+"@"+name]++
	return nil
}

func (p *fakePool) Replicate(_ context.Context, root, target, base, snapshot string) error {
	p.replicates = append(p.replicates, fakeReplicate{root: root, target: target, base: base, snapshot: snapshot})
	if err, ok := p.replicateErrs[base]; ok {
		delete(p.replicateErrs, base)
		return err
	}
	return nil
}

// guids 把根映射到 "name\tguid" 行；缺失的根视为数据集不存在，对应首次备份时的目标池。
func (p *fakePool) ListGUIDs(_ context.Context, root string) (zfs.GUIDInventory, error) {
	rows, ok := p.guids[root]
	if !ok {
		return zfs.GUIDInventory{}, storage.CommandError{Name: "zfs", Output: "cannot open '" + root + "': dataset does not exist", Err: errFake}
	}
	inv := zfs.GUIDInventory{}
	for _, row := range rows {
		parts := strings.SplitN(row, "\t", 3)
		e := zfs.GUIDEntry{Name: parts[0]}
		if len(parts) > 1 {
			e.GUID = parts[1]
		}
		if len(parts) > 2 {
			e.Origin = parts[2]
		}
		inv.Entries = append(inv.Entries, e)
	}
	return inv, nil
}

func (p *fakePool) PruneSnapshots(_ context.Context, root, prefix string, keep int) error {
	p.prunes = append(p.prunes, fmt.Sprintf("%s|%s|%d", root, prefix, keep))
	return nil
}

func (p *fakePool) EnsureFilesystem(_ context.Context, dataset string) error {
	if p.ensuredFilesystems == nil {
		p.ensuredFilesystems = map[string]int{}
	}
	p.ensuredFilesystems[dataset]++
	return nil
}

func (p *fakePool) EnsureDBCopyDataset(context.Context) (string, error) {
	return "/tank/nd/db", nil
}

func (p *fakePool) ListPoolNames(_ context.Context) ([]string, error) {
	if p.poolNames == nil {
		return []string{p.pool}, nil
	}
	return append([]string{}, p.poolNames...), nil
}

// ReceiveFile 像 zfs recv 一样创建数据集，否则后续导入步骤操作的是池里不存在的数据集。
func (p *fakePool) ReceiveFile(_ context.Context, _ string, dataset string, onProgress func(int)) error {
	if err := p.stop("receive " + dataset); err != nil {
		return err
	}
	if !p.present(dataset) {
		p.datasets = append(p.datasets, dataset)
	}
	if p.receiveSnapshot != "" {
		p.snaps[dataset+"@"+p.receiveSnapshot] = true
	}
	if onProgress != nil {
		onProgress(100)
	}
	return nil
}

func (p *fakePool) SendSnapshot(_ context.Context, snapshot string, w io.Writer) error {
	if err := p.stop("send " + snapshot); err != nil {
		return err
	}
	if !p.snaps[snapshot] {
		return zfsErr("cannot open '" + snapshot + "': snapshot does not exist")
	}
	_, err := w.Write([]byte("STREAM:" + snapshot))
	return err
}

func (p *fakePool) SendSize(_ context.Context, snapshot string) (int64, error) {
	if !p.snaps[snapshot] {
		return 0, zfsErr("cannot open '" + snapshot + "': snapshot does not exist")
	}
	return int64(len("STREAM:" + snapshot)), nil
}

func (p *fakePool) Referenced(_ context.Context, dataset string) (int64, error) {
	if !p.present(dataset) {
		return 0, zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	return 4 << 30, nil
}

// written 是假体里可设的"这块盘改了多少"；数据集不存在就和真 zfs 一样报错。
func (p *fakePool) Written(_ context.Context, dataset string) (int64, error) {
	if !p.present(dataset) {
		return 0, zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	return p.written[dataset], nil
}

func (p *fakePool) Used(_ context.Context, dataset string) (int64, error) {
	if !p.present(dataset) {
		return 0, zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	return p.used[dataset], nil
}

// ListAsideCopies 与真实现一致：返回 root 的兄弟中名为 "-rebuilding-" 或 "-diverged-" 加时间戳的数据集。
func (p *fakePool) ListAsideCopies(_ context.Context, root string) ([]string, error) {
	var out []string
	for _, d := range p.datasets {
		for _, marker := range []string{root + "-rebuilding-", root + "-diverged-"} {
			if rest, ok := strings.CutPrefix(d, marker); ok && !strings.Contains(rest, "/") {
				out = append(out, d)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *fakePool) CreatePool(context.Context, string, storage.PoolSpec) error { return nil }
func (p *fakePool) EnsureLayout(context.Context) error {
	p.layoutEnsured++
	return nil
}
func (p *fakePool) DestroyPool(context.Context, string) error                 { return nil }
func (p *fakePool) LabelClear(context.Context, string) error                  { return nil }
func (p *fakePool) AddDisk(context.Context, string, storage.PoolSpec) error   { return nil }
func (p *fakePool) AttachDisk(context.Context, string, string, string) error  { return nil }
func (p *fakePool) DetachDisk(context.Context, string, string) error          { return nil }
func (p *fakePool) AddSpecial(context.Context, string, []string) error        { return nil }
func (p *fakePool) RemoveSpecial(context.Context, string, string) error       { return nil }
func (p *fakePool) AddSpare(context.Context, string, []string) error          { return nil }
func (p *fakePool) RemoveSpare(context.Context, string, string) error         { return nil }
func (p *fakePool) RaidzExpansion(context.Context, string) (bool, string)     { return false, "" }
func (p *fakePool) RemoveDisk(context.Context, string, string) error          { return nil }
func (p *fakePool) ReplaceDisk(context.Context, string, string, string) error { return nil }
func (p *fakePool) AddReadCache(context.Context, string, []string) error      { return nil }
func (p *fakePool) RemoveReadCache(context.Context, string, string) error     { return nil }
func (p *fakePool) AddWriteCache(context.Context, string, []string) error     { return nil }
func (p *fakePool) RemoveWriteCache(context.Context, string, string) error    { return nil }
func (p *fakePool) FlushWriteCache(context.Context, string) error             { return nil }
func (p *fakePool) PoolStatus(context.Context, string) (zfs.PoolStatus, error) {
	return zfs.PoolStatus{}, nil
}
func (p *fakePool) SpaceUsage(context.Context) (zfs.SpaceUsage, error) {
	return zfs.SpaceUsage{Volumes: map[string]zfs.VolumeUsage{}}, nil
}

// VolumeSize 返回建卷时的大小；收下的 zvol 没有记录，但真池仍能回答，所以返回 0 而非错误。
func (p *fakePool) VolumeSize(_ context.Context, dataset string) (int64, error) {
	if !p.present(dataset) {
		return 0, zfsErr("cannot open '" + dataset + "': dataset does not exist")
	}
	for _, v := range p.volumes {
		if v.dataset == dataset {
			return v.sizeBytes, nil
		}
	}
	return 0, nil
}

func zfsErr(output string) error {
	return storage.CommandError{Name: "zfs", Output: output, Err: errStop}
}

type stopErr struct{}

func (stopErr) Error() string { return "injected failure" }

var errStop = stopErr{}

var _ ZFS = (*fakePool)(nil)

// poolWith 构造只需要数据集存在的池。
func poolWith(datasets ...string) *fakePool {
	p := newFakePool()
	p.datasets = append(p.datasets, datasets...)
	return p
}

// 替身必须像真 ZFS 一样拒绝这些操作，否则测试会掩盖真机上的失败。
func TestFakePoolRefusesWhatZFSRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(*fakePool) error
		want string
	}{
		{
			name: "非 ASCII 数据集名",
			run: func(p *fakePool) error {
				return p.CreateVolume(ctx, "tank/教学镜像", 1, 1)
			},
			want: "invalid character",
		},
		{
			name: "快照名带斜杠",
			run: func(p *fakePool) error {
				p.datasets = []string{"tank/nd/cfg"}
				return p.SnapshotVolume(ctx, "tank/nd/cfg", "a/b")
			},
			want: "trailing slash",
		},
		{
			name: "销毁被挂载占用的数据集",
			run: func(p *fakePool) error {
				p.datasets = []string{"tank/nd/cfg"}
				p.hold("tank/nd/cfg", "mounted")
				return p.Destroy(ctx, "tank/nd/cfg")
			},
			want: "dataset is busy",
		},
		{
			name: "销毁仍被克隆依赖的数据集",
			run: func(p *fakePool) error {
				p.datasets = []string{"tank/nd/cfg", "tank/nd/fork"}
				p.snaps["tank/nd/cfg@0"] = true
				p.origin["tank/nd/fork"] = "tank/nd/cfg@0"
				return p.Destroy(ctx, "tank/nd/cfg")
			},
			want: "has dependent clones",
		},
		{
			name: "提升与父同名快照的克隆",
			run: func(p *fakePool) error {
				p.datasets = []string{"tank/nd/img", "tank/nd/cfg"}
				p.snaps["tank/nd/img@0"] = true
				p.snaps["tank/nd/cfg@0"] = true
				p.origin["tank/nd/cfg"] = "tank/nd/img@0"
				return p.Promote(ctx, "tank/nd/cfg")
			},
			want: "conflicting snapshot",
		},
		{
			name: "重命名到已存在的名字",
			run: func(p *fakePool) error {
				p.datasets = []string{"tank/nd/a", "tank/nd/b"}
				return p.Rename(ctx, "tank/nd/a", "tank/nd/b")
			},
			want: "already exists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(newFakePool())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

var errFake = errors.New("exit status 1")

// ReceiveStream 像真实全量接收一样落下数据集及流所基于的快照。
func (p *fakePool) ReceiveStream(_ context.Context, dataset string, r io.Reader) error {
	if err := p.stop("receive " + dataset); err != nil {
		return err
	}
	if p.present(dataset) {
		return zfsErr("cannot receive new filesystem stream: destination '" + dataset + "' exists")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, snap, ok := strings.Cut(strings.TrimPrefix(string(b), "STREAM:"), "@")
	if !ok {
		return zfsErr("invalid stream")
	}
	p.datasets = append(p.datasets, dataset)
	p.snaps[dataset+"@"+snap] = true
	return nil
}

func (p *fakePool) RollbackVolume(_ context.Context, dataset, snapshot string) error {
	full := dataset + "@" + snapshot
	if err := p.stop("rollback " + full); err != nil {
		return err
	}
	if !p.snaps[full] {
		return zfsErr("cannot open '" + full + "': dataset does not exist")
	}
	later := false
	for _, s := range p.snapOrder {
		if s == full {
			later = true
			continue
		}
		if later && strings.HasPrefix(s, dataset+"@") {
			delete(p.snaps, s)
		}
	}
	return nil
}
