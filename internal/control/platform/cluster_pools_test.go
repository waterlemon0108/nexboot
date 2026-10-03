package platform

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/domain"
)

// 池记录随目录复制到每台，但容量、健康只有池所在节点读得出来，必须逐台去问，否则别的节点的池永远是 unknown。
func TestClusterPoolsAsksEachNodeForItsOwn(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-10 * time.Minute)

	for _, s := range []domain.Server{
		{ID: "self", Name: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "active", LastSeenAt: &fresh},
		{ID: "peer", Name: "peer", IP: "10.0.0.4", APIURL: "http://10.0.0.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "standby", LastSeenAt: &fresh},
		{ID: "gone", Name: "gone", IP: "10.0.0.9", APIURL: "http://10.0.0.9:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, HAState: "standby", LastSeenAt: &stale},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	cp := ClusterPools{
		Store: st, NodeID: "self", Now: func() time.Time { return now },
		Local: func(context.Context) ([]ops.PoolItem, error) {
			return []ops.PoolItem{{Name: "tank", Health: "ONLINE", Capacity: 80 << 30, Used: 18 << 30}}, nil
		},
		// 远端节点由注入的取数函数代替真实 HTTP
		Fetch: func(_ context.Context, apiURL string) ([]ops.PoolItem, error) {
			if apiURL == "http://10.0.0.4:8080" {
				return []ops.PoolItem{{Name: "tank", Health: "ONLINE", Capacity: 80 << 30, Used: 20 << 30}}, nil
			}
			return nil, context.DeadlineExceeded
		},
	}

	res, err := cp.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("nodes=%d", len(res.Nodes))
	}
	byIP := map[string]ClusterPoolView{}
	for _, n := range res.Nodes {
		byIP[n.IP] = n
	}
	if got := byIP["10.0.0.3"]; len(got.Pools) != 1 || got.Pools[0].Used != 18<<30 {
		t.Fatalf("本机的池没读到实时值: %+v", got)
	}
	if got := byIP["10.0.0.4"]; len(got.Pools) != 1 || got.Pools[0].Used != 20<<30 {
		t.Fatalf("对端的池没有跨节点读到: %+v", got)
	}
	// 失联节点要明说读不到，不能显示成 0 或 unknown 让人以为池坏了
	if got := byIP["10.0.0.9"]; got.Reachable || got.Error == "" {
		t.Fatalf("失联节点应带出原因: %+v", got)
	}
	// 集群合计只统计读得到的
	if res.TotalCapacity != 160<<30 || res.TotalUsed != 38<<30 {
		t.Fatalf("合计不对: cap=%d used=%d", res.TotalCapacity, res.TotalUsed)
	}
}
