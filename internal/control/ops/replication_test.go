package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

type fakeReplZFS struct {
	guids         map[string]zfs.GUIDInventory
	listErr       map[string]error
	snapshots     []string
	detached      []string
	prunes        []string
	received      []string // 喂给 ReceiveReplication 的流内容
	recvErrs      []error  // 每次调用弹出一个
	destroyed     []string
	renamed       []string
	resumeTok     string
	resumeDS      string   // 令牌所在的数据集；空 = 根
	recvTargets   []string // ReceiveReplication 收进了哪个数据集
	abortedDS     string
	resumeAborted bool
	abortErr      error // recv -A 也可能失败，报 dataset already exists
	dbCopyDir     string
	// needDestroyFirst 复刻 zfs 规则：目标数据集还带快照时拒收全量流。
	needDestroyFirst bool
	// partialOnErr 让失败的收流留下一份收到一半的副本，占住目的端那个名字。
	partialOnErr bool
	// pruneErr 模拟备机正用 zfs send 读标记快照时，删除报 dataset is busy。
	pruneErr error
	// siblings 是 <root> 旁边已经存在的挪开副本（上几轮失败留下的）。
	siblings []string
	// renameErr 模拟 rename 失败，如残留挂载让 tank/nd 卸不下来。
	renameErr error
	// datasetRecvErr 让追平轮的单个数据集接收失败。
	datasetRecvErr error
	// onRecvErr 在收流失败时改写本机状态：递归增量失败时，流里靠前的根和镜像已收下这一轮，
	// 被顶替的配置则被 -F 回滚。
	onRecvErr func()
}

func (f *fakeReplZFS) ListAsideCopies(_ context.Context, root string) ([]string, error) {
	out := []string{}
	for _, s := range f.siblings {
		if !contains(f.destroyed, s) {
			out = append(out, s)
		}
	}
	_ = root
	return out, nil
}

var errFakeRepl = fmt.Errorf("exit status 1")

func (f *fakeReplZFS) ListGUIDs(_ context.Context, root string) (zfs.GUIDInventory, error) {
	if err, ok := f.listErr[root]; ok {
		return zfs.GUIDInventory{}, err
	}
	return f.guids[root], nil
}
func (f *fakeReplZFS) SnapshotRecursive(_ context.Context, root, name string) error {
	f.snapshots = append(f.snapshots, root+"@"+name)
	return nil
}
func (f *fakeReplZFS) PruneSnapshotsExcept(_ context.Context, root, prefix string, keep int, pinned []string) error {
	f.prunes = append(f.prunes, fmt.Sprintf("%s|%s|%d|%s", root, prefix, keep, strings.Join(pinned, ",")))
	return f.pruneErr
}
func (f *fakeReplZFS) ReceiveReplication(_ context.Context, root string, r io.Reader) error {
	f.recvTargets = append(f.recvTargets, root)
	b, _ := io.ReadAll(r)
	f.received = append(f.received, string(b))
	if f.needDestroyFirst && !strings.Contains(string(b), "STREAM:rep-") {
		// 销毁或改名挪走都算腾空了目的端，真实 zfs 都认；只认销毁会错误拦下「挪开保留」。
		cleared := false
		for _, d := range f.destroyed {
			if d == root {
				cleared = true
			}
		}
		for _, r := range f.renamed {
			if strings.HasPrefix(r, root+"->") {
				cleared = true
			}
		}
		if !cleared {
			return fmt.Errorf("cannot receive new filesystem stream: destination has snapshots (eg. %s@rep-999)\nmust destroy them to overwrite it", root)
		}
	}
	if len(f.recvErrs) > 0 {
		err := f.recvErrs[0]
		f.recvErrs = f.recvErrs[1:]
		if f.onRecvErr != nil {
			f.onRecvErr()
		}
		// 全量流失败时 zfs 会把收到一半的残片存在原本不存在的名字下（"Partially received snapshot is saved"），
		// 占住该名字。增量流的目的端本来就在，所以只模拟全量。
		if _, exists := f.guids[root]; f.partialOnErr && !exists {
			f.guids[root] = zfs.GUIDInventory{Entries: []zfs.GUIDEntry{
				{Name: root + "/partial", GUID: "partial-1"}}}
		}
		return err
	}
	return nil
}
func (f *fakeReplZFS) DetachMounts(_ context.Context, dataset string) error {
	f.detached = append(f.detached, dataset)
	return nil
}
func (f *fakeReplZFS) Rename(_ context.Context, source, dataset string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	f.renamed = append(f.renamed, source+"->"+dataset)
	if inv, ok := f.guids[source]; ok {
		f.guids[dataset] = inv
		delete(f.guids, source)
	}
	return nil
}

func (f *fakeReplZFS) ReceiveDataset(_ context.Context, dataset string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.received = append(f.received, dataset+" <- "+string(b))
	return f.datasetRecvErr
}

func (f *fakeReplZFS) Destroy(_ context.Context, dataset string) error {
	f.destroyed = append(f.destroyed, dataset)
	delete(f.guids, dataset)
	return nil
}
func (f *fakeReplZFS) ResumeToken(_ context.Context, root string) (string, string, error) {
	if f.resumeTok == "" {
		return "", "", nil
	}
	if f.resumeDS != "" {
		return f.resumeDS, f.resumeTok, nil
	}
	return root, f.resumeTok, nil
}

// AbortResume 丢弃收到一半的状态，真实的 zfs 也是这么干的：令牌随之消失。
func (f *fakeReplZFS) AbortResume(_ context.Context, root string) error {
	if f.abortErr != nil {
		return f.abortErr
	}
	f.abortedDS = root
	f.resumeAborted = true
	f.resumeTok = ""
	return nil
}
func (f *fakeReplZFS) EnsureDBCopyDataset(context.Context) (string, error) {
	return f.dbCopyDir, nil
}

type fakeReplPeer struct {
	inv       ha.ReplicationInventory
	streams   []string
	confirmed []string
}

func (f *fakeReplPeer) Inventory(context.Context) (ha.ReplicationInventory, error) {
	return f.inv, nil
}
func (f *fakeReplPeer) Stream(_ context.Context, from, to string) (io.ReadCloser, error) {
	f.streams = append(f.streams, from+"->"+to)
	return io.NopCloser(strings.NewReader("STREAM:" + from + "->" + to)), nil
}
func (f *fakeReplPeer) StreamDataset(_ context.Context, rel, from, origin, to string) (io.ReadCloser, error) {
	f.streams = append(f.streams, fmt.Sprintf("dataset %q from %q origin %q to %q", rel, from, origin, to))
	return io.NopCloser(strings.NewReader("DATASET:" + rel)), nil
}
func (f *fakeReplPeer) Confirm(_ context.Context, marker string) error {
	f.confirmed = append(f.confirmed, marker)
	return nil
}
func (f *fakeReplPeer) StreamResume(_ context.Context, token string) (io.ReadCloser, error) {
	f.streams = append(f.streams, "resume:"+token)
	return io.NopCloser(strings.NewReader("RESUME:" + token)), nil
}

func replTestClock() func() time.Time {
	t := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// 只有目录真的变化才打一轮（记账快照不计入指纹），且每轮先写数据库副本。
func TestMarkOnceFingerprintsAndSnapshots(t *testing.T) {
	ctx := context.Background()
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd/win11", GUID: "1"},
			{Name: "tank/nd/win11@0", GUID: "2"},
		}},
	}}
	dbWrites := 0
	rep := &Replicator{
		ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { dbWrites++; return nil },
	}

	marked, err := rep.MarkOnce(ctx)
	if err != nil || !marked {
		t.Fatalf("first mark = %v, %v", marked, err)
	}
	if len(z.snapshots) != 1 || !strings.HasPrefix(z.snapshots[0], "tank/nd@rep-") {
		t.Fatalf("snapshots = %v", z.snapshots)
	}
	if dbWrites != 1 {
		t.Fatalf("db writes = %d", dbWrites)
	}
	if len(z.prunes) != 1 || z.prunes[0] != "tank/nd|rep-|3|" {
		t.Fatalf("prunes = %v", z.prunes)
	}

	// 目录未变（新的 rep-* 标记本身不算）：不打新一轮
	z.guids["tank/nd"] = zfs.GUIDInventory{Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "1"},
		{Name: "tank/nd/win11@0", GUID: "2"},
		{Name: "tank/nd@rep-1755608400000000000", GUID: "99"},
	}}
	marked, err = rep.MarkOnce(ctx)
	if err != nil || marked {
		t.Fatalf("unchanged mark = %v, %v", marked, err)
	}

	// 新还原点改变指纹：再打一轮
	grown := append(z.guids["tank/nd"].Entries, zfs.GUIDEntry{Name: "tank/nd/win11_default@office", GUID: "3"})
	z.guids["tank/nd"] = zfs.GUIDInventory{Entries: grown}
	marked, _ = rep.MarkOnce(ctx)
	if !marked {
		t.Fatal("changed catalogue must mark")
	}
}

// 闸门关闭（备机）时从不打标记。
func TestMarkOnceRefusedByClosedGate(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("备机", "")
	rep := &Replicator{ZFS: &fakeReplZFS{}, Root: "tank/nd", Gate: gate, Now: replTestClock(), DBSnapshot: func(context.Context, string) error { return nil }}
	marked, err := rep.MarkOnce(context.Background())
	if err != nil || marked {
		t.Fatalf("closed gate marked: %v %v", marked, err)
	}
}

// 备机从最新共同快照增量拉取写入节点的内容，并在 replication_state 记下这一轮。
func TestPullOnceIncrementalAndState(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd@rep-1", GUID: "g1"}}},
	}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "pool2/nd", Entries: []zfs.GUIDEntry{
		{Name: "pool2/nd@rep-1", GUID: "g1"},
		{Name: "pool2/nd@rep-2", GUID: "g2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(peer.streams) != 1 || peer.streams[0] != "rep-1->rep-2" {
		t.Fatalf("streams = %v", peer.streams)
	}
	if len(z.received) != 1 || z.received[0] != "STREAM:rep-1->rep-2" {
		t.Fatalf("received = %v", z.received)
	}
	rows, err := st.ReplicationStates().List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("state rows = %#v err=%v", rows, err)
	}
	if rows[0].Target != "http://a:8080" || rows[0].LastSnapshot != "rep-2" || rows[0].LastOKAt == nil || rows[0].LastError != "" {
		t.Fatalf("state = %#v", rows[0])
	}
}

// 写入节点上的 promote 让增量收不进来时，备机自行整体重建。
func TestPullOnceFallsBackToFullResync(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		// 夹具里要有真资产：空目录会就地重收（见 TestRebuildDoesNotPreserveAnEmptyCatalogue），
		// 这里测的是有目录时的行为。
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "g1"},
			{Name: "tank/nd/win11", GUID: "d1"},
		}}},
		recvErrs: []error{fmt.Errorf("cannot receive incremental stream")},
	}
	// 对端也要有资产，否则会先被「拒绝用空目录覆盖」的护栏挡下。
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "g1"},
		{Name: "tank/nd@rep-2", GUID: "g2"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(peer.streams, ",") != "rep-1->rep-2,->rep-2" {
		t.Fatalf("streams = %v", peer.streams)
	}
	// 旧副本先改名再删，中途被打断时本机不会只剩空名字。
	if len(z.destroyed) != 1 || !strings.HasPrefix(z.destroyed[0], "tank/nd-rebuilding-") {
		t.Fatalf("destroyed = %v", z.destroyed)
	}
	if len(z.renamed) != 1 || !strings.HasPrefix(z.renamed[0], "tank/nd->tank/nd-rebuilding-") {
		t.Fatalf("重建必须先把旧那份改名挪开，不能直接销毁：renamed = %v", z.renamed)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || rows[0].LastSnapshot != "rep-2" {
		t.Fatalf("state = %#v", rows)
	}
}

// 已同步：什么都不动，状态行照样刷新心跳。
func TestPullOnceNoopWhenInStep(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd@rep-2", GUID: "g2"}}},
	}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-2", GUID: "g2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(peer.streams) != 0 {
		t.Fatalf("streams = %v", peer.streams)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || rows[0].LastSnapshot != "rep-2" {
		t.Fatalf("state = %#v", rows)
	}
}

// 同步到最新一轮后，重建残留都已没用，全部回收；被打断的重建靠续传和追平补齐时不经过 rebuildWhole，只能在这里收。
func TestPullOncePrunesAsideCopiesAfterSync(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{
			"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd@rep-2", GUID: "g2"}}},
		},
		siblings: []string{"tank/nd-rebuilding-100", "tank/nd-diverged-150", "tank/nd-rebuilding-200"},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-2", GUID: "g2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !contains(z.destroyed, "tank/nd-rebuilding-100") || !contains(z.destroyed, "tank/nd-rebuilding-200") {
		t.Fatalf("同步成功后重建残留应全部回收：destroyed=%v", z.destroyed)
	}
	if contains(z.destroyed, "tank/nd-diverged-150") {
		t.Fatalf("保住的分叉副本不该删：destroyed=%v", z.destroyed)
	}
}

// 有中断留下的续传令牌时先续传。
func TestPullOnceResumesInterruptedStream(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		resumeTok: "1-tok",
		guids: map[string]zfs.GUIDInventory{
			"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd@rep-2", GUID: "g2"}}},
		},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-2", GUID: "g2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(peer.streams) == 0 || peer.streams[0] != "resume:1-tok" {
		t.Fatalf("streams = %v", peer.streams)
	}
	if z.received[0] != "RESUME:1-tok" {
		t.Fatalf("received = %v", z.received)
	}
}

// 闸门开（写入节点）时从不拉取。
func TestPullOnceRefusedByOpenGate(t *testing.T) {
	peer := &fakeReplPeer{}
	rep := &Replicator{Store: newImageTestStore(t), ZFS: &fakeReplZFS{}, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}
	if err := rep.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(peer.streams) != 0 {
		t.Fatalf("open gate pulled: %v", peer.streams)
	}
}

// Status 按目标列出状态行，滞后由最后一次成功的轮次算出。
func TestReplicatorStatusAndLagAlarm(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)
	if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{
		Target: "http://a:8080", Kind: "standby-pull", Root: "tank/nd",
		LastSnapshot: "rep-1", LastOKAt: &old, LastError: "short read", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	rep := &Replicator{Store: st, Now: func() time.Time { return now }}
	status, err := rep.Status(ctx)
	if err != nil || len(status.Targets) != 1 {
		t.Fatalf("status = %#v err=%v", status, err)
	}
	tg := status.Targets[0]
	if tg.Target != "http://a:8080" || tg.LagSeconds != 600 || tg.LastError != "short read" {
		t.Fatalf("target = %#v", tg)
	}

}

// 全新备机还没有 nd/，首次拉取用全量流创建；本机根不存在按空处理而非报错。
func TestPullOnceFullWhenLocalRootMissing(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{}} // 本机根完全不存在
	z.listErr = map[string]error{"tank/nd": storage.CommandError{Name: "zfs", Output: "cannot open 'tank/nd': dataset does not exist", Err: errFakeRepl}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "g1"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(peer.streams) != 1 || peer.streams[0] != "->rep-1" {
		t.Fatalf("streams = %v", peer.streams)
	}
}

// 中断的 -R 流可能只落下容器自己的快照、子数据集缺失；同一标记还要求数据集齐全，
// 残缺副本整体重建。
func TestPullOnceRepairsPartialReceive(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-2", GUID: "g2"}, // 标记到了……
			{Name: "tank/nd/win11", GUID: "w"},  // ……子数据集却只到了一个
		}},
	}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "pool2/nd", Entries: []zfs.GUIDEntry{
		{Name: "pool2/nd@rep-2", GUID: "g2"},
		{Name: "pool2/nd/win11", GUID: "w2"},
		{Name: "pool2/nd/win11_default", GUID: "d2"}, // 本机缺失
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 残缺的那份同样是先挪开、收成功了再删——收不下来时它还得能改回去。
	if len(z.destroyed) != 1 || !strings.HasPrefix(z.destroyed[0], "tank/nd-rebuilding-") {
		t.Fatalf("partial copy must be destroyed: %v", z.destroyed)
	}
	if len(peer.streams) != 1 || peer.streams[0] != "->rep-2" {
		t.Fatalf("streams = %v, want a full rebuild", peer.streams)
	}
}

// 新装节点抢到 VRRP 时目录是空的，从它拉取会删掉真正有数据那台的目录。
// 本机必须拒绝、保留数据并记下原因。
func TestPullRefusesToOverwriteRealDataWithAnEmptyPeer(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	// 本机有真实目录。
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd", GUID: "l0"},
			{Name: "tank/nd/win11", GUID: "l1"},
			{Name: "tank/nd/win11_default", GUID: "l2"},
			{Name: "tank/nd/db", GUID: "l3"},
		}},
	}}
	// 对端只有保留子数据集，从未有过目录。
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd", GUID: "p0"},
		{Name: "tank/nd@rep-9", GUID: "p1"},
		{Name: "tank/nd/db", GUID: "p2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	err := rep.PullOnce(ctx)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("local catalogue destroyed — this is the data-loss bug: %v", z.destroyed)
	}
	if !strings.Contains(err.Error(), "空") {
		t.Fatalf("error should say the peer looks empty: %v", err)
	}
	states, _ := st.ReplicationStates().List(ctx)
	if len(states) == 0 || states[0].LastError == "" {
		t.Fatalf("refusal must be recorded so the alarm fires: %+v", states)
	}
}

// 反面：本机空、对端有目录的正常首次同步必须照常进行。
func TestPullAcceptsFullStreamOntoAnEmptyCopy(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{}}
	z.listErr = map[string]error{"tank/nd": storage.CommandError{Name: "zfs", Output: "cannot open 'tank/nd': dataset does not exist", Err: errFakeRepl}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "g1"},
		{Name: "tank/nd/win11", GUID: "g2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("first full sync must work: %v", err)
	}
}

// 只改数据库（注册机器、调分组、改 DHCP）也必须触发新一轮，否则这些改动要等下次导入镜像
// 才复制出去，故障切换会把它们回滚。
func TestMarkOnceMarksARoundWhenOnlyTheDatabaseChanged(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(dbPath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
	}}
	rep := &Replicator{ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		LiveDBPath: dbPath, DBSnapshot: func(context.Context, string) error { return nil }}

	if marked, err := rep.MarkOnce(ctx); err != nil || !marked {
		t.Fatalf("first round: %v %v", marked, err)
	}
	if marked, err := rep.MarkOnce(ctx); err != nil || marked {
		t.Fatalf("nothing changed, must not mark: %v %v", marked, err)
	}

	// 只有数据库变化，没有数据集增减。
	if err := os.WriteFile(dbPath, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	marked, err := rep.MarkOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("a database-only change must still mark a round — otherwise it never reaches the standby")
	}
}

// 角色互换后，原主机留下的 primary-serve 行在新主机上指向它自己；这是残留，
// 不能报成滞后、误触发告警。
func TestStatusDropsFossilTargets(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-40 * time.Second)
	stale := now.Add(-7 * time.Hour)

	for _, row := range []domain.ReplicationState{
		{Target: "http://10.0.0.3:8080", Kind: "primary-serve", Root: "tank/nd", LastOKAt: &fresh},
		{Target: "http://10.0.0.4:8080", Kind: "primary-serve", Root: "tank/nd", LastOKAt: &stale}, // itself
		{Target: "http://10.0.0.9:8080", Kind: "primary-serve", Root: "tank/nd", LastOKAt: &stale}, // a node that stopped pulling
	} {
		if err := st.ReplicationStates().Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	rep := &Replicator{Store: st, Now: func() time.Time { return now }, SelfURL: "http://10.0.0.4:8080"}

	status, err := rep.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range status.Targets {
		if tg.Target == "http://10.0.0.4:8080" {
			t.Fatal("a node must not report replication lag against itself")
		}
		if tg.Target == "http://10.0.0.9:8080" {
			t.Fatal("a peer that stopped pulling hours ago is a fossil, not a lagging target")
		}
	}
	if len(status.Targets) != 1 {
		t.Fatalf("targets = %+v", status.Targets)
	}
}

// 主机不拉取：它当备机时留下的 standby-pull 行目标是 VIP，按本机地址认不出，不该再显示和报警。
func TestStatusDropsPullRowsOnTheServingNode(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{Target: "http://10.0.0.250:8080", Kind: "standby-pull", Root: "tank/nd",
		LastError: "返回 503", UpdatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	gate := &ha.Gate{}
	rep := &Replicator{Store: st, Gate: gate, Now: func() time.Time { return now }, SelfURL: "http://10.0.0.3:8080"}
	status, err := rep.Status(ctx)
	if err != nil || len(status.Targets) != 0 {
		t.Fatalf("serving node still reports its old pull: %+v err=%v", status.Targets, err)
	}
	gate.Close("standby", "http://10.0.0.4:8080")
	if status, _ := rep.Status(ctx); len(status.Targets) != 1 {
		t.Fatalf("a standby must still report its pull: %+v", status.Targets)
	}
}

// 两侧谱系分叉、没有共同快照时只能收全量流，而 ZFS 拒绝把全量流收进已有快照的数据集：
//
//	cannot receive new filesystem stream: destination has snapshots
//	must destroy them to overwrite it
//
// 不先腾空名字复制就永久失败，备机拿不到数据库副本而无法激活，集群会持有 VIP 却没有主机。
func TestPullOnceRebuildsWhenLineagesDivergedCompletely(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	// 本机有自己的快照，但没有一个与对端相同——谱系分叉
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd/win11", GUID: "local-1"},
			{Name: "tank/nd@rep-999", GUID: "local-9"},
		}}},
		needDestroyFirst: true, // 未先销毁就收全量流时，真实的 zfs 就是这么拒绝的
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "peer-1"},
		{Name: "tank/nd@rep-1", GUID: "peer-a"},
		{Name: "tank/nd@rep-2", GUID: "peer-b"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("谱系分叉不该让复制永久停摆：%v", err)
	}
	// 要守的是不卡死：收全量流前必须腾空名字，销毁或改名挪开都算。
	cleared := len(z.destroyed) > 0 || len(z.renamed) > 0
	if !cleared {
		t.Fatalf("全量收之前必须先把本机那份腾开，否则 zfs 永远拒收：destroyed=%v renamed=%v",
			z.destroyed, z.renamed)
	}
	if len(z.received) == 0 || !strings.Contains(z.received[len(z.received)-1], "->rep-2") {
		t.Fatalf("最终要把对端最新的那一轮收下来：received=%v", z.received)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || rows[0].LastSnapshot != "rep-2" {
		t.Fatalf("收完要记下进度：%#v", rows)
	}
}

// 复制主循环不能因一次失败退出：对端重启、网络抖动、快照忙都是常态，
// 循环退出后备机会无声停止追平。
func TestReplicationLoopsKeepGoingAfterAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 备机侧：让前两轮拉取失败，第三轮成功，循环必须都跑到
	pulls := make(chan int, 8)
	var n int32
	standby := &Replicator{
		Store: newImageTestStore(t), ZFS: &fakeReplZFS{}, Root: "tank/nd",
		Gate: ha.NewOpenGate(), Now: replTestClock(),
		Peer: &loopFakePeer{onInventory: func() error {
			i := atomic.AddInt32(&n, 1)
			pulls <- int(i)
			if i <= 2 {
				return fmt.Errorf("对端正在重启")
			}
			return nil
		}},
		PeerURL: "http://a:8080",
	}
	standby.Gate.Close("备机", "http://a:8080")
	go standby.RunStandby(ctx, 5*time.Millisecond)

	for want := 1; want <= 3; want++ {
		select {
		case got := <-pulls:
			if got != want {
				t.Fatalf("第 %d 轮，实际第 %d 轮", want, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 轮没等到——循环在失败后退出了，备机会静悄悄地停止追平", want)
		}
	}

	// 主机侧：同样不能因为一次打标失败就停下来
	marks := make(chan struct{}, 8)
	primary := &Replicator{
		ZFS: &failingMarkZFS{fail: 2, hit: marks}, Root: "tank/nd",
		Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	go primary.RunPrimary(ctx, 5*time.Millisecond)
	for i := 0; i < 3; i++ {
		select {
		case <-marks:
		case <-time.After(3 * time.Second):
			t.Fatalf("主机侧第 %d 轮没等到——打标循环在失败后退出了", i+1)
		}
	}
}

// context 取消后循环必须退出，否则旧角色的复制会和新角色争同一批数据集。
func TestReplicationLoopsStopOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r := &Replicator{
		ZFS: &fakeReplZFS{}, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	go func() { r.RunPrimary(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后循环没退出——切换时会和新角色抢同一批数据集")
	}
}

type loopFakePeer struct {
	fakeReplPeer
	onInventory func() error
}

func (f *loopFakePeer) Inventory(context.Context) (ha.ReplicationInventory, error) {
	if err := f.onInventory(); err != nil {
		return ha.ReplicationInventory{}, err
	}
	return ha.ReplicationInventory{Root: "tank/nd"}, nil
}

// failingMarkZFS 让前 fail 轮的打标失败，之后成功；每轮都往 hit 里放一个信号。
type failingMarkZFS struct {
	fakeReplZFS
	fail int32
	seen int32
	hit  chan struct{}
}

func (f *failingMarkZFS) ListGUIDs(_ context.Context, root string) (zfs.GUIDInventory, error) {
	i := atomic.AddInt32(&f.seen, 1)
	select {
	case f.hit <- struct{}{}:
	default:
	}
	if i <= f.fail {
		return zfs.GUIDInventory{}, fmt.Errorf("池正忙")
	}
	return zfs.GUIDInventory{Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}}, nil
}

// 排空条件是「至少追到」而非「等于」：主机每轮都打新标记，备机很容易越过目标，
// 用相等判断会一直等到超时。
func TestCaughtUpAcceptsAStandbyThatOvershotTheTarget(t *testing.T) {
	target := "rep-1787374375150111071"

	cases := []struct {
		name string
		got  string
		want bool
	}{
		{"正好追平", target, true},
		{"越过了目标（更新的标记）", "rep-1787374435150111071", true},
		{"还落后", "rep-1787374315150111071", false},
		// 位数不同时字典序与数值序相反，必须按数值比较。
		{"位数不同要按数值比", "rep-999999999999999999", false},
		{"没拉过", "", false},
		{"不是标记格式", "snapshot-x", false},
	}
	for _, c := range cases {
		if got := CaughtUpTo(c.got, target); got != c.want {
			t.Fatalf("%s：CaughtUpTo(%q, %q) = %v，要 %v", c.name, c.got, target, got, c.want)
		}
	}

	// 目标本身为空表示从没打过标记——无从排空，直接算完成
	if !CaughtUpTo("", "") {
		t.Fatal("没有目标时不该把切换卡住")
	}
}

// 本机在分区期间当过写入者（有对端没有的快照）时，复制不能停，也不能销毁本机那份：
// 应改名保留，并在状态里写明挪到了哪里。
func TestPullOncePreservesLocallyWrittenHistoryInsteadOfDestroyingIt(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-9", GUID: "local-only"}, // 分区期间本机自己打的
		{Name: "tank/nd/win11", GUID: "d1"},
	}}}}
	// 对端的谱系和本机没有任何交集
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "peer-1"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("分叉不该让复制停摆——停摆会让备机永远收不到数据库副本：%v", err)
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("本机写出来的历史被销毁了：%v", z.destroyed)
	}
	if len(z.renamed) != 1 || !strings.Contains(z.renamed[0], "-diverged-") {
		t.Fatalf("本机那份应当被改名保留：%v", z.renamed)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || !strings.Contains(rows[0].LastError, "分叉") {
		t.Fatalf("分叉必须留在状态里让人看见：%#v", rows)
	}
	if !strings.Contains(rows[0].LastError, "-diverged-") {
		t.Fatalf("状态里要写清挪到哪儿了，否则运维不知道去哪找：%#v", rows[0].LastError)
	}
}

// 反面：本机只是落后太多时照旧整体重建，否则一次正常的长时间离线都要人工介入。
func TestPullOnceStillRebuildsAStaleReplica(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "d1"}, // 只有数据集，没有本机独有的快照
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "peer-1"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("落后的副本应当照旧重建：%v", err)
	}
	if len(z.destroyed) != 1 {
		t.Fatalf("destroyed = %v", z.destroyed)
	}
}

// 重建不能先销毁再接收：两步之间被打断（被提成主机、对端中途宕机、断电）会丢掉整个目录，
// 写入者还会把残缺复制出去。必须挪开、收成功再删、失败则挪回。
func TestPullOnceKeepsTheCopyWhenARebuildIsInterrupted(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	// 有共同基准 rep-1，所以先走增量；增量收不下来才转全量重建。
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd/win11", GUID: "shared-1"},
			{Name: "tank/nd@rep-1", GUID: "shared-a"},
		}}},
		// 增量失败一次，转全量后再失败一次——对端在传输中途没了。
		recvErrs: []error{errFakeRepl, errFakeRepl},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "shared-1"},
		{Name: "tank/nd@rep-1", GUID: "shared-a"},
		{Name: "tank/nd@rep-2", GUID: "peer-b"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err == nil {
		t.Fatal("两次收流都失败了，这一轮必须报错，不能假装成功")
	}
	for _, d := range z.destroyed {
		if d == "tank/nd" {
			t.Fatalf("重建被打断后本机那份被销毁了，再也找不回来：destroyed=%v", z.destroyed)
		}
	}
	if _, ok := z.guids["tank/nd"]; !ok {
		t.Fatalf("收流失败后要把挪开的那份改回原名，否则本机目录凭空消失：renamed=%v", z.renamed)
	}
}

// 全量收流失败可能留下残片占住原名，导致挪开的完整副本改不回去、每轮多一个孤儿。
// 失败时要先清掉残片再改回完整副本，每轮结束本机都有一份能用的目录且不留残留。
func TestPullOnceRestoresTheCopyEvenWhenAPartialReceiveHoldsTheName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd/win11", GUID: "shared-1"},
			{Name: "tank/nd@rep-1", GUID: "shared-a"},
		}}},
		recvErrs:     []error{errFakeRepl, errFakeRepl},
		partialOnErr: true,
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "shared-1"},
		{Name: "tank/nd@rep-1", GUID: "shared-a"},
		{Name: "tank/nd@rep-2", GUID: "peer-b"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err == nil {
		t.Fatal("收流失败了，这一轮必须报错")
	}
	for name := range z.guids {
		if strings.Contains(name, "-rebuilding-") {
			t.Fatalf("失败的一轮不能留下挪开的副本，否则一轮攒一个：%v", z.guids)
		}
	}
	inv, ok := z.guids["tank/nd"]
	if !ok {
		t.Fatalf("失败后本机目录必须回到原名：renamed=%v destroyed=%v", z.renamed, z.destroyed)
	}
	for _, e := range inv.Entries {
		if strings.Contains(e.Name, "/partial") {
			t.Fatalf("回到原名的必须是那份完整的旧副本，不能是收到一半的残片：%v", inv.Entries)
		}
	}
}

// 有共同基准时，旧主崩溃前没送出的那一轮会被 recv -F 回滚（RPO 为一个复制轮次，属设计内）。
// 回滚不能无声：日志里要点名被回滚的轮次。
func TestPullOnceSaysWhatItRolledBack(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	var logged []string
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd/win11", GUID: "shared-1"},
			{Name: "tank/nd@rep-1", GUID: "shared-a"},
			// 本机崩溃前自己打的那一轮，对端从没有过
			{Name: "tank/nd@rep-9", GUID: "mine-9"},
		}}},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "shared-1"},
		{Name: "tank/nd@rep-1", GUID: "shared-a"},
		{Name: "tank/nd@rep-2", GUID: "peer-b"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080",
		Logger: slog.New(slog.NewTextHandler(&testWriter{lines: &logged}, nil))}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("有共同基准，增量该正常收下：%v", err)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "rep-9") {
		t.Fatalf("被回滚的那一轮要点名说出来，否则事后查无此事：\n%s", joined)
	}
}

type testWriter struct{ lines *[]string }

func (w *testWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}

// 空闲集群必须安静：目录没变就不打轮次。健康探针每 2 秒写入再回滚会弄脏 -wal，
// 判据不能看文件戳，否则每轮都要白做整库导出、递归快照和推流。
func TestMarkOnceStaysQuietWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "g1"},
	}}}}
	rev := uint64(7)
	rep := &Replicator{
		Store: st, ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil },
		DBRevision: func() uint64 { return rev },
	}

	if ok, err := rep.MarkOnce(ctx); err != nil || !ok {
		t.Fatalf("第一轮该打：进程刚起来，还不知道目录长什么样（ok=%v err=%v）", ok, err)
	}
	for i := 0; i < 5; i++ {
		ok, err := rep.MarkOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("第 %d 次：目录没变却打了一轮——空闲集群会一直空转", i+2)
		}
	}

	// 真有人改了目录，下一轮必须打，否则改动永远送不到备机。
	rev++
	if ok, err := rep.MarkOnce(ctx); err != nil || !ok {
		t.Fatalf("目录变了就必须打一轮（ok=%v err=%v）", ok, err)
	}
}

// 用不了的续传令牌不能把复制永久卡死：续传失败就 zfs recv -A 废掉半截状态，
// 回到正常的增量/全量路径。
func TestPullOnceAbandonsAResumeTokenItCannotUse(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		// 同上：要测「挪开重建」，夹具里就得真有东西值得挪。
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
			{Name: "tank/nd/win11", GUID: "d1"},
		}}},
		resumeTok: "1-stale-token",
		recvErrs:  []error{errFakeRepl}, // 续传那一次失败
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "a"},
		{Name: "tank/nd@rep-2", GUID: "b"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("续传用不了不该让这一轮失败——废掉它接着走增量即可：%v", err)
	}
	if !z.resumeAborted {
		t.Fatal("续不动的令牌必须废掉（zfs recv -A），否则下一轮还拿它重来，永远出不来")
	}
	if z.resumeTok != "" {
		t.Fatal("废掉之后本轮不该再被当成续传")
	}
	// 废掉之后要接着把这一轮收完
	if len(peer.streams) == 0 {
		t.Fatal("废掉续传之后要继续正常拉取，而不是空手而归")
	}
}

// recv -A 也失败（"cannot destroy 'tank/nd': dataset already exists"）时，挪开副本整体重建，
// 半截状态和令牌随旧名字一起移走。
func TestPullOnceRebuildsWhenTheResumeTokenCannotEvenBeDiscarded(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		// 有资产才有「值得挪开保全」这回事；空目录会就地重收，见
		// TestRebuildDoesNotPreserveAnEmptyCatalogue。
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
			{Name: "tank/nd/win11", GUID: "d1"},
		}}},
		resumeTok: "1-stale-token",
		abortErr:  errFakeRepl,
		recvErrs:  []error{errFakeRepl}, // 续传那一次失败
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "a"},
		{Name: "tank/nd@rep-2", GUID: "b"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("废弃不掉也要能自己爬出来：%v", err)
	}
	if len(z.renamed) == 0 {
		t.Fatalf("应当把这份用不了的副本挪开重建：renamed=%v destroyed=%v", z.renamed, z.destroyed)
	}
}

// 计划切换前要等每一台活着的备机都追到最新一轮：VRRP 按优先级选接任者，产品不知道是谁。
// 早已不再拉取的行（已死或当过主机）不算。
func TestLaggingPullersWaitsForEveryLiveStandby(t *testing.T) {
	now := time.Date(2026, 9, 2, 6, 14, 0, 0, time.UTC)
	latest := "rep-1788329600000000000"
	fresh := now.Add(-30 * time.Second)
	stale := now.Add(-20 * time.Minute)
	rows := []domain.ReplicationState{
		{Target: "http://10.0.0.4:8080", Kind: "primary-serve", LastSnapshot: latest, LastOKAt: &fresh},
		{Target: "http://10.0.0.3:8080", Kind: "primary-serve", LastSnapshot: "rep-1788329500000000000", LastOKAt: &fresh},
		{Target: "http://10.0.0.9:8080", Kind: "primary-serve", LastSnapshot: "rep-1", LastOKAt: &stale},
		{Target: "http://10.0.0.5:8080", Kind: "primary-serve", LastSnapshot: "rep-1", LastOKAt: &fresh}, // 本机自己的旧行
		{Target: "http://10.0.0.5:8080", Kind: "standby-pull", LastSnapshot: "rep-1", LastOKAt: &fresh},  // 不是拉取方
	}
	lagging := LaggingPullers(rows, latest, now, 3*time.Minute, "http://10.0.0.5:8080")
	if len(lagging) != 1 || lagging[0] != "http://10.0.0.3:8080" {
		t.Fatalf("lagging = %v，只有还在拉、且没追平的 .3 该被点名", lagging)
	}
	// .3 追上后：没人拖后腿
	rows[1].LastSnapshot = latest
	if got := LaggingPullers(rows, latest, now, 3*time.Minute, "http://10.0.0.5:8080"); len(got) != 0 {
		t.Fatalf("全部追平后 lagging = %v", got)
	}
	// 一台活着的备机都没有：不能当作「都追平了」——那是没人可交
	if got := LaggingPullers(nil, latest, now, 3*time.Minute, ""); len(got) != 1 || got[0] != "" {
		t.Fatalf("没有活着的拉取方时应返回一个空名占位表示不可交，得到 %v", got)
	}
}

// 备机落后超过 replicationMarkersKept 轮时，本机独有的旧标记是被对端裁掉的，不是本机写的；
// 当成分叉会在每次短暂离线后挪开并全量重建。早于对端最旧标记的独有标记只能是被裁的。
func TestPullOnceRebuildsALaggedReplicaWithoutSettingItAside(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000003", GUID: "old-3"}, // 对端早已修剪
		{Name: "tank/nd@rep-1788000000000000004", GUID: "old-4"},
		{Name: "tank/nd/win11", GUID: "d1"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000008", GUID: "p8"},
		{Name: "tank/nd@rep-1788000000000000009", GUID: "p9"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}
	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("落后的副本应当照旧重建：%v", err)
	}
	for _, r := range z.renamed {
		if strings.Contains(r, "-diverged-") {
			t.Fatalf("只是落后，不该把目录挪开保留：%v", z.renamed)
		}
	}
	if len(z.destroyed) != 1 {
		t.Fatalf("应整体重建（先清本地副本）：destroyed=%v", z.destroyed)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) == 1 && strings.Contains(rows[0].LastError, "分叉") {
		t.Fatalf("落后不是分叉，状态里不该吓人：%q", rows[0].LastError)
	}
}

func TestDivergentSnapshotsIgnoresMarkersThePeerPruned(t *testing.T) {
	local := zfs.GUIDInventory{Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000003", GUID: "a"},  // 老于对端最旧：被修剪
		{Name: "tank/nd@rep-1788000000000000012", GUID: "b"},  // 新于对端最旧却对端没有：本机自写
		{Name: "tank/nd/img@ndbackup-1", GUID: "c"},           // 非标记快照：一律算分叉
		{Name: "tank/nd@rep-1788000000000000009", GUID: "p9"}, // 两边都有
	}}
	peer := []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000008", GUID: "p8"},
		{Name: "tank/nd@rep-1788000000000000009", GUID: "p9"},
	}
	got := divergentSnapshots(local, peer)
	want := []string{"tank/nd/img@ndbackup-1", "tank/nd@rep-1788000000000000012"} // 字典序
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("divergent = %v, want %v", got, want)
	}
	// 对端一个标记都没有：分不清，按保守口径全算分叉。
	if got := divergentSnapshots(local, nil); len(got) != 4 {
		t.Fatalf("对端无标记时应全算分叉，得到 %v", got)
	}
}

// 挪开的目录的子数据集仍带着指向 <池>/nd/... 的显式挂载点，会遮住新收的 nd/db，
// 让数据库副本写进孤儿、复制冻结。挪走后必须摘掉挂载点。
func TestMovedAsideCopyReleasesItsMountpoints(t *testing.T) {
	ctx := context.Background()

	// 重建路径
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "d1"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "p1"}, {Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}
	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(z.detached) == 0 {
		t.Fatal("重建把 tank/nd 挪开之前没有摘掉它的挂载点，孤儿会盖住新收到的 nd/db")
	}
	for _, d := range z.detached {
		if !strings.Contains(d, "-rebuilding-") && !strings.Contains(d, "-diverged-") {
			t.Fatalf("摘挂载点的对象应当是被挪开的那一份，得到 %q", d)
		}
	}

	// 分叉保留路径
	st2 := newImageTestStore(t)
	gate2 := ha.NewOpenGate()
	gate2.Close("备机", "http://a:8080")
	z2 := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000012", GUID: "local-only"},
		{Name: "tank/nd/win11", GUID: "d1"},
	}}}}
	peer2 := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1788000000000000008", GUID: "p8"}, {Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep2 := &Replicator{Store: st2, ZFS: z2, Root: "tank/nd", Gate: gate2, Now: replTestClock(), Peer: peer2, PeerURL: "http://a:8080"}
	if err := rep2.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(z2.detached) == 0 {
		t.Fatal("分叉保留把 tank/nd 挪开之前没有摘掉它的挂载点")
	}
}

// 裁旧标记失败（备机正用 zfs send 读它，报 dataset is busy）不能让这一轮标记失败，
// 否则计划切换的排空会被拒绝。
func TestMarkOnceSurvivesAPruneThatCannotRunYet(t *testing.T) {
	ctx := context.Background()
	z := &fakeReplZFS{
		dbCopyDir: t.TempDir(),
		guids: map[string]zfs.GUIDInventory{
			"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
		},
		pruneErr: errors.New("cannot destroy snapshot tank/nd@rep-1: dataset is busy"),
	}
	rep := &Replicator{
		ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	marked, err := rep.MarkOnce(ctx)
	if err != nil {
		t.Fatalf("剪枝删不掉不该让这一轮失败：%v", err)
	}
	if !marked {
		t.Fatal("这一轮应当算作已标记——快照已经打上去了")
	}
	if len(z.snapshots) != 1 {
		t.Fatalf("标记快照没打上：%v", z.snapshots)
	}
}

// 目录本来为空时，重建不该再挪一份空壳到一边：反复重建的正是丢了目录的节点，
// 每轮一个空壳会把池写满，池满又让收流继续失败。
func TestRebuildDoesNotPreserveAnEmptyCatalogue(t *testing.T) {
	ctx := context.Background()
	z := &fakeReplZFS{
		dbCopyDir: t.TempDir(),
		guids: map[string]zfs.GUIDInventory{
			// 只有容器和保留子集，没有任何镜像或配置——空目录的样子
			"tank/nd": {Entries: []zfs.GUIDEntry{
				{Name: "tank/nd", GUID: "1"},
				{Name: "tank/nd/db", GUID: "2"},
			}},
		},
	}
	rep := &Replicator{ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		Peer: &fakeReplPeer{}, PeerURL: "http://a:8080"}
	if err := rep.rebuildWhole(ctx, "rep-9"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	for _, name := range z.renamed {
		if strings.Contains(name, "-rebuilding-") {
			t.Fatalf("空目录被挪到一边了：%v", z.renamed)
		}
	}
	if len(z.received) == 0 {
		t.Fatal("没有收流：让路的目的是把新的收进来")
	}
}

// 重建被重启打断时回收来不及执行，只能靠之后的轮次清掉；之后那轮失败也得回收，只留最新一份供人工恢复。
func TestFailedRebuildStillPrunesEarlierAsideCopies(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeReplZFS)
	}{
		{"改名挪开就失败", func(z *fakeReplZFS) {
			z.renameErr = errors.New("cannot unmount '/ndiskless/tank/nd': pool or dataset is busy")
		}},
		{"接收失败后改回原名", func(z *fakeReplZFS) { z.recvErrs = []error{errors.New("out of space")} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newImageTestStore(t)
			gate := ha.NewOpenGate()
			gate.Close("备机", "http://a:8080")
			// 本机没有共同快照、也没有自己写的东西：这一轮整体重建
			z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
				{Name: "tank/nd/win11", GUID: "d1"},
			}}}}
			z.siblings = []string{"tank/nd-rebuilding-100", "tank/nd-rebuilding-200", "tank/nd-diverged-250", "tank/nd-rebuilding-300"}
			tc.set(z)
			peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
				{Name: "tank/nd@rep-5", GUID: "peer-1"},
				{Name: "tank/nd/win11", GUID: "d2"},
			}}}
			rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}
			if err := rep.PullOnce(context.Background()); err == nil {
				t.Fatal("这一轮应当失败")
			}
			for _, old := range []string{"tank/nd-rebuilding-100", "tank/nd-rebuilding-200"} {
				if !contains(z.destroyed, old) {
					t.Fatalf("较早的重建残留 %s 没有回收：destroyed=%v", old, z.destroyed)
				}
			}
			if contains(z.destroyed, "tank/nd-rebuilding-300") || contains(z.destroyed, "tank/nd-diverged-250") {
				t.Fatalf("失败时最新一份和保住的副本不该删：destroyed=%v", z.destroyed)
			}
		})
	}
}

// 被打断的 -R 接收会把冲突数据集留在 recv-<pid>-<seq> 临时名下，挂在别的挂载点上让增量和重建改名一直报 busy。
// 先删掉它再照常复制，不必整体重建；写入者那边真有同名数据集的不能动。
func TestPullDropsStaleRecvTemp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		peerHasIt bool
	}{{"残留", false}, {"写入者也有同名数据集", true}} {
		t.Run(tc.name, func(t *testing.T) {
			st := newImageTestStore(t)
			gate := ha.NewOpenGate()
			gate.Close("备机", "http://a:8080")
			local := []zfs.GUIDEntry{
				{Name: "tank/nd", GUID: "r1"}, {Name: "tank/nd@rep-5", GUID: "m5"},
				{Name: "tank/nd/win11", GUID: "d1"}, {Name: "tank/nd/win11@rep-5", GUID: "w5"},
				{Name: "tank/nd/recv-1465346-1", GUID: "t1"},
			}
			peerEntries := []zfs.GUIDEntry{
				{Name: "tank/nd", GUID: "r1"}, {Name: "tank/nd@rep-5", GUID: "m5"},
				{Name: "tank/nd/win11", GUID: "d1"}, {Name: "tank/nd/win11@rep-5", GUID: "w5"},
			}
			if tc.peerHasIt {
				peerEntries = append(peerEntries, zfs.GUIDEntry{Name: "tank/nd/recv-1465346-1", GUID: "t1"})
			}
			z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: local}}}
			peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: peerEntries}}
			rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}
			if err := rep.PullOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			dropped := contains(z.destroyed, "tank/nd/recv-1465346-1")
			if tc.peerHasIt {
				if dropped || len(z.renamed) > 0 {
					t.Fatalf("写入者有的数据集不能当残留删：destroyed=%v renamed=%v", z.destroyed, z.renamed)
				}
				return
			}
			if !dropped || !contains(z.detached, "tank/nd/recv-1465346-1") {
				t.Fatalf("残留要先卸载再删除：detached=%v destroyed=%v", z.detached, z.destroyed)
			}
			if len(z.renamed) > 0 {
				t.Fatalf("删掉残留后照常复制即可，不该整体重建：renamed=%v", z.renamed)
			}
		})
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// 写入者的目录与它自己的数据库对不上（库里有、池里没有）时不许跟随：recv -F 会把缺的数据集从本机也删掉。
// 判据是写入者是否自洽，而不是「会不会删本机的东西」，否则合法删除也会被拦。
func TestPullRefusesAWriterWhoseCatalogueContradictsItsOwnDatabase(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "g1"},
		{Name: "tank/nd/win11", GUID: "d1"},
		{Name: "tank/nd/win11_default", GUID: "d2"}, // 本机还有这个配置
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{
		Root: "tank/nd",
		Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "g1"},
			{Name: "tank/nd@rep-2", GUID: "g2"},
			{Name: "tank/nd/win11", GUID: "d1"}, // 对端少了 win11_default
		},
		// 而且对端自己都说它对不上：库里还记着这个配置，池里却没有。
		Incomplete: []string{"win11_default"},
	}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	err := rep.PullOnce(ctx)
	if err == nil {
		t.Fatal("跟随了一个自己都对不上的写入者——它的残缺会被复制到全集群")
	}
	if !strings.Contains(err.Error(), "win11_default") {
		t.Fatalf("拒绝理由要指名缺了什么，否则现场无从下手：%v", err)
	}
	if !strings.Contains(err.Error(), "镜像管理") || strings.Contains(err.Error(), "系统设置") {
		t.Fatalf("要指向界面上真有的入口：%v", err)
	}
	if len(z.received) != 0 {
		t.Fatalf("不该收下这个流：%v", z.received)
	}
}

// 反面：对端自洽时照常跟随，哪怕它比本机少东西——那是运维删掉的，删除必须能传播。
func TestPullFollowsAConsistentWriterEvenWhenItHoldsLess(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "g1"},
		{Name: "tank/nd/win11", GUID: "d1"},
		{Name: "tank/nd/win11_default", GUID: "d2"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{
		Root: "tank/nd",
		Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "g1"},
			{Name: "tank/nd@rep-2", GUID: "g2"},
			{Name: "tank/nd/win11", GUID: "d1"},
		},
		Incomplete: nil, // 自洽：配置连同数据库行一起删掉了
	}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("对端自洽就该跟随，删除也是要传播的：%v", err)
	}
	if len(z.received) == 0 {
		t.Fatal("没有收流：合法的删除必须传播到备机")
	}
}

// 增量 recv -F 会回滚本机多出的快照：多的是轮次标记无妨，是还原点就会丢数据，必须挪开保留。
func TestPullOncePreservesRestorePointsAnIncrementalWouldRollBack(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common", Created: 1000},             // 与对端共有：增量的基准
		{Name: "tank/nd@rep-9", GUID: "local-marker", Created: 1200},       // 本机独有的轮次标记
		{Name: "tank/nd/vmdk_default@2", GUID: "local-red", Created: 1100}, // 基准之后本机写的 ← 不能被吞掉
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common"},
		{Name: "tank/nd@rep-11", GUID: "peer-newer"},
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("复制不该停摆：%v", err)
	}
	if len(z.renamed) != 1 || !strings.Contains(z.renamed[0], "-diverged-") {
		t.Fatalf("本机这份应当被挪到一边保住，而不是被回滚：renamed=%v received=%v", z.renamed, z.received)
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || !strings.Contains(rows[0].LastError, "还原点") || !strings.Contains(rows[0].LastError, "-diverged-") {
		t.Fatalf("状态里要说清丢的是还原点、挪到了哪：%#v", rows)
	}
}

// 反面：只多了轮次标记就照旧回滚，否则每次正常多跑一轮都要人工介入。
func TestPullOnceStillRollsBackMarkerOnlyRounds(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common"},
		{Name: "tank/nd@rep-9", GUID: "local-marker"}, // 只有标记
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common"},
		{Name: "tank/nd@rep-11", GUID: "peer-newer"},
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range z.renamed {
		if strings.Contains(r, "-diverged-") {
			t.Fatalf("只多了轮次标记，不该把目录挪开：%v", z.renamed)
		}
	}
	if len(z.received) == 0 {
		t.Fatalf("应当照旧收增量：received=%v", z.received)
	}
}

// 保住的那份不能被自动清理掉。清理是给重建过程的残留（-rebuilding-）准备的；
// -diverged- 里装着别处没有的内容，只有人能决定什么时候删。
func TestPruneAsideCopiesNeverReclaimsAPreservedCopy(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	// 本机只是落后（没有自己写的东西），这一轮会整体重建——清理就发生在这条路上
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd/win11", GUID: "d1"},
	}}}}
	z.siblings = []string{
		"tank/nd-diverged-100",   // 有内容，最老 —— 绝不能删
		"tank/nd-rebuilding-200", // 过程残留
		"tank/nd-rebuilding-300",
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "peer-1"},
		{Name: "tank/nd/win11", GUID: "d2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range z.destroyed {
		if strings.Contains(d, "-diverged-") {
			t.Fatalf("保住的那份被当成垃圾清掉了：%v", z.destroyed)
		}
	}
	if !contains(z.destroyed, "tank/nd-rebuilding-200") {
		t.Fatalf("重建残留还是该回收的：%v", z.destroyed)
	}
}

// 写入者删掉还原点是正常操作，备机上那份不算分叉，否则每删一次每台备机都要整体重建。
// 按 creation 判断，早于共同基准的是对端删掉的；不能用 createtxg，接收端按落盘顺序分配，根标记先于子还原点。
func TestPullOnceRollsBackRestorePointsThePeerDeleted(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common", Created: 1000},
		// 创建在基准之前，尽管它在本机的 createtxg 反而比基准大（收流顺序如此）
		{Name: "tank/nd/vmdk_default@old", GUID: "deleted-by-peer", Created: 500},
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-5", GUID: "common"},
		{Name: "tank/nd@rep-11", GUID: "peer-newer"},
		{Name: "tank/nd/vmdk_default", GUID: "d1"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(), Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range z.renamed {
		if strings.Contains(r, "-diverged-") {
			t.Fatalf("对端删还原点是正常操作，不该被当成分叉：%v", z.renamed)
		}
	}
	if len(z.received) == 0 {
		t.Fatalf("应当照旧收增量：%v", z.received)
	}
}

// 裁旧标记时要保住每台备机最后确认的那个，否则备机重启几分钟回来就没有共同快照、只能整体重建；
// 超过一天没露面的备机不再保。
func TestMarkOnceSparesWhatEachStandbyLastConfirmed(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for _, row := range []domain.ReplicationState{
		{Target: "http://10.0.0.4:8080", Kind: "primary-serve", LastSnapshot: "rep-100", LastOKAt: at(5 * time.Minute), UpdatedAt: now},
		{Target: "http://10.0.0.5:8080", Kind: "primary-serve", LastSnapshot: "rep-50", LastOKAt: at(30 * time.Hour), UpdatedAt: now},
		{Target: "http://10.0.0.6:8080", Kind: "primary-serve", LastSnapshot: "", LastOKAt: at(time.Minute), UpdatedAt: now},
		{Target: "http://10.0.0.3:8080", Kind: "standby-pull", LastSnapshot: "rep-77", LastOKAt: at(time.Minute), UpdatedAt: now},
	} {
		if err := st.ReplicationStates().Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
	}}
	rep := &Replicator{
		Store: st, ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: func() time.Time { return now },
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	if marked, err := rep.MarkOnce(ctx); err != nil || !marked {
		t.Fatalf("mark = %v, %v", marked, err)
	}
	if len(z.prunes) != 1 || z.prunes[0] != "tank/nd|rep-|3|rep-100" {
		t.Fatalf("prunes = %v，应只保住 5 分钟前露过面的那台备机的 rep-100", z.prunes)
	}
}

// 排空要自己打一轮并拿它当目标，退位前按住后台不再打；否则后台插进来的新一轮在退位后无人收到。
func TestMarkForSwitchStampsItsOwnRoundAndHoldsTheLoop(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 1, 31, 0, 0, time.UTC)
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
	}}
	rev := uint64(1)
	rep := &Replicator{
		ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: func() time.Time { return now },
		DBRevision: func() uint64 { return rev },
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	if marked, err := rep.MarkOnce(ctx); err != nil || !marked {
		t.Fatalf("first mark = %v, %v", marked, err)
	}

	// 目录没变也要打：排空等的必须是它自己打的这一轮
	name, err := rep.MarkForSwitch(ctx, 5*time.Minute)
	if err != nil || name == "" || len(z.snapshots) != 2 || !strings.HasSuffix(z.snapshots[1], "@"+name) {
		t.Fatalf("switch mark = %q err = %v snapshots = %v", name, err, z.snapshots)
	}

	// 排空期间数据库还在变（心跳、审计），后台不许再打新的一轮
	rev++
	now = now.Add(time.Minute)
	if marked, _ := rep.MarkOnce(ctx); marked || len(z.snapshots) != 2 {
		t.Fatalf("排空期间后台又打了一轮: %v", z.snapshots)
	}
	// 按住是有时限的：退位没发生（排空失败、重启出错），后台自己恢复
	now = now.Add(5 * time.Minute)
	if marked, _ := rep.MarkOnce(ctx); !marked {
		t.Fatal("时限过后后台应恢复打标记")
	}
	rev++
	rep.MarkForSwitch(ctx, 5*time.Minute)
	rep.ResumeMarking()
	rev++
	if marked, _ := rep.MarkOnce(ctx); !marked {
		t.Fatal("ResumeMarking 之后应立即恢复")
	}
}

// 超管保存后递归增量被拒时，逐个数据集追到同一轮：被顶替的配置只收新增还原点，根最后收，目录不挪不重建。
func TestPullCatchesUpDatasetByDatasetWhenTheRecursiveRoundIsRefused(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "2=c2new", "rep-2=c2r"},
	)
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
			[]string{"", "rep-1=r1"},
			[]string{"/img", "0=i0", "rep-1=i1"},
			[]string{"/cfg</img@0", "1=c1", "rep-1=c1r"},
		)},
		recvErrs: []error{errors.New("cannot receive new filesystem stream: destination has snapshots (eg. tank/nd/cfg@1)")},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range z.renamed {
		if strings.HasPrefix(r, "tank/nd->") {
			t.Fatalf("不该整份重建目录: %v", z.renamed)
		}
	}
	want := `[rep-1->rep-2 dataset "/img" from "rep-1" origin "" to "rep-2" dataset "/cfg" from "1" origin "" to "rep-2" dataset "" from "rep-1" origin "" to "rep-2"]`
	if got := fmt.Sprint(peer.streams); got != want {
		t.Fatalf("streams =\n%s\nwant\n%s", got, want)
	}
	if fmt.Sprint(z.received[1:]) != "[tank/nd/img <- DATASET:/img tank/nd/cfg <- DATASET:/cfg tank/nd <- DATASET:]" {
		t.Fatalf("received = %v", z.received)
	}
}

// 追平要跑很久时，写入者可能中途清掉目标那一轮的标记，流读到一半就断。下一轮换个目标重试即可；
// 升级成整体重建会把已经追平的几十 G 全部丢掉从头收。
func TestCatchUpInterruptedByTheStreamRetriesInsteadOfRebuilding(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
	)
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
			[]string{"", "rep-1=r1"},
			[]string{"/img", "0=i0", "rep-1=i1"},
		)},
		recvErrs:       []error{errors.New("cannot receive incremental stream: most recent snapshot of tank/nd/img does not match incremental source")},
		datasetRecvErr: storage.CommandError{Name: "zfs", Args: []string{"recv"}, Output: "cannot receive: failed to read from stream\n", Err: errFakeRepl},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err == nil {
		t.Fatal("这一轮没追完，应报错等下一轮")
	}
	for _, r := range z.renamed {
		if strings.HasPrefix(r, "tank/nd->") {
			t.Fatalf("流中断不该升级成整体重建: %v", z.renamed)
		}
	}
}

// 超管保存后递归增量失败时，流里靠前的根和镜像已收下 rep-2，被顶替的配置被 -F 回滚停在旧内容。
// 追平只补配置；根没有可发的一步，所以要另行向写入者确认这一轮，否则排空会一直等。
func TestCatchUpAfterARealFailedRoundConfirmsTheRoundToTheWriter(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "2=c2new", "rep-2=c2r"},
	)
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
			[]string{"", "rep-1=r1"},
			[]string{"/img", "0=i0", "rep-1=i1"},
			[]string{"/cfg</img@0", "1=c1", "rep-1=c1r"},
		)},
		recvErrs: []error{errors.New("cannot receive new filesystem stream: destination has snapshots (eg. tank/nd/cfg@1)")},
	}
	z.onRecvErr = func() {
		z.guids["tank/nd"] = inv("tank/nd",
			[]string{"", "rep-1=r1", "rep-2=r2"},
			[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
			[]string{"/cfg</img@0", "1=c1"},
		)
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	want := `[rep-1->rep-2 dataset "/cfg" from "1" origin "" to "rep-2"]`
	if got := fmt.Sprint(peer.streams); got != want {
		t.Fatalf("streams =\n%s\nwant\n%s", got, want)
	}
	if fmt.Sprint(peer.confirmed) != "[rep-2]" {
		t.Fatalf("追赶完成后应向写入者确认 rep-2，得到 %v", peer.confirmed)
	}
}

// 上次追平半路中断：根已有最新标记、数据集都在，但配置停在旧内容。
// 只比「标记一样、数据集都在」会误判已追平，写入者没有新改动时旧内容会一直留着。
func TestPullCatchesUpAConfigLeftBehindUnderTheNewestMarker(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "2=c2new", "rep-2=c2r"},
	)
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1"},
	)}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	want := `[dataset "/cfg" from "1" origin "" to "rep-2"]`
	if got := fmt.Sprint(peer.streams); got != want {
		t.Fatalf("streams =\n%s\nwant\n%s", got, want)
	}
	for _, r := range z.renamed {
		if strings.HasPrefix(r, "tank/nd->") {
			t.Fatalf("只差一个配置，不该整份重建: %v", z.renamed)
		}
	}
	if fmt.Sprint(peer.confirmed) != "[rep-2]" {
		t.Fatalf("补齐后应向写入者确认 rep-2，得到 %v", peer.confirmed)
	}
}

// 递归复制中断时令牌记在正在接收的子数据集上而非根上；只看根会重发增量冲掉半截进度。
func TestPullOnceResumesAnInterruptedChildDataset(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
		}}},
		resumeTok: "1-child-token",
		resumeDS:  "tank/nd/windows-vmdk",
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "a"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if z.resumeAborted {
		t.Fatal("能续的半截被废掉了，又得从头传")
	}
	if len(peer.streams) == 0 || peer.streams[0] != "resume:1-child-token" {
		t.Fatalf("应先用子数据集上的令牌续传，streams = %v", peer.streams)
	}
	if len(z.recvTargets) == 0 || z.recvTargets[0] != "tank/nd/windows-vmdk" {
		t.Fatalf("续传流要收进带令牌的那个数据集，recv = %v", z.recvTargets)
	}
}

// 续不动的子数据集令牌，也要在那个数据集上废掉（recv -A 根废不掉子的半截）。
func TestPullOnceAbandonsAChildResumeTokenOnThatDataset(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
		}}},
		resumeTok: "1-stale-token",
		resumeDS:  "tank/nd/windows-vmdk",
		recvErrs:  []error{errFakeRepl},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "a"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if z.abortedDS != "tank/nd/windows-vmdk" {
		t.Fatalf("要在带令牌的数据集上废掉半截，aborted = %q", z.abortedDS)
	}
}

// 标记相同但配置落后、本机又有写入者没有的还原点：追平计划会拒绝，
// 此时必须挪成 -diverged- 副本保留并告警，不能走重建把它静默删掉。
func TestPullPreservesLocalRestorePointsWhenCatchUpUnderTheNewestMarkerDiverges(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "2=c2", "rep-2=c2r"},
	)
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "3=c3local"}, // @3 是 rep-2 之后本机自己打的
	)}}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("复制不该停摆：%v", err)
	}
	checkDivergedPreserved(t, ctx, z, st)
}

// 增量收不进、重新读到的本机清单里有写入者没有的还原点：同样要保留成 -diverged- 副本并告警。
func TestPullPreservesLocalRestorePointsWhenCatchUpAfterARefusedIncrementalDiverges(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "rep-1=c1r", "rep-2=c2r"},
	)
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": inv("tank/nd",
			[]string{"", "rep-1=r1"},
			[]string{"/img", "0=i0", "rep-1=i1"},
			[]string{"/cfg</img@0", "1=c1", "rep-1=c1r"},
		)},
		recvErrs: []error{errors.New("cannot receive incremental stream: destination tank/nd/cfg has been modified")},
	}
	z.onRecvErr = func() {
		z.guids["tank/nd"] = inv("tank/nd",
			[]string{"", "rep-1=r1"},
			[]string{"/img", "0=i0", "rep-1=i1"},
			[]string{"/cfg</img@0", "1=c1", "rep-1=c1r", "3=c3local"},
		)
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: sender.Entries}}
	rep := &Replicator{Store: st, ZFS: z, Peer: peer, Gate: gate, Root: "tank/nd", PeerURL: "http://a:8080", Now: replTestClock()}

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatalf("复制不该停摆：%v", err)
	}
	checkDivergedPreserved(t, ctx, z, st)
}

func checkDivergedPreserved(t *testing.T, ctx context.Context, z *fakeReplZFS, st store.Store) {
	t.Helper()
	preserved := ""
	for _, r := range z.renamed {
		if strings.HasPrefix(r, "tank/nd->") && strings.Contains(r, "-diverged-") {
			preserved = strings.TrimPrefix(r, "tank/nd->")
		}
	}
	if preserved == "" {
		t.Fatalf("本机那份要挪成 -diverged- 副本保留：renamed=%v destroyed=%v", z.renamed, z.destroyed)
	}
	for _, d := range z.destroyed {
		if strings.HasPrefix(d, "tank/nd-") {
			t.Fatalf("挪开的副本被删了，本机写的还原点就此丢失：destroyed=%v", z.destroyed)
		}
	}
	rows, _ := st.ReplicationStates().List(ctx)
	if len(rows) != 1 || !strings.Contains(rows[0].LastError, preserved) {
		t.Fatalf("状态里要告警并点名保留在哪：%#v", rows)
	}
}

// 续传令牌废不掉而要整体重建时，也不能跟随自己都对不上的写入者。
func TestPullRefusesAnInconsistentWriterEvenWhenTheResumeTokenForcesARebuild(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
			{Name: "tank/nd/win11", GUID: "d1"},
			{Name: "tank/nd/win11_default", GUID: "d2"},
		}}},
		resumeTok: "1-stale-token",
		abortErr:  errFakeRepl,
		recvErrs:  []error{errFakeRepl},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "a"},
		{Name: "tank/nd@rep-2", GUID: "b"},
		{Name: "tank/nd/win11", GUID: "d1"},
	}, Incomplete: []string{"win11_default"}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	err := rep.PullOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "win11_default") {
		t.Fatalf("要拒绝并点名对不上的数据集：%v", err)
	}
	if len(z.renamed) != 0 || len(z.destroyed) != 0 {
		t.Fatalf("拒绝时不该动本机副本：renamed=%v destroyed=%v", z.renamed, z.destroyed)
	}
}

// 续传令牌废不掉而要整体重建时，也不能用空目录覆盖本机数据。
func TestPullRefusesAnEmptyWriterEvenWhenTheResumeTokenForcesARebuild(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://a:8080")
	z := &fakeReplZFS{
		guids: map[string]zfs.GUIDInventory{"tank/nd": {Entries: []zfs.GUIDEntry{
			{Name: "tank/nd@rep-1", GUID: "a"},
			{Name: "tank/nd/win11", GUID: "d1"},
		}}},
		resumeTok: "1-stale-token",
		abortErr:  errFakeRepl,
		recvErrs:  []error{errFakeRepl},
	}
	peer := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: []zfs.GUIDEntry{
		{Name: "tank/nd", GUID: "p0"},
		{Name: "tank/nd@rep-9", GUID: "p1"},
		{Name: "tank/nd/db", GUID: "p2"},
	}}}
	rep := &Replicator{Store: st, ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		Peer: peer, PeerURL: "http://a:8080"}

	err := rep.PullOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("要拒绝用空目录覆盖本机：%v", err)
	}
	if len(z.renamed) != 0 || len(z.destroyed) != 0 {
		t.Fatalf("拒绝时不该动本机副本：renamed=%v destroyed=%v", z.renamed, z.destroyed)
	}
}
