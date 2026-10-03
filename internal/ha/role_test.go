package ha

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 角色标记要能跨重启和 DB 替换保存：激活时 DB 会被对端副本覆盖，epoch 不能放在 DB 里。
func TestRoleStateFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ha-role")
	if rs, ok := ReadRoleState(path); ok {
		t.Fatalf("missing file read as %+v", rs)
	}
	if err := WriteRoleState(path, RoleState{Role: "active", Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	rs, ok := ReadRoleState(path)
	if !ok || rs.Role != "active" || rs.Epoch != 7 {
		t.Fatalf("read = %+v ok=%v", rs, ok)
	}
	// 覆盖是原子且整体的
	if err := WriteRoleState(path, RoleState{Role: "standby", Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	rs, _ = ReadRoleState(path)
	if rs.Role != "standby" {
		t.Fatalf("read = %+v", rs)
	}
}

type fakePeerStatus struct {
	status PeerStatus
	err    error
}

func (f fakePeerStatus) Status(context.Context) (PeerStatus, error) { return f.status, f.err }

func newTestController(t *testing.T) (*Controller, *[]int) {
	t.Helper()
	exits := &[]int{}
	c := &Controller{
		RoleFile:        filepath.Join(t.TempDir(), "ha-role"),
		Gate:            NewOpenGate(),
		Exit:            func(code int) { *exits = append(*exits, code) },
		PrepareActiveDB: func(context.Context) error { return nil },
	}
	return c, exits
}

// 激活打更高的 epoch 并以 active 重启，其余交给已幂等的启动路径。
func TestActivateStampsEpochAndRestarts(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	c.Peer = fakePeerStatus{err: errors.New("connection refused")} // 对端已死：继续

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" || rs.Epoch != 4 {
		t.Fatalf("state = %+v", rs)
	}
	if len(*exits) != 1 || (*exits)[0] != 0 {
		t.Fatalf("exits = %v", exits)
	}
}

// 激活守卫：可达对端仍以我们赢不了的 epoch 声称 active，说明是分区而非死亡，必须拒绝。
func TestActivateRefusedWhilePeerStillActive(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	c.Peer = fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 9}}

	err := c.Activate(context.Background(), "keepalived master")
	if err == nil {
		t.Fatal("activation must be refused")
	}
	// 只断言拒绝本身和理由里带出的判据，不咬字面措辞
	if !strings.Contains(err.Error(), "拒绝激活") || !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("err = %v", err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("state changed: %+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("exited: %v", exits)
	}
}

// 已降级（或 epoch 更低）的对端不阻止激活。
func TestActivateProceedsOverDemotedPeer(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	c.Peer = fakePeerStatus{status: PeerStatus{Role: "standby", Epoch: 5}}

	if err := c.Activate(context.Background(), "planned switch"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "active" || rs.Epoch != 6 {
		t.Fatalf("state = %+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("exits = %v", exits)
	}
}

// 对当前主机重复通知（keepalived 会这样做）不能让它循环重启。
func TestActivateIsIdempotentWhenAlreadyActive(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})

	if err := c.Activate(context.Background(), "keepalived re-notify"); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != 0 {
		t.Fatalf("re-notify restarted the service: %v", exits)
	}
}

// 备机激活前先放好复制来的 DB 副本：它自己的行只是备机私有状态，副本才是集群目录。
func TestActivatePreparesDBOnlyFromStandby(t *testing.T) {
	c, exits := newTestController(t)
	prepared := 0
	c.PrepareActiveDB = func(context.Context) error { prepared++; return nil }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 1})
	c.Peer = fakePeerStatus{err: errors.New("down")}

	if err := c.Activate(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 {
		t.Fatalf("prepared = %d", prepared)
	}
	// 完全没有 DB 副本：拒绝，而不是以空目录启动
	c2, _ := newTestController(t)
	c2.PrepareActiveDB = func(context.Context) error { return ErrNoDBCopy }
	_ = WriteRoleState(c2.RoleFile, RoleState{Role: "standby", Epoch: 1})
	c2.Peer = fakePeerStatus{err: errors.New("down")}
	if err := c2.Activate(context.Background(), "x"); err == nil {
		t.Fatal("empty catalogue activation must be refused")
	}
	_ = exits
}

// keepalived 启动时先进入 BACKUP 并触发 notify_backup，节点此时正要升为 MASTER。
// 在这里降级会让唯一的主机困在备机状态、再也拿不回 VIP；只在对端确实在服务时才降级。
func TestNotifyBackupIgnoredWhenNoPeerServing(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})
	c.Peer = fakePeerStatus{err: errors.New("connection refused")} // 没有对端在服务

	if err := c.NotifyBackup(context.Background(), "keepalived backup"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("a spurious startup backup notify demoted a sole active: %+v", rs)
	}
	if len(*exits) != 0 {
		t.Fatalf("restarted on a spurious backup notify: %v", exits)
	}
}

// 对端确实在服务时的 backup 通知是真降级：VIP 已移走，本节点必须让位。
func TestNotifyBackupDemotesWhenPeerServing(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})
	c.Peer = fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 5}} // 有对端在服务

	if err := c.NotifyBackup(context.Background(), "keepalived backup"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("a real demotion (peer serving) did not step down: %+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("real demotion did not restart: %v", exits)
	}
}

// 降级无条件执行：写标记、重启，由备机启动路径静默本节点。
func TestStandbyWritesMarkerAndRestarts(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})

	if err := c.Standby(context.Background(), "keepalived backup"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Role != "standby" || rs.Epoch != 4 {
		t.Fatalf("state = %+v", rs)
	}
	if len(*exits) != 1 {
		t.Fatalf("exits = %v", exits)
	}
	// 已是备机：不能循环重启
	*exits = (*exits)[:0]
	if err := c.Standby(context.Background(), "re-notify"); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != 0 {
		t.Fatalf("re-notify restarted: %v", exits)
	}
}

// 计划切换先排空：标记一轮、等备机拉走再降级。同步完不成就保持主机身份，丢数据的计划切换等于停机。
func TestPlannedSwitchSyncsThenStepsDown(t *testing.T) {
	c, _ := newTestController(t)
	restarted := make(chan struct{})
	c.Exit = func(int) { close(restarted) }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})
	synced := 0
	c.SyncBeforeSwitch = func(context.Context) error { synced++; return nil }

	if err := c.PlannedSwitch(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("没有重启")
	}
	if synced != 1 {
		t.Fatalf("synced=%d", synced)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("state = %+v", rs)
	}

	// 同步失败：保持主机
	c2, exits2 := newTestController(t)
	_ = WriteRoleState(c2.RoleFile, RoleState{Role: "active", Epoch: 4})
	c2.SyncBeforeSwitch = func(context.Context) error { return errors.New("对端 3 分钟未拉取") }
	if err := c2.PlannedSwitch(context.Background()); err == nil {
		t.Fatal("must refuse when the drain fails")
	}
	if rs, _ := ReadRoleState(c2.RoleFile); rs.Role != "active" || len(*exits2) != 0 {
		t.Fatalf("state=%+v exits=%v", rs, *exits2)
	}

	// 只有主机能发起
	c3, _ := newTestController(t)
	_ = WriteRoleState(c3.RoleFile, RoleState{Role: "standby", Epoch: 4})
	if err := c3.PlannedSwitch(context.Background()); err == nil {
		t.Fatal("standby must not initiate a planned switch")
	}
}

// 从未切换的节点角色来自配置：备机在首次切换前报 active 会触发对端守卫。
func TestStateDefaultsToConfiguredRole(t *testing.T) {
	c, _ := newTestController(t)
	c.DefaultRole = "standby"
	if rs := c.State(); rs.Role != "standby" || rs.Epoch != 0 {
		t.Fatalf("state = %+v", rs)
	}
	c2, _ := newTestController(t)
	if rs := c2.State(); rs.Role != "active" {
		t.Fatalf("state = %+v (empty default must stay active)", rs)
	}
}

// 替换活动 DB 必须连同 WAL 旁路文件一起删掉，否则新副本叠加旧 -wal 会读成损坏的数据库。
func TestReplaceLiveDBRemovesWALSidecars(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "ndiskless.db")
	for _, f := range []string{live, live + "-wal", live + "-shm"} {
		if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReplaceLiveDB(live, []byte("fresh-copy")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(live)
	if err != nil || string(b) != "fresh-copy" {
		t.Fatalf("live = %q err=%v", b, err)
	}
	for _, f := range []string{live + "-wal", live + "-shm"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived the replacement", f)
		}
	}
}

// 计划切换必须真的移走 VIP：降级节点约 2 秒重启完，健康在 fall 窗口结束前就变绿，VIP 不会离开。
// 降级留下限时标记让健康保持红色到对端接任，之后自行过期。
func TestStepDownMarkerIsTimeBoxed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "step-down")
	if StepDownActive(path, 60*time.Second) {
		t.Fatal("missing marker must not read as stepping down")
	}
	if err := MarkStepDown(path); err != nil {
		t.Fatal(err)
	}
	if !StepDownActive(path, 60*time.Second) {
		t.Fatal("fresh marker must read as stepping down")
	}
	// 把标记做旧到超出窗口：静默过期
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if StepDownActive(path, 60*time.Second) {
		t.Fatal("expired marker must not keep the node out of the running")
	}
}

// Standby() 写标记让 keepalived 在本节点重启期间放开 VIP；Activate() 清除标记，新主机立即有资格持有 VIP。
func TestStandbyMarksStepDownAndActivateClears(t *testing.T) {
	c, _ := newTestController(t)
	c.StepDownFile = filepath.Join(t.TempDir(), "step-down")
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 3})
	if err := c.Standby(context.Background(), "planned switch"); err != nil {
		t.Fatal(err)
	}
	if !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatal("demotion must mark step-down")
	}

	c.Peer = fakePeerStatus{err: errors.New("down")}
	if err := c.Activate(context.Background(), "back again"); err != nil {
		t.Fatal(err)
	}
	if StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatal("activation must clear the step-down marker")
	}
}

// 多节点下的激活判定：不再按多数派（理由见 role.go 中 activate 的注释），
// 要防的是第一台主机仍在服务时冒出第二台，由下面直接询问对端的用例守着。
func TestActivateFenceAgainstAServingPeer(t *testing.T) {
	unreachable := fakePeerStatus{err: context.DeadlineExceeded}
	standbyPeer := fakePeerStatus{status: PeerStatus{NodeID: "p", Role: "standby", Epoch: 3}}
	activePeer := fakePeerStatus{status: PeerStatus{NodeID: "p", Role: "active", Epoch: 9}}

	// 一个都联系不上也照样接管：这是最后幸存节点的处境，且它必须持有 VIP 才会走到这里。
	t.Run("nobody answers still proceeds", func(t *testing.T) {
		c, exits := newTestController(t)
		mustStandby(t, c)
		c.ClusterPeers = func(context.Context) []PeerStatusClient {
			return []PeerStatusClient{unreachable, unreachable}
		}
		if err := c.Activate(context.Background(), "test"); err != nil {
			t.Fatalf("err=%v exits=%v", err, *exits)
		}
	})
	t.Run("reachable standby peers proceed and adopt highest epoch", func(t *testing.T) {
		c, exits := newTestController(t)
		mustStandby(t, c)
		c.ClusterPeers = func(context.Context) []PeerStatusClient {
			return []PeerStatusClient{standbyPeer, unreachable} // N=3，可达 1 台
		}
		if err := c.Activate(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		if rs, _ := ReadRoleState(c.RoleFile); rs.Epoch != 4 || len(*exits) != 2 {
			t.Fatalf("rs=%+v exits=%v", rs, *exits)
		}
	})
	t.Run("reachable higher-epoch active refused", func(t *testing.T) {
		c, _ := newTestController(t)
		mustStandby(t, c)
		c.ClusterPeers = func(context.Context) []PeerStatusClient {
			return []PeerStatusClient{activePeer, standbyPeer}
		}
		if err := c.Activate(context.Background(), "test"); err == nil {
			t.Fatal("want refusal against live higher-epoch active")
		}
	})
	t.Run("force overrides minority", func(t *testing.T) {
		c, exits := newTestController(t)
		mustStandby(t, c)
		c.ClusterPeers = func(context.Context) []PeerStatusClient {
			return []PeerStatusClient{unreachable, unreachable}
		}
		if err := c.Activate(context.Background(), "operator"); err != nil || len(*exits) != 2 {
			t.Fatalf("err=%v exits=%v", err, *exits)
		}
	})
}

func mustStandby(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.Standby(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
}

// 角色切换是「打标退出、systemd 拉起」，期间几秒 /healthz 不应答。keepalived 探测分不出
// 这和节点已死，会放开 VIP、触发降级、再次重启，形成自激振荡（实测几分钟内重启 419 次）。
// 所以切换要先宣告：标记新鲜时探测把「连不上」当健康，过期后当死亡。
func TestSwitchMarkerCoversTheRestartWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "switching")

	if SwitchInProgress(path, 15*time.Second) {
		t.Fatal("no marker: must not claim a switch is in progress")
	}
	if err := MarkSwitching(path); err != nil {
		t.Fatal(err)
	}
	if !SwitchInProgress(path, 15*time.Second) {
		t.Fatal("fresh marker: a restart is under way")
	}
	// 过期的标记不能让真死掉的节点继续持有 VIP。
	old := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if SwitchInProgress(path, 15*time.Second) {
		t.Fatal("stale marker must expire, otherwise a crash-looping node keeps the VIP forever")
	}
	ClearSwitching(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("marker not cleared")
	}
}

// 两个方向的切换都要打标：激活和降级一样会重启。
func TestBothTransitionsStampTheSwitchMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Controller) error
	}{
		{"demote", func(c *Controller) error { return c.Standby(context.Background(), "test") }},
		{"activate", func(c *Controller) error {
			_ = c.Standby(context.Background(), "seed")
			return c.Activate(context.Background(), "test")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestController(t)
			c.SwitchFile = filepath.Join(t.TempDir(), "switching")
			if err := tc.run(c); err != nil {
				t.Fatal(err)
			}
			if !SwitchInProgress(c.SwitchFile, 15*time.Second) {
				t.Fatalf("%s did not announce the restart", tc.name)
			}
		})
	}
}

// keepalived 只在边沿通知一次，那次激活失败后就再没人重试，节点以备机身份持有 VIP，
// 集群没有写入者。所以节点要自己对账：持有 VIP 却不是写入者时自行激活。
func TestReconcileRoleActivatesWhenHoldingTheVIPAsStandby(t *testing.T) {
	c, exits := newTestController(t)
	if err := c.Standby(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	before := len(*exits)

	// 不持有 VIP：无需对账。
	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before {
		t.Fatal("a standby without the VIP must stay put")
	}

	// 持有 VIP：激活。
	if err := c.ReconcileRole(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before+1 {
		t.Fatal("standby holding the VIP did not re-activate")
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("role = %+v", rs)
	}
}

// 主机持有 VIP 是正常状态，对账必须什么也不做，不能每轮重启。
func TestReconcileRoleIsQuietWhenAlreadyActive(t *testing.T) {
	c, exits := newTestController(t)
	before := len(*exits)
	if err := c.ReconcileRole(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before {
		t.Fatalf("active node restarted for no reason: %v", *exits)
	}
}

// epoch 栅栏必须在整个集群单调。主机直接死掉时无人可问，一直当备机的节点会从自己的小数字
// 重新计数（实测 2390 → 4），旧主机回来时 epoch 更高、直接通过守卫，两台同时 active。
// 所以用名册里各节点上报的值作为下限。
func TestActivateTakesEpochFloorFromTheRoster(t *testing.T) {
	c, _ := newTestController(t)
	if err := c.Standby(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	c.Peer = fakePeerStatus{err: context.DeadlineExceeded} // 主机直接死掉
	c.KnownEpoch = func(context.Context) int64 { return 2390 }

	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Epoch != 2391 {
		t.Fatalf("epoch = %d, want 2391 (cluster floor + 1, not this node's own count)", rs.Epoch)
	}
}

// 名册知道的比本节点少时，不能把 epoch 拉低。
func TestActivateKeepsLocalEpochWhenRosterIsBehind(t *testing.T) {
	c, _ := newTestController(t)
	if err := WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 42}); err != nil {
		t.Fatal(err)
	}
	c.KnownEpoch = func(context.Context) int64 { return 7 }
	if err := c.Activate(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	rs, _ := ReadRoleState(c.RoleFile)
	if rs.Epoch != 43 {
		t.Fatalf("epoch = %d, want 43", rs.Epoch)
	}
}

// 另一半矛盾：以 active 运行却没有 VIP，这是脑裂中输的一侧，不处理就会一直双主。
// 只在确有他人服务时才让位：切换期间 VIP 会短暂不在任何节点上，据此降级会让集群没有写入者。
func TestReconcileRoleStepsDownWhenAnotherNodeServes(t *testing.T) {
	c, exits := newTestController(t)
	// 本节点是主机但不持有 VIP。
	if err := WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 5}); err != nil {
		t.Fatal(err)
	}
	before := len(*exits)

	// 没有别人声称在服务：保持原位（VIP 可能正在迁移）。
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{fakePeerStatus{status: PeerStatus{NodeID: "p", Role: "standby", Epoch: 4}}}
	}
	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before {
		t.Fatal("stepped down while nobody else was serving — that leaves the cluster with no writer")
	}

	// 确有对端在服务。一次观测不够（每次切换 VIP 都会短暂迁移），连续两次才让位。
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{fakePeerStatus{status: PeerStatus{NodeID: "p", Role: "active", Epoch: 6}}}
	}
	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before {
		t.Fatal("yielded on a single frame — that ping-pongs the roles during a failover")
	}
	if err := c.ReconcileRole(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != before+1 {
		t.Fatal("two actives left standing: the split brain was not resolved")
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("role = %+v", rs)
	}
}

// 退位是一次进程重启：先让调用方收到答复，再退出。
func TestPlannedSwitchAnswersBeforeRestarting(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 4})
	c.SyncBeforeSwitch = func(context.Context) error { return nil }
	restarted := make(chan struct{})
	c.Exit = func(int) { close(restarted) }

	if err := c.PlannedSwitch(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
		t.Fatal("答复之前就重启了")
	default:
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("返回时角色就应已写成备机: %+v", rs)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("答复之后没有重启")
	}
}
