package ha

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWriter 是一台还活着的对端：答得上状态，也接得住交接请求。
type fakeWriter struct {
	status    PeerStatus
	final     string // 交接时打出的最后一轮
	prepErr   error
	prepared  int
	committed int
	aborted   int
}

func (f *fakeWriter) Status(context.Context) (PeerStatus, error) { return f.status, nil }
func (f *fakeWriter) PrepareHandover(context.Context, string) (string, error) {
	f.prepared++
	return f.final, f.prepErr
}
func (f *fakeWriter) CommitHandover(context.Context, string) error { f.committed++; return nil }
func (f *fakeWriter) AbortHandover(context.Context) error          { f.aborted++; return nil }

func newTakeoverController(t *testing.T, mine *string) (*Controller, *[]int) {
	t.Helper()
	c, exits := newTestController(t)
	dir := t.TempDir()
	c.NodeID = "node-b"
	c.StepDownFile = filepath.Join(dir, "step-down")
	c.YieldFile = filepath.Join(dir, "yielded")
	c.ReportFile = filepath.Join(dir, "ha-takeover.json")
	c.HoldsVIP = func() bool { return true }
	c.CatalogueMark = func(context.Context) string { return *mine }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	return c, exits
}

// 写入者活着但不健康（池降级、库写不进、维护标记）：它拿不回 VIP，让出去没意义；它还在继续打标记，
// 本机经 VIP 永远追不上。直接从它的节点地址追平再接任。
func TestActivateCatchesUpFromAnUnhealthyWriterInsteadOfYielding(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}, final: "rep-300"}
	c.Peer = writer
	var wanted []string
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error {
		wanted = append(wanted, target)
		mine = target
		return nil
	}

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatalf("应追平后接任: %v", err)
	}
	if len(*exits) != 1 {
		t.Fatalf("应重启为主机: exits=%v", *exits)
	}
	if StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatal("对端接不住虚 IP，不该让出")
	}
	if writer.prepared != 1 || len(wanted) != 1 || wanted[0] != "rep-300" {
		t.Fatalf("应先请写入者打最后一轮，再追到那一轮: prepared=%d wanted=%v", writer.prepared, wanted)
	}
	if writer.committed != 1 {
		t.Fatalf("追平后应请写入者退位: committed=%d", writer.committed)
	}
	if _, ok := ReadTakeoverReport(c.ReportFile); ok {
		t.Fatal("追平了就没有丢东西，不该留下接任记录")
	}
}

var errCatchUpTimedOut = errors.New("3 分钟内没有追平")

// 写入者健康：第一次让出 VIP，它的 keepalived 正常就会拿回去。十分钟内 VIP 又回到本机，说明它拿不回去，
// 再让只会让 VIP 在备机间来回漂。此时请它交出主机身份：关写入、打最后一轮，本机追平后接任，它退位。
func TestActivateStandsAsideOnceThenTakesTheRoleOver(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: true, Mark: "rep-100"}, final: "rep-150"}
	c.Peer = writer
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error { mine = target; return nil }

	err := c.Activate(context.Background(), "keepalived master")
	if err == nil || len(*exits) != 0 || !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatalf("第一次应让出虚 IP: err=%v exits=%v", err, *exits)
	}
	if writer.prepared != 0 {
		t.Fatal("让出时不该打扰写入者")
	}

	ClearStepDown(c.StepDownFile) // 一分钟后本机恢复参选，VIP 又回来了
	if err := c.Activate(context.Background(), "self-heal: VIP held while standby"); err != nil {
		t.Fatalf("虚 IP 又回到本机，应接过主机身份: %v", err)
	}
	if len(*exits) != 1 || StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatalf("应接任且不再让出: exits=%v", *exits)
	}
	if writer.prepared != 1 || writer.committed != 1 || mine != "rep-150" {
		t.Fatalf("应追到写入者的最后一轮并请它退位: prepared=%d committed=%d mine=%s", writer.prepared, writer.committed, mine)
	}
}

// 让出很久（超过十分钟）后 VIP 才又到本机：另一回事，重新从让出开始。
func TestActivateForgetsAnOldYield(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	now := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: true, Mark: "rep-100"}}
	c.Peer = writer
	_ = c.Activate(context.Background(), "keepalived master")
	ClearStepDown(c.StepDownFile)

	now = now.Add(11 * time.Minute)
	err := c.Activate(context.Background(), "keepalived master")
	if err == nil || len(*exits) != 0 || writer.prepared != 0 {
		t.Fatalf("隔了这么久应重新让出: err=%v exits=%v prepared=%d", err, *exits, writer.prepared)
	}
}

// 追不平（对端池坏到发不出流，或三分钟内没追上）：照样接任，VIP 上不能一直没有主机；
// 并写明双方各到了哪一轮，接任后运维在告警里看得到。
func TestActivateTakesOverAndReportsWhenCatchUpFails(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}, prepErr: errors.New("pool is suspended")}
	c.Peer = writer
	c.CatchUp = func(context.Context, PeerStatusClient, string) error { return errCatchUpTimedOut }

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatalf("追不平也要接任: %v", err)
	}
	if len(*exits) != 1 {
		t.Fatalf("应重启为主机: exits=%v", *exits)
	}
	report, ok := ReadTakeoverReport(c.ReportFile)
	if !ok || report.Peer != "node-a" || report.Mine != "rep-100" || report.Theirs != "rep-200" || report.Cause == "" {
		t.Fatalf("接任记录应写明双方各到哪一轮和原因: %+v ok=%v", report, ok)
	}
	if writer.committed != 1 {
		t.Fatal("接任了就要请旧主退位，两个主机更糟；它手里多出来的内容跟随时会被挪开保留")
	}
}

// 本机目录不完整且有健康对端：先让给它一次；它没接住（VIP 又回来了），就从它那里补齐再接任。
func TestActivateFillsInAnIncompleteCopyFromAPeer(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	missing := []string{"配置「办公」"}
	c.CatalogueIncomplete = func(context.Context) []string { return missing }
	peer := &fakeWriter{status: PeerStatus{NodeID: "node-c", Role: "standby", Epoch: 5, Healthy: true, Mark: "rep-100"}}
	c.Peer = peer
	c.CatchUp = func(context.Context, PeerStatusClient, string) error { missing = nil; return nil }

	err := c.Activate(context.Background(), "keepalived master")
	if err == nil || len(*exits) != 0 || !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatalf("第一次应让给健康的对端: err=%v exits=%v", err, *exits)
	}
	ClearStepDown(c.StepDownFile)
	if err := c.Activate(context.Background(), "self-heal"); err != nil || len(*exits) != 1 {
		t.Fatalf("补齐后应接任: err=%v exits=%v", err, *exits)
	}
	if peer.prepared != 0 || peer.committed != 0 {
		t.Fatalf("对端是备机，没有主机身份可交: prepared=%d committed=%d", peer.prepared, peer.committed)
	}
	if _, ok := ReadTakeoverReport(c.ReportFile); ok {
		t.Fatal("补齐了就不该留下接任记录")
	}
}

// 激活到一半拿不到 DB 副本：本机接不了任，要通知写入者交接取消，让它恢复写入。
func TestActivateTellsTheWriterWhenItCannotTakeOverAfterAll(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}, final: "rep-300"}
	c.Peer = writer
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error { mine = target; return nil }
	c.PrepareActiveDB = func(context.Context) error { return ErrNoDBCopy }

	err := c.Activate(context.Background(), "keepalived master")
	if !errors.Is(err, ErrNoDBCopy) || len(*exits) != 0 {
		t.Fatalf("err=%v exits=%v", err, *exits)
	}
	if writer.aborted != 1 || writer.committed != 0 {
		t.Fatalf("应取消交接而不是让写入者退位: aborted=%d committed=%d", writer.aborted, writer.committed)
	}
}

// 通知触发的激活还在追平（可能几分钟），自愈循环 15 秒一轮又来：不能并发再起一次。
func TestActivateAdmitsOneTakeoverAtATime(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	c.Peer = &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}}
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error {
		calls++
		close(entered)
		<-release
		mine = target
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- c.Activate(context.Background(), "keepalived master") }()
	<-entered
	if st := c.Takeover(); st.Phase != "catching-up" || st.Reason == "" {
		t.Fatalf("追平期间应说得出自己在做什么: %+v", st)
	}
	if err := c.Activate(context.Background(), "self-heal"); err != nil {
		t.Fatalf("第二次应直接返回: %v", err)
	}
	close(release)
	if err := <-done; err != nil || calls != 1 || len(*exits) != 1 {
		t.Fatalf("err=%v calls=%d exits=%v", err, calls, *exits)
	}
}

// 拒绝原因要保留给界面：持有 VIP 的备机上，运维打开页面看到的就是它。
func TestRefusalIsKeptForTheConsole(t *testing.T) {
	mine := "rep-100"
	c, _ := newTakeoverController(t, &mine)
	now := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	c.Peer = &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: true, Mark: "rep-100"}}
	_ = c.Activate(context.Background(), "keepalived master")

	st := c.Takeover()
	if st.Phase != "refused" || !strings.Contains(st.Reason, "node-a") {
		t.Fatalf("state = %+v", st)
	}
	now = now.Add(2 * time.Minute)
	if st := c.Takeover(); st.Phase != "" {
		t.Fatalf("过时的拒绝不该再显示: %+v", st)
	}
}

// 运维经 VIP 打开页面，落到持有 VIP 却未接任的备机，状态接口要说出它为什么没接任，页面顶部横幅靠它。
func TestHAStatusExplainsAStandbyHoldingTheVIP(t *testing.T) {
	mine := "rep-100"
	c, _ := newTakeoverController(t, &mine)
	c.Peer = &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: true, Mark: "rep-100"}}
	svc := StatusService{Controller: c, NodeID: "node-b"}

	st, _ := svc.Status(context.Background())
	if !st.HoldsVIP || st.Takeover != nil {
		t.Fatalf("还没尝试接任时只报持有虚 IP: %+v", st)
	}
	_ = c.Activate(context.Background(), "keepalived master")
	st, _ = svc.Status(context.Background())
	if st.Takeover == nil || st.Takeover.Phase != "refused" || !strings.Contains(st.Takeover.Reason, "node-a") {
		t.Fatalf("应说明没有接任的原因: %+v", st.Takeover)
	}
}

// 写入者不健康、两边标记相同：看似没什么可追，但它上一轮之后确认的改动还没进任何标记。
// 只要它还能应答，就请它关写入、打最后一轮，追到这一轮再接任。
func TestActivateAsksALiveWriterForAFinalRoundEvenWhenLevel(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-100"}, final: "rep-130"}
	c.Peer = writer
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error { mine = target; return nil }

	if err := c.Activate(context.Background(), "keepalived master"); err != nil || len(*exits) != 1 {
		t.Fatalf("err=%v exits=%v", err, *exits)
	}
	if writer.prepared != 1 || mine != "rep-130" || writer.committed != 1 {
		t.Fatalf("应追到最后一轮: prepared=%d mine=%s committed=%d", writer.prepared, mine, writer.committed)
	}
	if _, ok := ReadTakeoverReport(c.ReportFile); ok {
		t.Fatal("追平了，不该留下接任记录")
	}
}

// 写入者坏到打不出最后一轮、已知标记两边相同：没有可追的目标，不为它多等，立刻接任；
// 但它最后的改动是否带过来无从核对，要留下记录让运维知道。
func TestActivateDoesNotWaitOnAWriterThatCannotStampARound(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-100"},
		prepErr: errors.New("pool is suspended")}
	c.Peer = writer
	pulls := 0
	c.CatchUp = func(context.Context, PeerStatusClient, string) error { pulls++; return nil }

	if err := c.Activate(context.Background(), "keepalived master"); err != nil || len(*exits) != 1 {
		t.Fatalf("err=%v exits=%v", err, *exits)
	}
	if pulls != 0 {
		t.Fatalf("没有可追的目标，不该去拉: pulls=%d", pulls)
	}
	report, ok := ReadTakeoverReport(c.ReportFile)
	if !ok || report.Peer != "node-a" || !strings.Contains(report.Cause, "suspended") {
		t.Fatalf("应留下记录说明没能核对: %+v ok=%v", report, ok)
	}
	if writer.committed != 1 {
		t.Fatal("接任了就要请旧主退位")
	}
}

// 追平要几分钟，期间 VIP 被别的节点拿走（本机健康检查抖动、另一台优先级更高）：
// 本机再接任就成了没有 VIP 的主机。放弃，并让写入者恢复写入。
func TestActivateGivesUpWhenTheVIPLeftDuringCatchUp(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	holds := true
	c.HoldsVIP = func() bool { return holds }
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}, final: "rep-300"}
	c.Peer = writer
	c.CatchUp = func(_ context.Context, _ PeerStatusClient, target string) error {
		holds = false
		mine = target
		return nil
	}

	err := c.Activate(context.Background(), "keepalived master")
	if err == nil || !strings.Contains(err.Error(), "虚 IP") || len(*exits) != 0 {
		t.Fatalf("虚 IP 已不在本机，不该接任: err=%v exits=%v", err, *exits)
	}
	if writer.aborted != 1 || writer.committed != 0 {
		t.Fatalf("应取消交接: aborted=%d committed=%d", writer.aborted, writer.committed)
	}
}

// 探测时写入者还没持有 VIP，请它交接时它已拿回 VIP 在服务并答「不交出」。不能把这当成「打不出最后一轮」
// 照样接任再请它退位，否则它上一轮之后的改动会丢。对方说自己在服务就是活着的主机：放弃接任，与探测时一致。
func TestActivateStandsDownWhenTheWriterSaysItIsServing(t *testing.T) {
	mine := "rep-100"
	c, exits := newTakeoverController(t, &mine)
	writer := &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-100"},
		prepErr: ErrWriterServing}
	c.Peer = writer
	c.CatchUp = func(context.Context, PeerStatusClient, string) error { return nil }

	err := c.Activate(context.Background(), "self-heal")
	if err == nil || !strings.Contains(err.Error(), "node-a") || len(*exits) != 0 {
		t.Fatalf("对方正在服务，应放弃接任并点名它: err=%v exits=%v", err, *exits)
	}
	if writer.committed != 0 {
		t.Fatal("不接任就不能请对方退位")
	}
	if _, ok := ReadTakeoverReport(c.ReportFile); ok {
		t.Fatal("没有接任，不该留下接任记录")
	}
}
