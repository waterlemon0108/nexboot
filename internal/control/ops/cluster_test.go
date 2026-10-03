package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
)

// Register 只做 upsert，新节点加入时不能把主机自己的行改键（那是 ensureServer
// 处理本机换 ID 的路径）。
func TestClusterRegistryRegisterAndPeers(t *testing.T) {
	st := newImageTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)

	// 主机自己的行先存在（启动时 EnsureLocalServer）。
	self := domain.Server{ID: "node-a", Name: "主机A", IP: "10.0.0.1", PortalIP: "10.0.0.250", APIURL: "http://10.0.0.1:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := EnsureLocalServer(ctx, st, self); err != nil {
		t.Fatal(err)
	}

	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }}

	// 新节点加入。
	if err := reg.Register(ctx, ha.NodeInfo{NodeID: "node-b", APIURL: "http://10.0.0.2:8080", PortalIP: "10.0.0.2", Role: "standby", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	// 本机行不变。
	if got, err := st.Servers().Get(ctx, "node-a"); err != nil || got.Name != "主机A" {
		t.Fatalf("self row: %+v err=%v", got, err)
	}
	b, err := st.Servers().Get(ctx, "node-b")
	if err != nil || b.PortalIP != "10.0.0.2" || b.HAState != "standby" || b.LastSeenAt == nil || !b.LastSeenAt.Equal(now) {
		t.Fatalf("joined row: %+v err=%v", b, err)
	}

	// 再次注册（心跳）更新新鲜度和角色，保留操作者改过的名字。
	b.Name = "备机B"
	if err := st.Servers().Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * time.Second)
	reg.Now = func() time.Time { return later }
	if err := reg.Register(ctx, ha.NodeInfo{NodeID: "node-b", APIURL: "http://10.0.0.2:8080", PortalIP: "10.0.0.2", Role: "active", Epoch: 5}); err != nil {
		t.Fatal(err)
	}
	b2, _ := st.Servers().Get(ctx, "node-b")
	if b2.Name != "备机B" || b2.HAState != "active" || b2.Epoch != 5 || !b2.LastSeenAt.Equal(later) {
		t.Fatalf("heartbeat row: %+v", b2)
	}

	// Peers 返回完整名册。
	peers, err := reg.Peers(ctx)
	if err != nil || len(peers) != 2 {
		t.Fatalf("peers=%v err=%v", peers, err)
	}
}

// 备机按主机名册刷新时不能覆盖本机行（归注册循环管），并删掉主机不再列出的行。
func TestSyncPeersIntoStore(t *testing.T) {
	st := newImageTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	self := domain.Server{ID: "node-b", Name: "备机B", IP: "10.0.0.2", PortalIP: "10.0.0.2", APIURL: "http://10.0.0.2:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := EnsureLocalServer(ctx, st, self); err != nil {
		t.Fatal(err)
	}
	roster := []ha.NodeInfo{
		{NodeID: "node-a", APIURL: "http://10.0.0.1:8080", PortalIP: "10.0.0.250", Role: "active", Epoch: 7},
		{NodeID: "node-b", APIURL: "http://CLOBBER:1", PortalIP: "CLOBBER", Role: "standby"},
		{NodeID: "node-c", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "standby"},
	}
	if err := SyncPeersIntoStore(ctx, st, "node-b", roster, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Servers().Get(ctx, "node-b"); got.PortalIP != "10.0.0.2" {
		t.Fatalf("self clobbered: %+v", got)
	}
	if got, err := st.Servers().Get(ctx, "node-a"); err != nil || got.HAState != "active" {
		t.Fatalf("node-a: %+v err=%v", got, err)
	}
	if _, err := st.Servers().Get(ctx, "node-c"); err != nil {
		t.Fatalf("node-c missing: %v", err)
	}
	// node-c 离开集群，下次同步删除它。
	if err := SyncPeersIntoStore(ctx, st, "node-b", roster[:2], now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Servers().Get(ctx, "node-c"); err == nil {
		t.Fatal("node-c should be gone")
	}
}

// 克隆模板的机器共用 /etc/machine-id，上报同一 node_id；注册必须报冲突，不能把名册写乱。
func TestRegisterRejectsIdentityCollision(t *testing.T) {
	st := newImageTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }}

	if err := reg.Register(ctx, ha.NodeInfo{NodeID: "same-id", APIURL: "http://10.0.0.2:8080", PortalIP: "10.0.0.2", Role: "standby"}); err != nil {
		t.Fatal(err)
	}
	// 第一台仍新鲜时，另一台机器以同一身份注册。
	err := reg.Register(ctx, ha.NodeInfo{NodeID: "same-id", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "standby"})
	if err == nil {
		t.Fatal("want collision refused")
	}
	if !strings.Contains(err.Error(), "machine-id") {
		t.Fatalf("error must point at the real cause: %v", err)
	}
	// 第一台的行不变。
	got, _ := st.Servers().Get(ctx, "same-id")
	if got.IP != "10.0.0.2" {
		t.Fatalf("existing row clobbered: %+v", got)
	}

	// 旧行已不新鲜时，同一台机器换地址是合法的。
	reg.Now = func() time.Time { return now.Add(10 * time.Minute) }
	if err := reg.Register(ctx, ha.NodeInfo{NodeID: "same-id", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "standby"}); err != nil {
		t.Fatalf("stale row must be re-keyable: %v", err)
	}
}

// 不在册的新节点自报的 epoch 不作数（从 0 起算），否则带着旧库大 epoch 的机器
// 会经 KnownEpoch 把整个集群的栅栏顶高；已在册后照常按上报更新。
func TestRegisterDoesNotLetAStrangerAssertAHistoricalEpoch(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }}

	// 现任成员，集群 epoch 为 2。
	if err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "member", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "active", Epoch: 2,
	}); err != nil {
		t.Fatal(err)
	}

	// 陌生机器带着旧 epoch 加入。
	if err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "stranger", APIURL: "http://10.0.0.5:8080", PortalIP: "10.0.0.5", Role: "standby", Epoch: 2492,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Servers().Get(ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 0 {
		t.Fatalf("陌生节点的 epoch 被记成了 %d——花名册最大值是集群栅栏的下限，"+
			"这等于让一台刚回来的机器把整个集群顶到它的旧高度", got.Epoch)
	}
	// 但它要在册：可见、可放置、参与多数派计数。
	if got.Status != domain.ServerStatusUp || got.APIURL != "http://10.0.0.5:8080" {
		t.Fatalf("陌生节点仍应正常入册：%+v", got)
	}

	// 已在册的成员照常更新 epoch。
	if err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "member", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "active", Epoch: 3,
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := st.Servers().Get(ctx, "member")
	if m.Epoch != 3 {
		t.Fatalf("现任成员的 epoch 应当正常推进，实际 %d", m.Epoch)
	}
	// 陌生节点第二次上报时已在册，也照常更新。
	if err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "stranger", APIURL: "http://10.0.0.5:8080", PortalIP: "10.0.0.5", Role: "standby", Epoch: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s2, _ := st.Servers().Get(ctx, "stranger")
	if s2.Epoch != 1 {
		t.Fatalf("入册之后按上报值更新，实际 %d", s2.Epoch)
	}
}

// 摘除不可逆：不能摘本机，也不能摘还持有存储池的节点；干净的节点可以摘，且不影响其余节点。
func TestForgetNodeGuardsAgainstTheIrreversibleMistakes(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 8, 22, 5, 0, 0, 0, time.UTC)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }, SelfNodeID: "self"}

	for _, id := range []string{"self", "gone", "haspool"} {
		if err := reg.Register(ctx, ha.NodeInfo{
			NodeID: id, APIURL: "http://" + id + ":8080", PortalIP: "10.0.0.1", Role: "standby",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-haspool--tank", ServerID: "haspool", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	if err := reg.Forget(ctx, "self"); err == nil {
		t.Fatal("不能把本机从集群里摘掉")
	}
	if err := reg.Forget(ctx, "haspool"); err == nil {
		t.Fatal("还拥有存储池的节点不能摘——它的池记录会悬空")
	}
	if err := reg.Forget(ctx, "nobody"); err == nil {
		t.Fatal("不存在的节点要报错，而不是静静地成功")
	}

	if err := reg.Forget(ctx, "gone"); err != nil {
		t.Fatalf("干净的节点应当可以摘除：%v", err)
	}
	if _, err := st.Servers().Get(ctx, "gone"); err == nil {
		t.Fatal("摘除后仍在花名册里")
	}
	// 其余节点不受影响。
	if _, err := st.Servers().Get(ctx, "self"); err != nil {
		t.Fatalf("摘一台不该动到别人：%v", err)
	}
}

// 池记录可能已过时（跨节点销毁写不回主机的库），摘除时要问节点本身：说没有池
// 就清掉陈旧记录并放行；说有或问不到才拒绝。
func TestForgetPurgesStalePoolRowsWhenNodeReportsNone(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	live := map[string][]string{}
	var liveErr error
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }, SelfNodeID: "self",
		LivePools: func(_ context.Context, node string) ([]string, error) { return live[node], liveErr }}
	for _, id := range []string{"self", "stale"} {
		if err := reg.Register(ctx, ha.NodeInfo{NodeID: id, APIURL: "http://" + id + ":8080", Role: "standby"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-stale--ndpool", ServerID: "stale", Name: "ndpool"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PoolDisks().Create(ctx, domain.PoolDisk{ID: "pd-1", PoolID: "pool-stale--ndpool", Path: "/dev/sdb", Role: "data"}); err != nil {
		t.Fatal(err)
	}

	// 节点仍有池：拒绝。
	live["stale"] = []string{"ndpool"}
	if err := reg.Forget(ctx, "stale"); err == nil {
		t.Fatal("节点自己报告还有池，不能摘")
	}
	// 问不到节点：拒绝（记录是唯一证据）。
	live["stale"], liveErr = nil, errors.New("unreachable")
	if err := reg.Forget(ctx, "stale"); err == nil {
		t.Fatal("问不到节点时不能凭空放行")
	}
	// 节点报告没有池：记录已过时，清掉并摘除。
	liveErr = nil
	if err := reg.Forget(ctx, "stale"); err != nil {
		t.Fatalf("池已在节点上销毁，应清掉陈旧记录并摘除：%v", err)
	}
	if _, err := st.Servers().Get(ctx, "stale"); err == nil {
		t.Fatal("摘除后仍在花名册里")
	}
	if _, err := st.Pools().Get(ctx, "pool-stale--ndpool"); err == nil {
		t.Fatal("陈旧池行应随节点一起清掉")
	}
	disks, _ := st.PoolDisks().List(ctx)
	for _, d := range disks {
		if d.PoolID == "pool-stale--ndpool" {
			t.Fatal("陈旧池的盘记录应一起清掉")
		}
	}
}

// 按规则拒绝要返回 errs.ErrConflict 并点名，不能是裸 error（API 会回 500 并丢掉原文）。
func TestForgetRefusalsAreConflictsNotServerErrors(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }, SelfNodeID: "self"}
	for _, id := range []string{"self", "haspool"} {
		if err := reg.Register(ctx, ha.NodeInfo{NodeID: id, APIURL: "http://" + id + ":8080", Role: "standby"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-haspool--tank", ServerID: "haspool", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, node string }{
		{"本机", "self"}, {"还持有存储池", "haspool"},
	} {
		err := reg.Forget(ctx, c.node)
		if err == nil {
			t.Fatalf("%s：应当被拒", c.name)
		}
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("%s：应当是「按规则拒绝」而不是服务器错误，否则 API 会回 500 且丢掉原文：%v",
				c.name, err)
		}
		if !strings.Contains(err.Error(), c.node) && !strings.Contains(err.Error(), "本机") {
			t.Fatalf("%s：理由里要说清是哪一台：%v", c.name, err)
		}
	}
}

// 只有主机收心跳，备机要沿用主机给的 LastSeenAt；保留首次写入的时间会让备机
// 界面把健康集群显示为全部离线。
func TestSyncPeersIntoStoreCarriesFreshness(t *testing.T) {
	st := newImageTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	self := domain.Server{ID: "node-b", Name: "备机B", IP: "10.0.0.2", PortalIP: "10.0.0.2", APIURL: "http://10.0.0.2:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := EnsureLocalServer(ctx, st, self); err != nil {
		t.Fatal(err)
	}
	seen := t0
	roster := []ha.NodeInfo{{NodeID: "node-a", APIURL: "http://10.0.0.1:8080", PortalIP: "10.0.0.250", Role: "active", Epoch: 7, LastSeenAt: &seen}}
	if err := SyncPeersIntoStore(ctx, st, "node-b", roster, t0); err != nil {
		t.Fatal(err)
	}

	// 一小时后 node-a 仍在心跳，备机要采用主机给的新时间。
	fresh := t0.Add(time.Hour)
	roster[0].LastSeenAt = &fresh
	if err := SyncPeersIntoStore(ctx, st, "node-b", roster, fresh); err != nil {
		t.Fatal(err)
	}
	got, err := st.Servers().Get(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeenAt == nil || !got.LastSeenAt.Equal(fresh) {
		t.Fatalf("standby kept a stale heartbeat: %v want %v", got.LastSeenAt, fresh)
	}

	// 主机认为早已离线的节点，同步不能把它变成新鲜。
	stale := t0
	roster[0].LastSeenAt = &stale
	if err := SyncPeersIntoStore(ctx, st, "node-b", roster, fresh.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Servers().Get(ctx, "node-a"); got.LastSeenAt == nil || !got.LastSeenAt.Equal(stale) {
		t.Fatalf("standby invented freshness: %v want %v", got.LastSeenAt, stale)
	}
}

// 节点地址落在分组区间内时注册硬拒并点名分组和地址，否则 dnsmasq 会把该地址
// 发给客户机，节点从网络上消失。
func TestRegisterRefusesANodeInsideAGroupRange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	seedBackupSource(t, st, "img-1", "cfg-1")
	if err := st.Reductions().Create(ctx, domain.Reduction{
		ID: "red-1", ConfigID: "cfg-1", Name: "@base", DisplayName: "base",
		CreatedAt: now, Status: domain.ReductionStatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Groups().Create(ctx, domain.Group{
		ID: "g-1", Name: "教学一班", StartIP: "192.168.10.100", ClientMax: 100,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
		SystemReductionID: "red-1",
	}); err != nil {
		t.Fatal(err)
	}
	reg := ClusterRegistry{Store: st, SelfNodeID: "self", Now: func() time.Time { return now }}

	err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "node-b", APIURL: "http://192.168.10.150:8080", PortalIP: "192.168.10.150", Role: "standby",
	})
	if err == nil {
		t.Fatal("地址落在分组区间里的节点被收下了")
	}
	if !strings.Contains(err.Error(), "教学一班") || !strings.Contains(err.Error(), "192.168.10.150") {
		t.Fatalf("报错没说清撞的是谁：%v", err)
	}

	// 区间外的地址照常注册。
	if err := reg.Register(ctx, ha.NodeInfo{
		NodeID: "node-c", APIURL: "http://192.168.10.5:8080", PortalIP: "192.168.10.5", Role: "standby",
	}); err != nil {
		t.Fatalf("区间外的节点也被拦了：%v", err)
	}
}
