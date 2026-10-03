package ops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

func standbyReplicator(t *testing.T, z *fakeReplZFS, vip ReplicationPeer) *Replicator {
	t.Helper()
	gate := ha.NewOpenGate()
	gate.Close("备机", "http://192.168.10.250:8080")
	return &Replicator{Store: newImageTestStore(t), ZFS: z, Peer: vip, Gate: gate,
		Root: "tank/nd", PeerURL: "http://192.168.10.250:8080", Now: replTestClock()}
}

// 接任前的追平走对端自己的地址：持有 VIP 的备机向 VIP 拉，拉到的是自己。
func TestPullFromReadsTheNamedNodeNotTheVIP(t *testing.T) {
	ctx := context.Background()
	mine := inv("tank/nd", []string{"", "rep-1=r1"}, []string{"/img", "0=i0", "rep-1=i1"})
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": mine}}
	vip := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: mine.Entries}} // 虚 IP 在本机：问到的是自己
	writer := inv("data/nd", []string{"", "rep-1=r1", "rep-2=r2"}, []string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"})
	direct := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "data/nd", Entries: writer.Entries}}
	rep := standbyReplicator(t, z, vip)

	if err := rep.PullFrom(ctx, direct, "http://192.168.10.3:8080"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(direct.streams) != "[rep-1->rep-2]" || len(vip.streams) != 0 {
		t.Fatalf("应从指名的节点拉: direct=%v vip=%v", direct.streams, vip.streams)
	}
	if rep.Peer != ReplicationPeer(vip) || rep.PeerURL != "http://192.168.10.250:8080" {
		t.Fatal("拉完之后周期拉取仍应指向虚 IP")
	}
}

// blockingPeer 停在读清单那一步，直到放行。
type blockingPeer struct {
	*fakeReplPeer
	entered chan struct{}
	release chan struct{}
}

func (p *blockingPeer) Inventory(ctx context.Context) (ha.ReplicationInventory, error) {
	close(p.entered)
	<-p.release
	return p.fakeReplPeer.Inventory(ctx)
}

// 两路拉取收进的是同一棵目录，不能同时进行：追平期间周期拉取要等。
func TestPullFromAndThePeriodicPullDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	mine := inv("tank/nd", []string{"", "rep-1=r1"}, []string{"/img", "0=i0", "rep-1=i1"})
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": mine}}
	vip := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: mine.Entries}}
	direct := &blockingPeer{fakeReplPeer: &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: mine.Entries}},
		entered: make(chan struct{}), release: make(chan struct{})}
	rep := standbyReplicator(t, z, vip)

	pulled := make(chan error, 1)
	go func() { pulled <- rep.PullFrom(ctx, direct, "http://192.168.10.3:8080") }()
	<-direct.entered
	periodic := make(chan error, 1)
	go func() { periodic <- rep.PullOnce(ctx) }()
	select {
	case <-periodic:
		t.Fatal("追平还没结束，周期拉取就进去了")
	case <-time.After(100 * time.Millisecond):
	}
	close(direct.release)
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	if err := <-periodic; err != nil {
		t.Fatal(err)
	}
}

// 交接时主机先关写入再打最后一轮，闸门关着也要打得出；周期打标记则照旧在闸门关着时不打。
func TestFinalRoundIsStampedWithChangesClosed(t *testing.T) {
	ctx := context.Background()
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
	}}
	gate := ha.NewOpenGate()
	rep := &Replicator{ZFS: z, Root: "tank/nd", Gate: gate, Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil }}
	gate.Close("正在把主机身份交给节点 node-b", "")

	if marked, err := rep.MarkOnce(ctx); err != nil || marked {
		t.Fatalf("周期打标记在闸门关着时不该打: %v %v", marked, err)
	}
	marker, err := rep.MarkForSwitch(ctx, time.Minute)
	if err != nil || marker == "" || len(z.snapshots) != 1 {
		t.Fatalf("最后一轮应打得出来: marker=%q err=%v snapshots=%v", marker, err, z.snapshots)
	}
}

// 持有 VIP 的备机没有可跟随的对象，不拉：向 VIP 拉连到的是自己，连接失效后
// 拉取循环会卡死。
func TestStandbyHoldingTheVIPDoesNotPullFromItself(t *testing.T) {
	ctx := context.Background()
	mine := inv("tank/nd", []string{"", "rep-1=r1"})
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": mine}}
	vip := &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: mine.Entries}}
	rep := standbyReplicator(t, z, vip)
	asked := 0
	rep.HoldsVIP = func() bool { asked++; return true }

	if err := rep.PullOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if asked == 0 || len(vip.streams) != 0 {
		t.Fatalf("持有虚 IP 时不该向虚 IP 拉: asked=%d streams=%v", asked, vip.streams)
	}
}

// hangingPeer 读清单时一直不返回，直到请求被取消，模拟失效的连接。
type hangingPeer struct{ *fakeReplPeer }

func (p hangingPeer) Inventory(ctx context.Context) (ha.ReplicationInventory, error) {
	<-ctx.Done()
	return ha.ReplicationInventory{}, ctx.Err()
}

// 对端不回应时，一轮拉取要在限定时间内结束并报错，下一轮才有机会重来。
func TestPullGivesUpOnAPeerThatDoesNotAnswer(t *testing.T) {
	mine := inv("tank/nd", []string{"", "rep-1=r1"})
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": mine}}
	rep := standbyReplicator(t, z, hangingPeer{&fakeReplPeer{}})
	rep.InventoryTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- rep.PullOnce(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("对端不回应应报错")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("对端不回应时拉取卡住了")
	}
}

// 接任前的追平要等周期拉取让出目录；周期拉取卡住时，等待须在期限内放弃，否则接任跟着卡死。
func TestPullFromGivesUpWaitingForAStuckPull(t *testing.T) {
	mine := inv("tank/nd", []string{"", "rep-1=r1"})
	z := &fakeReplZFS{guids: map[string]zfs.GUIDInventory{"tank/nd": mine}}
	stuck := &blockingPeer{fakeReplPeer: &fakeReplPeer{inv: ha.ReplicationInventory{Root: "tank/nd", Entries: mine.Entries}},
		entered: make(chan struct{}), release: make(chan struct{})}
	defer close(stuck.release)
	rep := standbyReplicator(t, z, stuck)
	go func() { _ = rep.PullOnce(context.Background()) }()
	<-stuck.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rep.PullFrom(ctx, &fakeReplPeer{}, "http://192.168.10.5:8080") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("没等到目录应报错")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等周期拉取让出目录时卡住了")
	}
}
