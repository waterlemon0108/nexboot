package place

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

type nullAgent struct{ storage.ClientHostAgent }

func newStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedServer(t *testing.T, st *store.SQLStore, id string, seen time.Time) {
	t.Helper()
	s := domain.Server{ID: id, Name: id, IP: id + ".ip", PortalIP: "10.9.0." + id[len(id)-1:], APIURL: "http://" + id + ":8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if !seen.IsZero() {
		s.LastSeenAt = &seen
	}
	if err := st.Servers().Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func newRouter(st *store.SQLStore, now time.Time) Router {
	return Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.250",
		Token: "tok", Now: func() time.Time { return now }}
}

func TestForGroupPlacement(t *testing.T) {
	st := newStore(t)
	now := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-b", now.Add(-10*time.Second)) // fresh
	seedServer(t, st, "node-c", now.Add(-5*time.Minute))  // stale
	r := newRouter(st, now)
	bID, cID, dID := "node-b", "node-c", "node-d"

	t.Run("unpinned group stays local", func(t *testing.T) {
		p := r.ForGroup(context.Background(), domain.Group{ID: "g"})
		if p.Remote || p.ServerID != "node-a" || p.Portal != "10.9.0.250" {
			t.Fatalf("p=%+v", p)
		}
	})
	t.Run("pinned to self stays local", func(t *testing.T) {
		self := "node-a"
		p := r.ForGroup(context.Background(), domain.Group{ID: "g", StorageServerID: &self})
		if p.Remote {
			t.Fatalf("p=%+v", p)
		}
	})
	t.Run("pinned to fresh node goes remote with its portal", func(t *testing.T) {
		p := r.ForGroup(context.Background(), domain.Group{ID: "g", StorageServerID: &bID})
		if !p.Remote || p.ServerID != "node-b" || p.Portal != "10.9.0.b" || !p.Healthy {
			t.Fatalf("p=%+v", p)
		}
	})
	t.Run("stale node falls back local, flagged degraded", func(t *testing.T) {
		p := r.ForGroup(context.Background(), domain.Group{ID: "g", StorageServerID: &cID})
		if p.Remote || p.ServerID != "node-a" || p.Healthy {
			t.Fatalf("p=%+v", p)
		}
	})
	t.Run("unknown node falls back local", func(t *testing.T) {
		p := r.ForGroup(context.Background(), domain.Group{ID: "g", StorageServerID: &dID})
		if p.Remote || p.Healthy {
			t.Fatalf("p=%+v", p)
		}
	})
}

// 清理跟随客户机记录的实际落点而非分组当前指定；节点失联也不回落，
// 克隆不可能在别的节点上清理。
func TestForTerminalFollowsRecord(t *testing.T) {
	st := newStore(t)
	now := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-b", now.Add(-5*time.Minute)) // 已失联但仍是归属节点
	r := newRouter(st, now)
	bID := "node-b"

	p := r.ForTerminal(context.Background(), domain.Terminal{ID: "t", StorageServerID: &bID})
	if !p.Remote || p.ServerID != "node-b" || p.Healthy {
		t.Fatalf("p=%+v", p)
	}
	if q := r.ForTerminal(context.Background(), domain.Terminal{ID: "t"}); q.Remote {
		t.Fatalf("q=%+v", q)
	}
}

// 巡检遍历本机和心跳新鲜的远端节点，跳过失联节点（问不到它的会话）。
func TestAllHosts(t *testing.T) {
	st := newStore(t)
	now := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now) // 本机行也在列表里，不能重复
	seedServer(t, st, "node-b", now.Add(-10*time.Second))
	seedServer(t, st, "node-c", now.Add(-5*time.Minute))
	r := newRouter(st, now)

	hosts := r.AllHosts(context.Background())
	if len(hosts) != 2 || hosts[0].ServerID != "node-a" || hosts[1].ServerID != "node-b" {
		ids := []string{}
		for _, h := range hosts {
			ids = append(ids, h.ServerID)
		}
		t.Fatalf("hosts=%v", ids)
	}
}

// 未绑定的分组默认自动分散到各节点，不能全压在主机上。
// 均衡只在开机时发生一次（iSCSI 地址写死在启动脚本里，中途迁不走）。
func TestPlaceSpreadsClientsAcrossNodes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	seedServer(t, st, "node-c", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}

	// 未绑定的分组：连续放置应当摊开，而不是全压本机
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		p := r.ForClient(ctx, domain.Group{ID: "g"}, domain.Terminal{ID: fmt.Sprintf("t%d", i)})
		seen[p.ServerID]++
	}
	if len(seen) < 3 {
		t.Fatalf("只用到了 %d 台，没有摊开: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n > 4 {
			t.Fatalf("%s 分到 %d 台，明显偏斜: %v", id, n, seen)
		}
	}
}

// 运维显式绑定的分组不受自动均衡影响。
func TestPinBeatsAutoBalance(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}
	pin := "node-b"
	for i := 0; i < 5; i++ {
		p := r.ForClient(ctx, domain.Group{ID: "g", StorageServerID: &pin}, domain.Terminal{ID: fmt.Sprintf("t%d", i)})
		if p.ServerID != "node-b" {
			t.Fatalf("绑定被忽略，落到了 %s", p.ServerID)
		}
	}
}

// 上次落在哪台，就回哪台：克隆还在、ARC 是热的，重启最快。
func TestPlaceReturnsToLastNode(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	seedServer(t, st, "node-c", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}
	last := "node-c"
	for i := 0; i < 3; i++ {
		p := r.ForClient(ctx, domain.Group{ID: "g"}, domain.Terminal{ID: "t1", StorageServerID: &last})
		if p.ServerID != "node-c" {
			t.Fatalf("没有回到上次的节点，落到了 %s", p.ServerID)
		}
	}
}

// 超管机必须回持久盘所在节点，不参与均衡。
func TestSuperClientStaysOnItsNode(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}
	home := "node-b"
	pinElsewhere := "node-a"
	p := r.ForClient(ctx, domain.Group{ID: "g", StorageServerID: &pinElsewhere},
		domain.Terminal{ID: "s1", IsSuper: true, StorageServerID: &home})
	if p.ServerID != "node-b" {
		t.Fatalf("超管机被挪走了：%s（它的持久盘在 node-b）", p.ServerID)
	}
}

// 失联节点不参与均衡，不能把客户机分给它。
func TestAutoBalanceSkipsDarkNodes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now.Add(-10*time.Minute)) // 失联
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}
	for i := 0; i < 4; i++ {
		p := r.ForClient(ctx, domain.Group{ID: "g"}, domain.Terminal{ID: fmt.Sprintf("t%d", i)})
		if p.ServerID == "node-b" {
			t.Fatal("把客户机放到了失联的节点上")
		}
	}
}

// 决策必须无锁（此路径上的全局锁曾把 7.6 秒拖成 90 秒）；
// 并发放置时不 panic、不串行等待、结果仍分散。
func TestConcurrentPlacementStaysSpread(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	seedServer(t, st, "node-c", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	for i := 0; i < 90; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := r.ForClient(ctx, domain.Group{ID: "g"}, domain.Terminal{ID: fmt.Sprintf("t%d", i)})
			mu.Lock()
			seen[p.ServerID]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(seen) < 3 {
		t.Fatalf("并发下没有摊开: %v", seen)
	}
	for id, n := range seen {
		if n > 60 { // 90 台三节点，允许偏差但不能全挤一台
			t.Fatalf("%s 拿到 %d 台，羊群效应: %v", id, n, seen)
		}
	}
}

// 超管机必须落在写入者上：<池>/nd/ 由写入者 recv -F 单向复制，
// 在备机上保存会被下一轮复制回滚（原因详见 Router.ForClient）。
func TestSuperMachineGoesToTheWriter(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	// node-b 是备机，node-a 才是写入者
	writer, _ := st.Servers().Get(ctx, "node-a")
	writer.HAState = "active"
	if err := st.Servers().Update(ctx, writer); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}

	// 之前的盘在备机上也要挪回写入者
	onStandby := "node-b"
	p := r.ForClient(ctx, domain.Group{ID: "g"},
		domain.Terminal{ID: "s1", IsSuper: true, StorageServerID: &onStandby})
	if p.ServerID != "node-a" {
		t.Fatalf("超管机落在了 %s，而写入者是 node-a：在别处保存会被复制流回滚", p.ServerID)
	}
}

// 无节点自称写入者（单机或节点表未收敛）时退回原锚定节点，保证单机能用超管。
func TestSuperMachineKeepsItsDiskWhenNoWriterIsKnown(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	seedServer(t, st, "node-a", now)
	seedServer(t, st, "node-b", now)
	r := &Router{Store: st, NodeID: "node-a", Local: nullAgent{}, LocalPortal: "10.9.0.1",
		Token: "tok", Now: func() time.Time { return now }}
	home := "node-b"
	p := r.ForClient(ctx, domain.Group{ID: "g"},
		domain.Terminal{ID: "s1", IsSuper: true, StorageServerID: &home})
	if p.ServerID != "node-b" {
		t.Fatalf("没有已知写入者时不该挪动超管机：%s", p.ServerID)
	}
}
