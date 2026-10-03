package ha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writerUnderTest 是交接里「交出去」的那一侧：一台主机，经真实的 HTTP 接口被请求。
type writerUnderTest struct {
	c       *Controller
	peer    Peer
	exits   func() int
	stamped *[]time.Duration
	resumed *atomic.Int32
}

func newWriterUnderTest(t *testing.T) writerUnderTest {
	t.Helper()
	c, _ := newTestController(t)
	var mu sync.Mutex
	exited := 0
	c.Exit = func(int) { mu.Lock(); exited++; mu.Unlock() }
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "active", Epoch: 7})
	var stamped []time.Duration
	resumed := &atomic.Int32{}
	c.StampFinalRound = func(_ context.Context, hold time.Duration) (string, error) {
		stamped = append(stamped, hold)
		return "rep-900", nil
	}
	c.ResumeRounds = func() { resumed.Add(1) }
	srv := httptest.NewServer(ControlHandler{Controller: c, NodeID: "node-a", Token: "s3cret"})
	t.Cleanup(srv.Close)
	return writerUnderTest{c: c, peer: Peer{BaseURL: srv.URL, Token: "s3cret"},
		exits: func() int { mu.Lock(); defer mu.Unlock(); return exited }, stamped: &stamped, resumed: resumed}
}

// 交接第一步：主机先关写入再打最后一轮，这一轮之后不会再有改动，接任者追到它就是追平。
func TestPrepareHandoverClosesWritesAndStampsAFinalRound(t *testing.T) {
	w := newWriterUnderTest(t)
	var hp HandoverPeer = w.peer

	marker, err := hp.PrepareHandover(context.Background(), "node-b")
	if err != nil || marker != "rep-900" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	refused := w.c.Gate.Allow()
	if refused == nil || !strings.Contains(refused.Error(), "node-b") || !strings.Contains(refused.Error(), "稍后") {
		t.Fatalf("交接期间应拒绝改动，并说明原因和该怎么办: %v", refused)
	}
	if len(*w.stamped) != 1 {
		t.Fatalf("应打一轮: %v", *w.stamped)
	}
}

// 只有主机有身份可交。备机收到请求要拒绝，且不能弄乱自己的闸门状态。
func TestPrepareHandoverRefusedOnAStandby(t *testing.T) {
	w := newWriterUnderTest(t)
	_ = WriteRoleState(w.c.RoleFile, RoleState{Role: "standby", Epoch: 7})
	w.c.Gate.Close("本节点为备机", "http://192.168.10.250:8080")

	if _, err := w.peer.PrepareHandover(context.Background(), "node-b"); err == nil {
		t.Fatal("备机不该受理交接")
	}
	if refused := w.c.Gate.Allow(); refused == nil || !strings.Contains(refused.Error(), "备机") {
		t.Fatalf("备机的闸门不该被改动: %v", refused)
	}
	if len(*w.stamped) != 0 {
		t.Fatal("备机不该打标记")
	}
}

// 最后一轮打不出来（池坏了）：交接做不成，主机恢复写入，并把原因告诉对方。
func TestPrepareHandoverReopensWritesWhenTheRoundCannotBeStamped(t *testing.T) {
	w := newWriterUnderTest(t)
	w.c.StampFinalRound = func(context.Context, time.Duration) (string, error) {
		return "", errors.New("pool is suspended")
	}
	_, err := w.peer.PrepareHandover(context.Background(), "node-b")
	if err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("err=%v", err)
	}
	if w.c.Gate.Allow() != nil || w.resumed.Load() != 1 {
		t.Fatalf("应恢复写入与打标记: gate=%v resumed=%d", w.c.Gate.Allow(), w.resumed.Load())
	}
}

func TestAbortHandoverLetsTheWriterWriteAgain(t *testing.T) {
	w := newWriterUnderTest(t)
	if _, err := w.peer.PrepareHandover(context.Background(), "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := w.peer.AbortHandover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.c.Gate.Allow() != nil || w.resumed.Load() != 1 || w.exits() != 0 {
		t.Fatalf("取消后应恢复写入，且仍是主机: gate=%v resumed=%d exits=%d", w.c.Gate.Allow(), w.resumed.Load(), w.exits())
	}
}

// 请求交接的一方没了下文（自己挂了、网断了）：主机不能一直关着写入等它。
func TestHandoverExpiresWhenTheRequesterVanishes(t *testing.T) {
	w := newWriterUnderTest(t)
	w.c.HandoverHold = 60 * time.Millisecond
	if _, err := w.peer.PrepareHandover(context.Background(), "node-b"); err != nil {
		t.Fatal(err)
	}
	if w.c.Gate.Allow() == nil {
		t.Fatal("交接期间应关闭写入")
	}
	deadline := time.Now().Add(2 * time.Second)
	for w.c.Gate.Allow() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.c.Gate.Allow() != nil || w.resumed.Load() != 1 {
		t.Fatalf("到期后应自动恢复: gate=%v resumed=%d", w.c.Gate.Allow(), w.resumed.Load())
	}
}

// 交接最后一步：主机退位。对方要先收到答复，因为退位就是进程重启，连接会断。
func TestCommitHandoverStepsTheWriterDown(t *testing.T) {
	w := newWriterUnderTest(t)
	w.c.StepDownFile = t.TempDir() + "/step-down"
	if _, err := w.peer.PrepareHandover(context.Background(), "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := w.peer.CommitHandover(context.Background(), "node-b"); err != nil {
		t.Fatalf("对方应收到答复: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for w.exits() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rs, _ := ReadRoleState(w.c.RoleFile)
	if w.exits() != 1 || rs.Role != "standby" {
		t.Fatalf("应退位为备机: exits=%d role=%s", w.exits(), rs.Role)
	}
}

// keepalived 通知脚本用 curl 发激活请求，5 秒超时就断开；追平可能要几分钟，请求断了接任不能跟着断。
func TestActivationOutlivesTheNotifyRequest(t *testing.T) {
	mine := "rep-100"
	c, _ := newTakeoverController(t, &mine)
	restarted := make(chan struct{})
	c.Exit = func(int) { close(restarted) }
	c.Peer = &fakeWriter{status: PeerStatus{NodeID: "node-a", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-200"}}
	entered, release := make(chan struct{}), make(chan struct{})
	cancelled := make(chan bool, 1)
	c.CatchUp = func(ctx context.Context, _ PeerStatusClient, target string) error {
		close(entered)
		<-release
		cancelled <- ctx.Err() != nil
		mine = target
		return nil
	}
	srv := httptest.NewServer(ControlHandler{Controller: c, NodeID: "node-b", Token: "s3cret"})
	defer srv.Close()

	ctx, hangUp := context.WithCancel(context.Background())
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/internal/ha/activate", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	hangUp() // curl 超时走了
	time.Sleep(100 * time.Millisecond)
	close(release)
	if <-cancelled {
		t.Fatal("通知请求断开后，追平被一起取消了")
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("应完成接任")
	}
}

// 主机自己仍持有 VIP 并在服务：不交出身份，来要的那台弄错了。
func TestPrepareHandoverRefusedWhileTheWriterHoldsTheVIP(t *testing.T) {
	w := newWriterUnderTest(t)
	w.c.HoldsVIP = func() bool { return true }

	_, err := w.peer.PrepareHandover(context.Background(), "node-b")
	if err == nil || !strings.Contains(err.Error(), "虚 IP") {
		t.Fatalf("应拒绝: %v", err)
	}
	if w.c.Gate.Allow() != nil || len(*w.stamped) != 0 {
		t.Fatalf("拒绝时不该关写入、不该打标记: gate=%v stamped=%v", w.c.Gate.Allow(), *w.stamped)
	}
}

// 「持有 VIP、正在服务」要能穿过 HTTP 被识别出来，与「打不出最后一轮」区分开。
func TestPrepareHandoverServingRefusalIsRecognisable(t *testing.T) {
	w := newWriterUnderTest(t)
	w.c.HoldsVIP = func() bool { return true }
	_, err := w.peer.PrepareHandover(context.Background(), "node-b")
	if !errors.Is(err, ErrWriterServing) {
		t.Fatalf("应认得出对方正在服务: %v", err)
	}
	w.c.HoldsVIP = func() bool { return false }
	w.c.StampFinalRound = func(context.Context, time.Duration) (string, error) { return "", errors.New("pool is suspended") }
	if _, err := w.peer.PrepareHandover(context.Background(), "node-b"); err == nil || errors.Is(err, ErrWriterServing) {
		t.Fatalf("打不出最后一轮是另一回事: %v", err)
	}
}
