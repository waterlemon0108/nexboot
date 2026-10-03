package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

// —— 栅栏所依赖的 epoch 必须可信 ——
//
// 两任主机共用同一个 epoch 时，判据 `对端.epoch ≥ 本机.epoch+1` 恒假，激活会在对端仍在服务时通过。
// 根因是 `servers` 表在整体复制的目录库里，备机拉目录时会覆盖「集群到过 30」的记忆，对端一死两个下限来源同时失效。
// 所以把见过的最高 epoch 也记在不被 DB 覆盖的角色文件里（见 RoleState）。

func TestSeenEpochSurvivesAndRaisesTheFloor(t *testing.T) {
	c, _ := newTestController(t)
	// 本机自己只到过 3，但它见过集群到过 30（对端曾经报过）
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3, SeenEpoch: 30})
	// 对端已死、名册也被复制回滚：两个下限来源同时失效
	c.Peer = fakePeerStatus{err: errors.New("connection refused")}
	c.KnownEpoch = func(context.Context) int64 { return 3 }

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Epoch != 31 {
		t.Fatalf("epoch = %d，应当从见过的 30 抬起再 +1。按本机的 3 算会得到 4，"+
			"而集群已经用过 4~30——两任主机共用 epoch，栅栏就此失效", rs.Epoch)
	}
}

// 见过就要记下且只增不减：这份记忆的价值就在于对端死后它还在。
func TestObservingAPeerRecordsTheHighestEpochSeen(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 2})

	c.observePeer(PeerStatus{NodeID: "peer", Role: "active", Epoch: 9})
	if rs, _ := ReadRoleState(c.RoleFile); rs.SeenEpoch != 9 {
		t.Fatalf("seen = %d，应记下 9", rs.SeenEpoch)
	}
	// 更低的观测不能把记忆拉低：对端重装后会从小数字重新开始
	c.observePeer(PeerStatus{NodeID: "peer", Role: "standby", Epoch: 1})
	if rs, _ := ReadRoleState(c.RoleFile); rs.SeenEpoch != 9 {
		t.Fatalf("seen = %d，低于已知值的观测不该把记忆抹低", rs.SeenEpoch)
	}
	c.observePeer(PeerStatus{NodeID: "peer", Role: "active", Epoch: 12})
	if rs, _ := ReadRoleState(c.RoleFile); rs.SeenEpoch != 12 {
		t.Fatalf("seen = %d，应更新到 12", rs.SeenEpoch)
	}
	// 记忆不能顺带改掉角色和本机 epoch
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" || rs.Epoch != 2 {
		t.Fatalf("观测不该动本机的角色与 epoch：%+v", rs)
	}
}

// —— 判据不再只依赖 epoch ——
//
// 对端此刻可达、自称主机且真的持有 VIP，就是在服务，与 epoch 高低无关。这是防双写的兜底。
func TestActivateRefusesWhenTheReachablePeerHoldsTheVIP(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 30})
	// 对端 epoch 比本机低：只看 epoch 的判据 `28 ≥ 31` 拦不住
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "active", Epoch: 28, HoldsVIP: true}}

	err := c.Activate(context.Background(), "test")
	if err == nil {
		t.Fatal("对端持有 VIP 且在服务时激活，就是双写——必须拒绝")
	}
	if len(*exits) != 0 {
		t.Fatalf("被拒的激活不该重启：exits=%v", *exits)
	}
	// 再试一次也一样被拦住
	if err := c.Activate(context.Background(), "test"); err == nil {
		t.Fatal("强制激活也必须被这道栅栏拦住")
	}
}

// 反面同样重要：自称主机却没有 VIP、epoch 也不占优的节点是卡住的旧角色，不是在服务的主机。
// 为它让路会把双写换成永久无主：VIP 持有者永远激活不了，客户机连得上地址却开不了机。
func TestActivateProceedsPastAStaleActiveThatHoldsNoVIP(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 30})
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "active", Epoch: 12, HoldsVIP: false}}

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatalf("卡住的旧主不该挡住合法接管，否则集群永远没有主机：%v", err)
	}
	if len(*exits) != 1 {
		t.Fatalf("激活要以重启落地：exits=%v", *exits)
	}
}

// 本机是否持有 VIP 要能报给对端，它是上面那道栅栏的输入。
func TestStatusReportsWhetherThisNodeHoldsTheVIP(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5})
	c.HoldsVIP = func() bool { return true }
	h := ControlHandler{Controller: c, NodeID: "self", Token: "t"}
	got := probeStatus(t, h, "t")
	if !got.HoldsVIP {
		t.Fatal("持有 VIP 却报成没有，对端的栅栏就少了最可靠的那个输入")
	}
	c.HoldsVIP = func() bool { return false }
	if probeStatus(t, h, "t").HoldsVIP {
		t.Fatal("没有 VIP 却报成有，会把合法接管挡住")
	}
	// 没接探针时保持沉默，不能乱报 true
	c.HoldsVIP = nil
	if probeStatus(t, h, "t").HoldsVIP {
		t.Fatal("没接探针时不能默认报 true")
	}
}

func probeStatus(t *testing.T, h ControlHandler, token string) PeerStatus {
	t.Helper()
	r := httptest.NewRequest("GET", "/internal/ha/status", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out PeerStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("状态解析失败：%v (%s)", err, rec.Body.String())
	}
	return out
}

// —— 计划切换：两种拒绝要能分辨 ——
//
// 「指错了机器」应换一台重发，「同步还没完成」应等复制追平。都报 409 的话运维无从区分。
func TestPlannedSwitchRefusalsAreDistinguishable(t *testing.T) {
	standby, _ := newTestController(t)
	_ = WriteRoleState(standby.RoleFile, RoleState{Role: "standby", Epoch: 5})
	errWrongNode := (StatusService{Controller: standby}).PlannedSwitch(context.Background())

	busy, _ := newTestController(t)
	_ = WriteRoleState(busy.RoleFile, RoleState{Role: "active", Epoch: 5})
	busy.SyncBeforeSwitch = func(context.Context) error { return errors.New("对端落后 3 轮") }
	errNotSynced := (StatusService{Controller: busy}).PlannedSwitch(context.Background())

	if errWrongNode == nil || errNotSynced == nil {
		t.Fatal("两种情形都该被拒")
	}
	if !IsNotActive(errWrongNode) {
		t.Fatalf("「指错了机器」要能被上层认出来：%v", errWrongNode)
	}
	if IsNotActive(errNotSynced) {
		t.Fatalf("「同步没完成」不该被当成「指错了机器」——处置完全不同：%v", errNotSynced)
	}
}

// 备机是下一任主机，最需要记住集群 epoch 有多高；自愈对账里它若直接 return、从不探测对端，
// 就永远学不到对方涨到了哪（实测连切三次 epoch 只涨一次）。探测只为记住，不据此做任何决定。
func TestStandbyLearnsThePeerEpochWhileIdling(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "active", Epoch: 42, HoldsVIP: true}}

	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.SeenEpoch != 42 {
		t.Fatalf("seen = %d，备机该记住集群已经到过 42。记不住的话，等对端死了它就"+
			"按自己的 3 算，两任主机共用 epoch，栅栏失效", rs.SeenEpoch)
	}
	// 只是记住，不改角色、不重启
	if rs.Role != "standby" || rs.Epoch != 3 {
		t.Fatalf("观测不该动本机状态：%+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("备机对账不该重启：exits=%v", *exits)
	}
}

// 进程刚启动时最需要学习对端 epoch（刚启动的进程很可能正处在一次切换中），但不能在此刻做角色决定：
// keepalived 还没配上 VIP，刚起来的主机会看到「自己没 VIP、对端在服务」而把自己退掉。所以启动时只观测，不裁决。
func TestObservePeersLearnsWithoutDeciding(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 3})
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "active", Epoch: 40, HoldsVIP: true}}

	c.ObservePeers(context.Background())

	rs, _ := ReadRoleState(c.RoleFile)
	if rs.SeenEpoch != 40 {
		t.Fatalf("seen = %d，启动那一下就该把对端的 40 记下来", rs.SeenEpoch)
	}
	if rs.Role != "active" {
		t.Fatalf("只观测不该改角色——启动瞬间 VIP 还没配上，据此退位会把刚起来的"+
			"主机自己退掉：%+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("只观测不该重启：exits=%v", *exits)
	}
	// 连续调用多次也不会攒出退位决定
	for i := 0; i < 5; i++ {
		c.ObservePeers(context.Background())
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("反复观测不该累计成退位：%+v", rs)
	}
}

// —— 启动时的角色必须与现实核对一次 ——
//
// 角色标记只记得「崩溃前我是主机」。崩溃的主机重启后会立刻开写闸门，而首次对账在 15 秒后、退位还要连续两轮，
// 中间有三四十秒双写窗口（还多出第二个 DHCP）。判据与自愈一致：本机没有 VIP 且确有别人在服务时才降为备机；
// 对端探不到不算，那通常是整机房刚上电，此时降级会让集群没有主机。
func TestStartingRoleChecksTheMarkerAgainstReality(t *testing.T) {
	ctx := context.Background()
	serving := fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "active", Epoch: 9, HoldsVIP: true}}
	quiet := fakePeerStatus{status: PeerStatus{NodeID: "peer", Role: "standby", Epoch: 9}}
	dead := fakePeerStatus{err: errors.New("connection refused")}
	active := RoleState{Role: "active", Epoch: 8}

	cases := []struct {
		name     string
		marker   RoleState
		holdsVIP bool
		peer     PeerStatusClient
		want     string
	}{
		{"崩溃前是主机、回来没 VIP、别人在服务 → 降为备机", active, false, serving, "standby"},
		{"仍持有 VIP → 保持主机（keepalived 没把它挪走）", active, true, serving, "active"},
		{"没人在服务 → 保持主机（降级会让集群无主）", active, false, quiet, "active"},
		{"对端探不到 → 保持主机（多半是整机房刚上电）", active, false, dead, "active"},
		{"本来就是备机 → 不变", RoleState{Role: "standby", Epoch: 8}, false, serving, "standby"},
	}
	for _, c := range cases {
		got := StartingRole(ctx, c.marker, c.holdsVIP, []PeerStatusClient{c.peer})
		if got != c.want {
			t.Fatalf("%s：得到 %q，要 %q", c.name, got, c.want)
		}
	}

	// 没有对端可问时（单机）保持标记原样
	if got := StartingRole(ctx, active, false, nil); got != "active" {
		t.Fatalf("单机形态应保持主机，得到 %q", got)
	}
}
