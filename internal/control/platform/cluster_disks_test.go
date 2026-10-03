package platform

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// 在别的节点建池必须列那台自己的空闲盘，不能拿本机的盘去别的节点建池。
func TestClusterDisksAsksTheNamedNode(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * time.Second)
	for _, s := range []domain.Server{
		{ID: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &fresh},
		{ID: "peer", IP: "10.0.0.4", APIURL: "http://10.0.0.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &fresh},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	cd := ClusterDisks{
		Store: st, NodeID: "self",
		Local: func(context.Context) (ops.DiskListResult, error) {
			return ops.DiskListResult{Items: []storage.DiskInfo{{Path: "/dev/sdb", Name: "sdb"}}}, nil
		},
		Fetch: func(_ context.Context, apiURL string) (ops.DiskListResult, error) {
			if apiURL != "http://10.0.0.4:8080" {
				t.Fatalf("问错了机器: %s", apiURL)
			}
			return ops.DiskListResult{Items: []storage.DiskInfo{{Path: "/dev/sdx", Name: "sdx"}}}, nil
		},
	}

	local, err := cd.List(ctx, "self")
	if err != nil || len(local.Items) != 1 || local.Items[0].Path != "/dev/sdb" {
		t.Fatalf("本机盘: %+v %v", local, err)
	}
	remote, err := cd.List(ctx, "peer")
	if err != nil || len(remote.Items) != 1 || remote.Items[0].Path != "/dev/sdx" {
		t.Fatalf("对端盘: %+v %v", remote, err)
	}
	if _, err := cd.List(ctx, "ghost"); err == nil {
		t.Fatal("未知节点应当拒绝，而不是回本机的盘——那会让人拿本机盘去别处建池")
	}
}
