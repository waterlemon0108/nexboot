package assets

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/storage/remote"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

type fakeReclaimStorage struct {
	storage.StorageAgent
	sessions    []string
	sessionsErr error
	clones      []string
	clonesErr   error
	cleaned     []string
	failFor     map[string]error
}

func (s *fakeReclaimStorage) ActiveClientMACs(context.Context) ([]string, error) {
	return s.sessions, s.sessionsErr
}

func (s *fakeReclaimStorage) ClientCloneMACs(context.Context) ([]string, error) {
	return s.clones, s.clonesErr
}

func (s *fakeReclaimStorage) CleanupClientClones(_ context.Context, mac string, _ []string) error {
	s.cleaned = append(s.cleaned, mac)
	return s.failFor[mac]
}

func (s *fakeReclaimStorage) ReclaimIdleClientClones(ctx context.Context, mac string, _ time.Time) (bool, error) {
	err := s.CleanupClientClones(ctx, mac, nil)
	return err == nil, err
}

// seedOfflineTerminal 创建一台在 offlineAt 已确认离线的终端。
func seedOfflineTerminal(t *testing.T, ctx context.Context, st *store.SQLStore, id, mac, ip, groupID string, offlineAt time.Time, super bool) {
	t.Helper()
	if err := st.Terminals().Create(ctx, domain.Terminal{
		ID: id, MAC: mac, IP: ip, GroupID: groupID,
		State: domain.TerminalStateOffline, OfflineAt: &offlineAt, IsSuper: super,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimOfflineClonesWaitsOutGracePeriod(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 会话断开不等于关机，只有整个宽限期都无会话的终端才回收克隆。
	seedOfflineTerminal(t, ctx, st, "term-1", "AABBCCDDEE01", "192.168.1.10", group.ID, now.Add(-cloneGracePeriod), false)
	seedOfflineTerminal(t, ctx, st, "term-2", "AABBCCDDEE02", "192.168.1.11", group.ID, now.Add(-cloneGracePeriod+time.Second), false)

	agent := &fakeReclaimStorage{clones: []string{"AABBCCDDEE01", "AABBCCDDEE02"}}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	reclaimed, err := service.ReclaimOfflineClones(ctx, map[string]time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	if !reflect.DeepEqual(agent.cleaned, []string{"AABBCCDDEE01"}) {
		t.Fatalf("cleaned = %v, want [AABBCCDDEE01] (term-2 is still inside the grace period)", agent.cleaned)
	}
}

func TestReclaimOfflineClonesSkipsSuperTerminals(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 超管机的盘是持久工作成果，离线多久都不回收。
	seedOfflineTerminal(t, ctx, st, "term-1", "AABBCCDDEE01", "192.168.1.10", group.ID, now.Add(-24*time.Hour), true)

	agent := &fakeReclaimStorage{clones: []string{"AABBCCDDEE01"}}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	if _, err := service.ReclaimOfflineClones(ctx, map[string]time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v, want none (super clone must survive)", agent.cleaned)
	}
}

func TestReclaimOfflineClonesSpareLiveSession(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 终端记录显示早已离线，但节点报告有活动会话：以会话为准，否则正在开机的客户机会丢盘。
	seedOfflineTerminal(t, ctx, st, "term-1", "AABBCCDDEE01", "192.168.1.10", group.ID, now.Add(-24*time.Hour), false)

	agent := &fakeReclaimStorage{
		sessions: []string{"aa:bb:cc:dd:ee:01"}, // 大小写与分隔符混用，必须归一化后匹配
		clones:   []string{"AABBCCDDEE01"},
	}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	if _, err := service.ReclaimOfflineClones(ctx, map[string]time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v, want none (client holds a live session)", agent.cleaned)
	}
}

func TestReclaimOfflineClonesSkipsCycleOnSessionError(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	seedOfflineTerminal(t, ctx, st, "term-1", "AABBCCDDEE01", "192.168.1.10", group.ID, now.Add(-24*time.Hour), false)

	// 会话列表读不出来是「未知」，不是「全部关机」。
	agent := &fakeReclaimStorage{sessionsErr: errors.New("configfs unavailable"), clones: []string{"AABBCCDDEE01"}}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	if _, err := service.ReclaimOfflineClones(ctx, map[string]time.Time{}); err == nil {
		t.Fatal("want the session error propagated so the cycle is skipped")
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v, want none", agent.cleaned)
	}
}

func TestReclaimOfflineClonesAgesOutOrphans(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 没有终端记录的克隆（终端已删或中断流程的残留）没有 OfflineAt，从首次看到起计时。
	agent := &fakeReclaimStorage{clones: []string{"AABBCCDDEE01"}}
	firstSeen := map[string]time.Time{}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v on first sighting, want none (grace period starts now)", agent.cleaned)
	}
	if _, ok := firstSeen["AABBCCDDEE01"]; !ok {
		t.Fatal("first sighting was not clocked")
	}

	later := now.Add(cloneGracePeriod)
	service.Now = func() time.Time { return later }
	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agent.cleaned, []string{"AABBCCDDEE01"}) {
		t.Fatalf("cleaned = %v, want [AABBCCDDEE01] after the grace period", agent.cleaned)
	}
}

func TestReclaimOfflineClonesCancelsOrphanClockOnReturn(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	agent := &fakeReclaimStorage{clones: []string{"AABBCCDDEE01"}}
	firstSeen := map[string]time.Time{}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}
	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}

	// 宽限期内客户机重新上线：待回收必须作废而非暂停，否则长期在线的客户机下次抖动就会被回收。
	agent.sessions = []string{"AABBCCDDEE01"}
	service.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	if _, ok := firstSeen["AABBCCDDEE01"]; ok {
		t.Fatal("clock survived the client coming back online")
	}

	agent.sessions = nil
	service.Now = func() time.Time { return now.Add(cloneGracePeriod + time.Minute) }
	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v, want none: the grace period restarts from the new sighting", agent.cleaned)
	}
}

func TestReclaimOfflineClonesCapsOneCycle(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 整间机房同时关机不能让回收本身变成风暴。
	clones := make([]string, 0, cloneReclaimBatch*2)
	firstSeen := map[string]time.Time{}
	for i := 0; i < cloneReclaimBatch*2; i++ {
		mac := fmt.Sprintf("AABBCCDD%04X", i)
		clones = append(clones, mac)
		firstSeen[mac] = now.Add(-cloneGracePeriod)
	}
	agent := &fakeReclaimStorage{clones: clones}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	reclaimed, err := service.ReclaimOfflineClones(ctx, firstSeen)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != cloneReclaimBatch || len(agent.cleaned) != cloneReclaimBatch {
		t.Fatalf("reclaimed = %d / cleaned = %d, want %d for both", reclaimed, len(agent.cleaned), cloneReclaimBatch)
	}

	// 剩余部分不丢，下一轮接着回收。
	agent.clones = clones[cloneReclaimBatch:]
	agent.cleaned = nil
	if _, err := service.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != cloneReclaimBatch {
		t.Fatalf("second cycle cleaned = %d, want %d", len(agent.cleaned), cloneReclaimBatch)
	}
}

func TestReclaimOfflineClonesToleratesPerClientFailure(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	agent := &fakeReclaimStorage{
		clones:  []string{"AABBCCDDEE01", "AABBCCDDEE02"},
		failFor: map[string]error{"AABBCCDDEE01": errors.New("dataset is busy")},
	}
	firstSeen := map[string]time.Time{
		"AABBCCDDEE01": now.Add(-cloneGracePeriod),
		"AABBCCDDEE02": now.Add(-cloneGracePeriod),
	}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	reclaimed, err := service.ReclaimOfflineClones(ctx, firstSeen)
	if err != nil {
		t.Fatalf("one client's failure aborted the cycle: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	if !reflect.DeepEqual(agent.cleaned, []string{"AABBCCDDEE01", "AABBCCDDEE02"}) {
		t.Fatalf("cleaned = %v, want both attempted", agent.cleaned)
	}
	// 失败的那台保留计时，下一轮立即重试而不是重新等宽限期。
	if _, ok := firstSeen["AABBCCDDEE01"]; !ok {
		t.Fatal("failed reclaim lost its clock and will wait out the grace period again")
	}
}

func TestReclaimOfflineClonesIgnoresTerminalsWithoutClones(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 离线已久但克隆已被回收：不必为没有克隆的客户机去调 zfs 和 configfs。
	seedOfflineTerminal(t, ctx, st, "term-1", "AABBCCDDEE01", "192.168.1.10", group.ID, now.Add(-24*time.Hour), false)

	agent := &fakeReclaimStorage{}
	service := TerminalService{Store: st, Storage: agent, Now: func() time.Time { return now }}

	if _, err := service.ReclaimOfflineClones(ctx, map[string]time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(agent.cleaned) != 0 {
		t.Fatalf("cleaned = %v, want none (no clone exists)", agent.cleaned)
	}
}

// 每个 host 的克隆由该 host 自己回收：会话按所有 host 合并统计，清理调用回到克隆所在节点。
func TestReclaimOfflineClonesRoutesPerHost(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()

	localAgent := &fakeReclaimStorage{clones: []string{"aa:aa:aa:aa:aa:01"}}
	remoteAgent := &fakeReclaimStorage{clones: []string{"aa:aa:aa:aa:aa:02"}}
	srv := httptest.NewServer(remote.Handler{Agent: remoteAgent, Token: "tok"})
	defer srv.Close()
	seen := now
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-b", Name: "b", IP: "b", PortalIP: "10.9.0.2",
		APIURL: srv.URL, Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &seen}); err != nil {
		t.Fatal(err)
	}

	svc := TerminalService{Store: st, Storage: localAgent, Now: func() time.Time { return now },
		Place: &place.Router{Store: st, NodeID: "node-a", Local: localAgent, LocalPortal: "vip", Token: "tok",
			Now: func() time.Time { return now }}}

	firstSeen := map[string]time.Time{}
	// 第 1 轮开始计时，第 2 轮（过宽限期）按 host 分别回收。
	if _, err := svc.ReclaimOfflineClones(ctx, firstSeen); err != nil {
		t.Fatal(err)
	}
	for k := range firstSeen {
		firstSeen[k] = now.Add(-10 * time.Minute)
	}
	n, err := svc.ReclaimOfflineClones(ctx, firstSeen)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !reflect.DeepEqual(localAgent.cleaned, []string{"AAAAAAAAAA01"}) {
		t.Fatalf("local cleaned: %v", localAgent.cleaned)
	}
	if !reflect.DeepEqual(remoteAgent.cleaned, []string{"AAAAAAAAAA02"}) {
		t.Fatalf("remote cleaned: %v", remoteAgent.cleaned)
	}
}
