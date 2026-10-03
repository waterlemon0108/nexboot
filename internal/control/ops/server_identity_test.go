package ops

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

// 节点行记录真实地址（客户机连的 portal、对端调的 API URL），不是把 id 重复三遍。
func TestEnsureServerWritesRealCoordinates(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)

	err := ensureServer(ctx, st, domain.Server{
		ID: "node-a", Name: "node-a", IP: "192.168.50.10",
		PortalIP: "192.168.50.1", APIURL: "http://192.168.50.10:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.Servers().Get(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.PortalIP != "192.168.50.1" || row.APIURL != "http://192.168.50.10:8080" || row.IP != "192.168.50.10" {
		t.Fatalf("row = %#v", row)
	}

	// 地址变化（改地址、VIP 漂移）时同一调用更新现有行，不报错也不重复。
	err = ensureServer(ctx, st, domain.Server{
		ID: "node-a", Name: "node-a", IP: "192.168.60.10",
		PortalIP: "192.168.60.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, _ = st.Servers().Get(ctx, "node-a")
	if row.PortalIP != "192.168.60.1" || row.IP != "192.168.60.10" {
		t.Fatalf("row after update = %#v", row)
	}
}

// 旧部署用 boot URL 的主机名作为服务器行的键。改用稳定的 machine id 时，挂在旧键
// 上的池和克隆记录要跟着迁移，否则会归属到不存在的服务器。
func TestEnsureServerAdoptsTheLegacyRow(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "192.168.50.1", Name: "192.168.50.1", IP: "192.168.50.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank", ServerID: "192.168.50.1", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	// 管理 IP 正是旧行的 id 和 ip；servers.ip 是 UNIQUE，改键不能直接先插入。
	err := ensureServer(ctx, st, domain.Server{
		ID: "node-a", Name: "node-a", IP: "192.168.50.1", PortalIP: "192.168.50.1",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := st.Servers().List(ctx)
	if err != nil || len(servers) != 1 || servers[0].ID != "node-a" {
		t.Fatalf("servers = %#v err=%v (legacy row must be re-keyed, not duplicated)", servers, err)
	}
	pool, err := st.Pools().Get(ctx, "pool-tank")
	if err != nil || pool.ServerID != "node-a" {
		t.Fatalf("pool = %#v err=%v (must follow the re-keyed server)", pool, err)
	}
}

// 心跳循环写 last_seen_at，用它区分在线节点和残留记录。
func TestTouchServerHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := ensureServer(ctx, st, domain.Server{ID: "node-a", Name: "node-a", IP: "x", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	if err := TouchServerHeartbeat(ctx, st, "node-a", now); err != nil {
		t.Fatal(err)
	}
	row, _ := st.Servers().Get(ctx, "node-a")
	if row.LastSeenAt == nil || !row.LastSeenAt.Equal(now) {
		t.Fatalf("last_seen_at = %v", row.LastSeenAt)
	}
}

// 重置 /etc/machine-id、重装系统或换主板会让同一台机器得到新 node id，旧行仍占着
// 本机地址，而 servers.ip 是 UNIQUE，注册会撞约束导致进程反复退出。复制来的目录
// 不止一行，改键不能只在单行表时触发。
func TestEnsureLocalServerRekeysAnyRowHoldingThisAddress(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)

	// 复制来的目录：对端的行加上本机的旧身份。
	peer := domain.Server{ID: "peer-node", Name: "peer", IP: "192.168.10.3", PortalIP: "192.168.10.250",
		APIURL: "http://192.168.10.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := st.Servers().Create(ctx, peer); err != nil {
		t.Fatal(err)
	}
	old := domain.Server{ID: "old-machine-id", Name: "me", IP: "192.168.10.4", PortalIP: "192.168.10.250",
		APIURL: "http://192.168.10.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := st.Servers().Create(ctx, old); err != nil {
		t.Fatal(err)
	}

	// 同一台机器，新身份，地址不变。
	fresh := domain.Server{ID: "new-machine-id", Name: "new-machine-id", IP: "192.168.10.4",
		PortalIP: "192.168.10.250", APIURL: "http://192.168.10.4:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := EnsureLocalServer(ctx, st, fresh); err != nil {
		t.Fatalf("a machine that changed its id must still start: %v", err)
	}

	if _, err := st.Servers().Get(ctx, "new-machine-id"); err != nil {
		t.Fatalf("new identity not registered: %v", err)
	}
	if _, err := st.Servers().Get(ctx, "old-machine-id"); err == nil {
		t.Fatal("the superseded row should be gone")
	}
	// 对端的行不受影响。
	if got, err := st.Servers().Get(ctx, "peer-node"); err != nil || got.IP != "192.168.10.3" {
		t.Fatalf("peer row touched: %+v err=%v", got, err)
	}
}
