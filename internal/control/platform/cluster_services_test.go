package platform

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func newClusterTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+t.TempDir()+"/c.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 节点表是心跳复制的副本，备机上那份会滞后，角色轮换后可能仍把退位节点记成主机。
// 角色以节点自报为准，否则会画出两个主机。
func TestListPrefersTheRoleEachNodeReportsAboutItself(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)

	// 节点表里这台还记着 active（已陈旧）
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "self", Name: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "active", LastSeenAt: &fresh,
	}); err != nil {
		t.Fatal(err)
	}

	cs := ClusterServices{
		Store: st, NodeID: "self", Now: func() time.Time { return now },
		// 而这台机器自己知道已经退位
		SelfRole: func() string { return "standby" },
		Local: func(context.Context) (ServiceListResult, error) {
			return ServiceListResult{Items: []ServiceItem{{Key: "dnsmasq", OK: true, Provided: false}}}, nil
		},
	}
	res, err := cs.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("nodes=%d", len(res.Nodes))
	}
	if res.Nodes[0].Role != "standby" {
		t.Fatalf("role=%q，花名册的陈旧值压过了本机的第一手答案", res.Nodes[0].Role)
	}
}

// 版本、主机名、运行时长每台一份，每个节点随服务清单报自己的主机信息；
// 只报持 VIP 的那台会看不出滚动升级时的版本不一致。
func TestListCarriesEachNodesOwnHostFacts(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "self", Name: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "active", LastSeenAt: &fresh,
	}); err != nil {
		t.Fatal(err)
	}

	cs := ClusterServices{
		Store: st, NodeID: "self", Now: func() time.Time { return now },
		Local: func(context.Context) (ServiceListResult, error) {
			return ServiceListResult{
				Items: []ServiceItem{{Key: "dnsmasq", OK: true}},
				Host:  &HostFacts{Hostname: "nimblex1", OS: "Ubuntu 24.04.1 LTS", UptimeSec: 111600, Version: "dev"},
			}, nil
		},
	}
	res, err := cs.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Nodes[0].Host
	if got == nil {
		t.Fatal("节点没带上自己的主机信息")
	}
	if got.Hostname != "nimblex1" || got.Version != "dev" || got.UptimeSec != 111600 {
		t.Fatalf("host=%+v", got)
	}
}

// 节点表已标记离线的节点不去请求，否则每次刷新都要多等一个超时。
func TestListSkipsNodesTheRosterAlreadyKnowsAreOffline(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-10 * time.Minute) // 远超新鲜度窗口

	if err := st.Servers().Create(ctx, domain.Server{
		ID: "self", Name: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "active", LastSeenAt: &fresh,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "dead", Name: "dead", IP: "10.0.0.5", APIURL: "http://10.0.0.5:8080",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "standby", LastSeenAt: &stale,
	}); err != nil {
		t.Fatal(err)
	}

	dialed := make(chan string, 4)
	cs := ClusterServices{
		Store: st, NodeID: "self", Now: func() time.Time { return now },
		Local: func(context.Context) (ServiceListResult, error) {
			return ServiceListResult{Items: []ServiceItem{{Key: "dnsmasq", OK: true}}}, nil
		},
		Fetch: func(_ context.Context, apiURL string) (ServiceListResult, error) {
			dialed <- apiURL
			return ServiceListResult{}, context.DeadlineExceeded
		},
	}
	res, err := cs.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	close(dialed)
	for url := range dialed {
		t.Fatalf("拨了已知离线节点的号：%s", url)
	}
	var dead ClusterServiceView
	for _, n := range res.Nodes {
		if n.NodeID == "dead" {
			dead = n
		}
	}
	if dead.Reachable || dead.Error == "" {
		t.Fatalf("离线节点应当带着说明出现在结果里：%+v", dead)
	}
}
