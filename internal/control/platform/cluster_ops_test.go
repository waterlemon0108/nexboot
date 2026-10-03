package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

type opCall struct {
	apiURL, node, key, action string
	lines                     int
}

// 节点表里的地址可能已换了主人，重启错机器不会报错只会让另一批客户机掉盘，
// 所以目标节点必须核对身份，不是被点名的那台就拒绝执行。
func TestClusterServiceOpsRoutesToTheNamedNode(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	fresh := now.Add(-3 * time.Second)
	for _, s := range []domain.Server{
		{ID: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &fresh},
		{ID: "peer", IP: "10.0.0.4", APIURL: "http://10.0.0.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &fresh},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	var remote []opCall
	var local []opCall
	ops := ClusterServiceOps{
		Store: st, NodeID: "self", Now: func() time.Time { return now },
		LocalAction: func(_ context.Context, key, action string) error {
			local = append(local, opCall{key: key, action: action})
			return nil
		},
		LocalLogs: func(_ context.Context, key string, lines int) (string, error) {
			local = append(local, opCall{key: key, lines: lines})
			return "本机日志", nil
		},
		Post: func(_ context.Context, apiURL, node, key, action string) error {
			remote = append(remote, opCall{apiURL: apiURL, node: node, key: key, action: action})
			return nil
		},
		Fetch: func(_ context.Context, apiURL, node, key string, lines int) (string, error) {
			remote = append(remote, opCall{apiURL: apiURL, node: node, key: key, lines: lines})
			return "对端日志", nil
		},
	}

	// 点名本机：不走网络
	if err := ops.Action(ctx, "self", "dnsmasq", "restart"); err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0].action != "restart" || len(remote) != 0 {
		t.Fatalf("本机操作不该走网络: local=%+v remote=%+v", local, remote)
	}

	// 点名对端：必须带上目标身份，让对端有机会拒绝
	if err := ops.Action(ctx, "peer", "target", "restart"); err != nil {
		t.Fatal(err)
	}
	if len(remote) != 1 || remote[0].apiURL != "http://10.0.0.4:8080" || remote[0].node != "peer" {
		t.Fatalf("没按名字路由或没带目标节点: %+v", remote)
	}

	logs, err := ops.Logs(ctx, "peer", "target", 200)
	if err != nil || logs != "对端日志" {
		t.Fatalf("跨节点日志: %q %v", logs, err)
	}

	// 节点表里没有的节点，什么都不做
	if err := ops.Action(ctx, "ghost", "dnsmasq", "restart"); err == nil {
		t.Fatal("未知节点应当拒绝，而不是猜一台执行")
	}
}

// 对端一侧：被点名的不是自己就拒绝。
func TestNodeSelfCheckRefusesAnotherNodesRequest(t *testing.T) {
	if err := AssertSelf("nodeA", "nodeB"); err == nil {
		t.Fatal("被点名的不是自己，必须拒绝")
	} else if !errors.Is(err, ErrWrongNode) {
		t.Fatalf("要能被上层认出来: %v", err)
	}
	if err := AssertSelf("nodeA", "nodeA"); err != nil {
		t.Fatal(err)
	}
	// 未点名不检查：老客户端与本机直接调用照旧
	if err := AssertSelf("nodeA", ""); err != nil {
		t.Fatal(err)
	}
}
