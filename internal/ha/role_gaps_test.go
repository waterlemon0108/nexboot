package ha

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// —— 栅栏的两半：抬升下限，以及把结果记下来 ——
//
// epoch 是防双写的唯一凭据，靠两件事维持：激活前把下限抬到集群到过的高度，激活后把新高度记下让别人看见。
// 缺一半，两任主机就会共用同一个 epoch，旧主机回归再也挡不住。

// 对端报出的 epoch 比本机高时，必须抬升下限再 +1，而不是按本机旧值 +1。
func TestActivateRaisesEpochFloorFromThePeerProbe(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	// 对端已退位，但它到过 epoch 9：集群高度是 9，不是本机记得的 3
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "standby", Epoch: 9}}

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Epoch != 10 {
		t.Fatalf("epoch = %d，应当从对端的 9 抬起再 +1；按本机旧值算会得到 4，"+
			"于是两任主机共用 epoch，栅栏失效", rs.Epoch)
	}
}

// 激活成功后要把新 epoch 写入名册，否则别人（包括重启后的自己）看到的仍是旧值，下次激活会重用同一个 epoch。
func TestActivateRecordsTheNewEpochWhereOthersCanSeeIt(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 4})
	var recorded []int64
	c.RecordEpoch = func(_ context.Context, e int64) { recorded = append(recorded, e) }

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if len(recorded) != 1 || recorded[0] != 5 {
		t.Fatalf("落库的 epoch = %v，应当只落一次且等于 5", recorded)
	}
	if recorded[0] != rs.Epoch {
		t.Fatalf("落库 %d 与角色文件 %d 不一致——两处说法不同时，谁也不知道集群到过哪",
			recorded[0], rs.Epoch)
	}
}

// 对端探不到时不阻塞激活：keepalived 只在自己的健康检查失败后才交出 VIP。但此时下限只能来自本机与名册，
// 这里钉住这一点，免得以为「探不到」也会抬升下限。
func TestActivateProceedsWhenThePeerCannotBeProbed(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 6})
	c.Peer = fakePeerStatus{err: errors.New("dial tcp: connection refused")}

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatalf("对端探不到不应阻塞激活：%v", err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" || rs.Epoch != 7 {
		t.Fatalf("state = %+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("激活要以重启落地，exits=%v", *exits)
	}
}

// 不再有 force；对端仍在服务时，任何激活都必须被拦。
func TestActivateRefusedWhileAPeerStillServes(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 1})
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errors.New("down")},
			fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 9, HoldsVIP: true}},
		}
	}
	if err := c.Activate(ctx, "test"); err == nil {
		t.Fatal("对端仍在服务，激活必须被拦住")
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("被拦下之后角色不该变：%+v", rs)
	}
}

func TestPlannedSwitchRefusesOnAStandby(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})

	err := (StatusService{Controller: c}).PlannedSwitch(context.Background())
	if err == nil {
		t.Fatal("备机不能发起计划切换：它本来就没有主机身份可交")
	}
	if !strings.Contains(err.Error(), "只有主机") {
		t.Fatalf("理由要说清是「指错了机器」：%v", err)
	}
	if len(*exits) != 0 {
		t.Fatalf("被拒的切换不该重启进程：exits=%v", *exits)
	}
}

func TestPlannedSwitchKeepsTheRoleWhenTheDrainFails(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5})
	c.SyncBeforeSwitch = func(context.Context) error { return errors.New("对端未追平") }

	err := (StatusService{Controller: c}).PlannedSwitch(context.Background())
	if err == nil {
		t.Fatal("同步没完成就退位，等于把还没复制过去的改动丢掉")
	}
	if !strings.Contains(err.Error(), "已保持主机身份") {
		t.Fatalf("理由要说清本机仍是主机：%v", err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" {
		t.Fatalf("拒绝之后必须仍是主机，实际 %+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("没切成就不该重启：exits=%v", *exits)
	}
}

func TestPlannedSwitchDrainsThenStepsDown(t *testing.T) {
	c, _ := newTestController(t)
	restarted := make(chan struct{})
	c.Exit = func(int) { close(restarted) }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5})
	c.StepDownFile = filepath.Join(t.TempDir(), "stepdown")
	order := []string{}
	c.SyncBeforeSwitch = func(context.Context) error { order = append(order, "drain"); return nil }

	if err := (StatusService{Controller: c}).PlannedSwitch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || order[0] != "drain" {
		t.Fatalf("退位前必须先把复制排空：%v", order)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "standby" || rs.Epoch != 5 {
		t.Fatalf("退位保留 epoch（不是新一任，不该 +1）：%+v", rs)
	}
	// 要写下退位标记，keepalived 才知道该让出 VIP
	if !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatal("没写退位标记的话，VIP 不会动，切换只完成了一半")
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("退位以重启落地，但没有重启")
	}
}

// —— HA 状态页：对端挂了尤其要渲染得出来 ——
func TestStatusRendersWithAnUnreachablePeer(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 8})
	s := StatusService{
		Controller: c, NodeID: "self", PeerURL: "http://peer:8080",
		Peer: fakePeerStatus{err: errors.New("down")},
		Rate: func(context.Context) (int, string, error) { return 100, "setting", nil },
	}
	out, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("对端挂了也必须给得出本机视图：%v", err)
	}
	if out.Role != "active" || out.Epoch != 8 || out.NodeID != "self" {
		t.Fatalf("本机部分不对：%+v", out)
	}
	if out.PeerReachable || out.PeerRole != "" {
		t.Fatalf("探不到就要如实说探不到，而不是留空当成正常：%+v", out)
	}
	if out.RateMBPS != 100 || out.RateSource != "setting" {
		t.Fatalf("限速没带出来：%+v", out)
	}

	// 对端活着时带出它的角色与 epoch：运维靠这两个数判断该不该切
	s.Peer = fakePeerStatus{status: PeerStatus{Role: "standby", Epoch: 7}}
	out, _ = s.Status(context.Background())
	if !out.PeerReachable || out.PeerRole != "standby" || out.PeerEpoch != 7 {
		t.Fatalf("对端信息没带出来：%+v", out)
	}
}

func TestSetRateSaysSoWhenUnavailable(t *testing.T) {
	c, _ := newTestController(t)
	if err := (StatusService{Controller: c}).SetRate(context.Background(), 50); err == nil {
		t.Fatal("没接存储时要明说不可用，而不是假装存下了")
	}
	saved := -1
	s := StatusService{Controller: c, SaveRate: func(_ context.Context, m int) error { saved = m; return nil }}
	if err := s.SetRate(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Fatalf("0 表示不限速，必须原样存下去，实际 %d", saved)
	}
}

// —— 自愈对账的另一半：不持 VIP 却自称主机 ——
//
// 不持 VIP 的主机是隐形的双写源：照样接受写、照样渲染 dnsmasq，只是没人经 VIP 找得到它。
// 但也不能一看到自己没 VIP 就退位：VRRP 有抖动，切换过程中也有短暂空窗。

func TestReconcileRoleStaysActiveWhileNobodyElseIsServing(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 3})
	c.Peer = fakePeerStatus{status: PeerStatus{Role: "standby", Epoch: 3}}

	for i := 0; i < 5; i++ {
		if err := c.ReconcileRole(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" {
		t.Fatalf("没人在服务时不能自行退位——那会让集群一个主机都不剩：%+v", rs)
	}
}

func TestReconcileRoleStaysActiveWhenThePeerCannotBeReached(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 3})
	c.Peer = fakePeerStatus{err: errors.New("down")}

	for i := 0; i < 5; i++ {
		if err := c.ReconcileRole(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" {
		t.Fatalf("探不到对端 ≠ 对端在服务。据此退位的话，一次网络抖动就能让集群无主：%+v", rs)
	}
}

// 连续两轮观测才算数：切换中会出现「对方已激活、VIP 还没漂过去」的瞬间，一轮就退位会让两边互相让位。
func TestReconcileRoleNeedsTwoConsecutiveRoundsBeforeStandingDown(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 3})
	serving := fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 4}}
	quiet := fakePeerStatus{status: PeerStatus{Role: "standby", Epoch: 4}}

	c.Peer = serving
	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("第一轮就退位了：%+v", rs)
	}
	// 中间夹一轮「对方并没在服务」，计数必须归零
	c.Peer = quiet
	_ = c.ReconcileRole(context.Background(), false)
	c.Peer = serving
	_ = c.ReconcileRole(context.Background(), false)
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("中断过的观察不该累计——否则抖动攒够次数也会触发退位：%+v", rs)
	}
	// 连续两轮：这才退位
	_ = c.ReconcileRole(context.Background(), false)
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("连续两轮确认有人在服务后应当退位：%+v", rs)
	}
}

// —— 退位本身 ——
func TestStandbyIsIdempotentAndSurvivesAMarkerFailure(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 2})
	if err := c.Standby(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != 0 {
		t.Fatalf("已经是备机就什么都不做，重启一次纯属自伤：exits=%v", *exits)
	}

	// 标记写不下去（目录不可写）也要完成退位：角色错了比标记丢了严重得多
	c2, exits2 := newTestController(t)
	_ = WriteRoleState(c2.RoleFile, RoleState{Role: "active", Epoch: 2})
	c2.StepDownFile = filepath.Join(t.TempDir(), "no-such-dir", "stepdown")
	if err := c2.Standby(context.Background(), "planned"); err != nil {
		t.Fatalf("标记写失败不该中断退位：%v", err)
	}
	if rs, _ := ReadRoleState(c2.RoleFile); rs.Role != "standby" {
		t.Fatalf("角色没落地：%+v", rs)
	}
	if len(*exits2) != 1 {
		t.Fatalf("退位仍要以重启落地：exits=%v", *exits2)
	}
}

// 不做多数派，VIP 是仲裁者（理由见 role.go 中 activate 的注释）。
// 多数派会把不参与选主的节点也算一票，导致第三台宕机后主备对失去自动切换：VIP 持有者每 10 秒被拦一次。
func TestActivateNoLongerNeedsAMajorityWhenNobodyAnswers(t *testing.T) {
	ctx := context.Background()
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	// 三节点集群剩下的最后一台：两个对端都联系不上
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errors.New("down")},
			fakePeerStatus{err: errors.New("down")},
		}
	}
	if err := c.Activate(ctx, "test"); err != nil {
		t.Fatalf("最后一台持有 VIP 的节点应当能接管：%v", err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" || rs.Epoch != 6 {
		t.Fatalf("state = %+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("激活要以重启落地，exits=%v", *exits)
	}
}

// 但栅栏一步不能松：只要有一个对端应答且在服务，照样拒绝，这才是真正防双写的那道。
func TestActivateStillRefusesWhenSomePeerIsServing(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errors.New("down")},
			fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 9, HoldsVIP: true}},
		}
	}
	if err := c.Activate(ctx, "test"); err == nil {
		t.Fatal("有节点仍在服务，激活必须被拦住")
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "standby" {
		t.Fatalf("被拦下之后角色不该变：%+v", rs)
	}
}

// 被隔离的备机持有 VIP，每 10 秒尝试激活：激活成功、重启成主机，启动核对发现对端在服务又降回备机，
// 循环往复，每轮都是一次服务重启。所以降级过的节点要先冷却：对端真死了只是晚一分钟接任。
func TestDemotedAtStartupBacksOffBeforeTryingAgain(t *testing.T) {
	ctx := context.Background()
	c, exits := newTestController(t)
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	demoted := now.Add(-10 * time.Second)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5, DemotedAt: &demoted})
	// 探不到任何对端：正是被隔离时的处境
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errors.New("down")},
			fakePeerStatus{err: errors.New("down")},
		}
	}

	if err := c.ReconcileRole(ctx, true); err != nil {
		t.Fatalf("退避期内不该报错，只是不动作：%v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("退避期内不该激活：%+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("退避期内不该重启：%v", *exits)
	}

	// 冷却过后照常接任：退避是压抖动，不是永久禁用
	now = now.Add(2 * time.Minute)
	if err := c.ReconcileRole(ctx, true); err != nil {
		t.Fatalf("冷却之后应当能接管：%v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("冷却之后没接管：%+v", rs)
	}
}

// 退避压的是抖动，不能吞掉原因。刚被降级又持有 VIP 的节点，若对端正在服务，不激活的真正原因是
// 「对端还在服务」，必须说出来，否则运维只看到 VIP 有人持却没有主机、日志里什么都没有。
// 退避只在无人应答、无从判断时才生效。
func TestBackoffStillReportsWhyWhenAPeerIsServing(t *testing.T) {
	ctx := context.Background()
	c, exits := newTestController(t)
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	demoted := now.Add(-10 * time.Second) // 仍在退避窗口内
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5, DemotedAt: &demoted})
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{status: PeerStatus{NodeID: "peer-1", Role: "active", Epoch: 9, HoldsVIP: true}},
			fakePeerStatus{err: errors.New("down")},
		}
	}

	err := c.ReconcileRole(ctx, true)
	if err == nil {
		t.Fatal("对端仍在服务时，不激活要给出理由，不能静悄悄地返回成功")
	}
	if !strings.Contains(err.Error(), "拒绝激活以避免双写") {
		t.Fatalf("理由要写明为什么拒绝：%v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("不该激活：%+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("不该重启：%v", *exits)
	}
}

// 退避只在谁都探不到时生效。能探到对端且没有一台在服务，就是已核实的接任；计划切换正是这样
// （当前主机退成备机并照常应答）。若这里也退避，VIP 会在备机上停留 90 秒，运维发起的切换等于没发生。
func TestBackoffDoesNotDelayAVerifiedTakeover(t *testing.T) {
	ctx := context.Background()
	c, exits := newTestController(t)
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	demoted := now.Add(-10 * time.Second) // 仍在退避窗口内
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5, DemotedAt: &demoted})
	// 对端都能应答且都不是主机：没有第二个写入者的风险
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{status: PeerStatus{NodeID: "peer-1", Role: "standby", Epoch: 5}},
			fakePeerStatus{status: PeerStatus{NodeID: "peer-2", Role: "standby", Epoch: 5}},
		}
	}

	if err := c.ReconcileRole(ctx, true); err != nil {
		t.Fatalf("已核实的接管不该报错：%v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("对端都答话且无人在服务时必须立刻接管，不能等冷却：%+v", rs)
	}
	if len(*exits) == 0 {
		t.Fatal("接管要重启进主机角色")
	}
}

// 探测对端必须有超时：网络分区是丢包而非拒连，TCP 会挂到内核超时，而 ha.Peer 用的是没有超时的 http.DefaultClient。
// 不设超时时实测自愈判定激活后十五分钟才真正激活，期间 VIP 有人持、集群没有主机。
func TestPeerProbesAreBounded(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t)
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})

	var deadlines []bool
	probe := probeFunc(func(pctx context.Context) (PeerStatus, error) {
		_, ok := pctx.Deadline()
		deadlines = append(deadlines, ok)
		return PeerStatus{}, errors.New("blackholed")
	})
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{probe, probe}
	}

	_ = c.ReconcileRole(ctx, true)
	if len(deadlines) == 0 {
		t.Fatal("没有探测对端")
	}
	for i, ok := range deadlines {
		if !ok {
			t.Fatalf("第 %d 次探测没有超时上限：丢包的对端会把激活挂死", i+1)
		}
	}
}

// probeFunc 把一个函数当成 PeerStatusClient 用。
type probeFunc func(context.Context) (PeerStatus, error)

func (f probeFunc) Status(ctx context.Context) (PeerStatus, error) { return f(ctx) }

// 启动时的现实核对也要有超时上限：分区期间重启的机器会卡在询问对端上，服务起不来。
func TestStartingRoleProbesAreBounded(t *testing.T) {
	var bounded []bool
	probe := probeFunc(func(pctx context.Context) (PeerStatus, error) {
		_, ok := pctx.Deadline()
		bounded = append(bounded, ok)
		return PeerStatus{}, errors.New("blackholed")
	})
	// 角色文件说自己是主机、手里却没有 VIP：正是要与现实核对的时刻。
	StartingRole(context.Background(), RoleState{Role: "active", Epoch: 3},
		false, []PeerStatusClient{probe, probe})
	if len(bounded) == 0 {
		t.Fatal("没有探测对端")
	}
	for i, ok := range bounded {
		if !ok {
			t.Fatalf("第 %d 次探测没有超时上限：分区时重启会卡在这里起不来", i+1)
		}
	}
}

// 两节点集群名册里只有对端一台，NDISKLESS_PEER_URL 却是 VIP：接任时必须问名册里的对端，
// 问 VIP 等于问自己，会把仍在服务的对端漏掉造成双写。
func TestActivateAsksTheSingleRosterPeerNotThePeerURL(t *testing.T) {
	c, exits := newTestController(t)
	c.NodeID = "n1"
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{fakePeerStatus{status: PeerStatus{NodeID: "n2", Role: "active", Epoch: 4, HoldsVIP: true}}}
	}
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "n1", Role: "standby", Epoch: 3}}

	if err := c.Activate(context.Background(), "keepalived master"); err == nil {
		t.Fatal("名册里的对端仍在服务，激活必须被拦住")
	}
	if len(*exits) != 0 {
		t.Fatalf("被拦下不该重启：exits=%v", *exits)
	}
}

// 应答的 NodeID 是本机时不能当对端：拿自己当「仍在服务的对端」会让节点永远接不了任。
func TestActivateIgnoresAnAnswerFromItself(t *testing.T) {
	c, exits := newTestController(t)
	c.NodeID = "n1"
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	self := fakePeerStatus{status: PeerStatus{NodeID: "n1", Role: "active", Epoch: 9, HoldsVIP: true}}
	c.ClusterPeers = func(context.Context) []PeerStatusClient { return []PeerStatusClient{self} }
	c.Peer = self

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatalf("自己的应答不算对端，不该据此拒绝：%v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" || rs.Epoch != 4 {
		t.Fatalf("应以本机 epoch +1 接任，不能抬到自己报的 9：%+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("接任要重启：exits=%v", *exits)
	}
}

// 计划切换打最后一轮之前就要关闸：排空要几分钟，期间落地的改动不在最后一轮里，切换后会丢。
func TestPlannedSwitchClosesTheGateBeforeTheFinalRound(t *testing.T) {
	c, _ := newTestController(t)
	c.Exit = func(int) {}
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5})
	var during error
	c.SyncBeforeSwitch = func(context.Context) error { during = c.Gate.Allow(); return nil }

	if err := c.PlannedSwitch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if during == nil {
		t.Fatal("排空期间闸门必须已关，否则最后一轮之后的写入会在切换后丢失")
	}
	if !strings.Contains(during.Error(), "正在切换主机") {
		t.Fatalf("拒绝写入的提示要说清在切换：%v", during)
	}
}

// 排空失败、保持主机身份时必须重新开闸，否则主机一直拒绝写入。
func TestPlannedSwitchReopensTheGateWhenTheDrainFails(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5})
	c.SyncBeforeSwitch = func(context.Context) error { return errors.New("对端未追平") }

	if err := c.PlannedSwitch(context.Background()); err == nil {
		t.Fatal("排空失败必须拒绝切换")
	}
	if err := c.Gate.Allow(); err != nil {
		t.Fatalf("仍是主机就要重新接受写入：%v", err)
	}
}

// 名册为空时退回问 c.Peer（界面建集群时是 VIP）；本机持有 VIP 时问到的是自己，
// 自己的 active 应答不能让 keepalived 的 backup 通知把本机降级。
func TestPeerIsServingIgnoresAnAnswerFromItself(t *testing.T) {
	c, _ := newTestController(t)
	c.NodeID = "n1"
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "n1", Role: "active", Epoch: 4, HoldsVIP: true}}
	if c.peerIsServing(context.Background()) {
		t.Fatal("把自己当成了在服务的对端")
	}
}
