package ops

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
)

func reportedPool(node, name string, disks ...string) ha.ReportedPool {
	var statuses []domain.PoolDiskStatus
	for _, d := range disks {
		statuses = append(statuses, domain.PoolDiskStatus{Path: d, Role: domain.PoolDiskRoleData, Status: "ONLINE"})
	}
	return ha.ReportedPool{
		Pool:  domain.Pool{ID: storage.PoolID(node, name), ServerID: node, Name: name, Disks: disks, Layout: domain.PoolLayoutStripe, GroupWidth: 1, Capacity: 60 << 30},
		Disks: statuses,
	}
}

func poolNamesOf(t *testing.T, reg ClusterRegistry, node string) []string {
	t.Helper()
	pools, err := reg.Store.Pools().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pools {
		if p.ServerID == node {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names
}

// 备机上建/删的池只写进备机自己的库，会被写入者的副本覆盖。写入者按节点心跳里
// 上报的池对齐记录：补上新池、删掉已销毁的，保留节点仍认得的缺失池，不动别的节点。
func TestHeartbeatBringsPoolRecordsInLineWithTheNode(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return now }}
	node := ha.NodeInfo{NodeID: "node-5", APIURL: "http://10.0.0.5:8080", PortalIP: "10.0.0.5", Role: "standby"}
	if err := reg.Register(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, ha.NodeInfo{NodeID: "node-3", APIURL: "http://10.0.0.3:8080", PortalIP: "10.0.0.3", Role: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.Pool{
		{ID: storage.PoolID("node-5", "data"), ServerID: "node-5", Name: "data", Disks: []string{"/dev/sdb1"}},
		{ID: storage.PoolID("node-5", "old"), ServerID: "node-5", Name: "old", Disks: []string{"/dev/sdc1"}},       // 在备机上删掉了
		{ID: storage.PoolID("node-5", "broken"), ServerID: "node-5", Name: "broken", Disks: []string{"/dev/sdd1"}}, // 没导入，节点自己还记着
		{ID: storage.PoolID("node-3", "data"), ServerID: "node-3", Name: "data", Disks: []string{"/dev/sdb1"}},     // 别的节点
	} {
		if err := st.Pools().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	node.Pools = &ha.PoolReport{
		Present: []ha.ReportedPool{reportedPool("node-5", "data", "/dev/sdb1"), reportedPool("node-5", "backup", "/dev/sdc1")},
		Known:   []string{"data", "broken"},
	}
	if err := reg.Register(ctx, node); err != nil {
		t.Fatal(err)
	}
	got := poolNamesOf(t, reg, "node-5")
	want := []string{"backup", "broken", "data"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("node-5 的池记录 = %v，want %v（补上 backup；删掉 old；broken 节点自己还记着，留给运维看）", got, want)
	}
	if other := poolNamesOf(t, reg, "node-3"); len(other) != 1 {
		t.Fatalf("别的节点的记录不该动: %v", other)
	}
	disks, _ := st.PoolDisks().List(ctx)
	found := false
	for _, d := range disks {
		if d.PoolID == storage.PoolID("node-5", "backup") && d.Path == "/dev/sdc1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("补上的池要带上它的盘: %+v", disks)
	}
}

// 老版本节点不报池：什么都不动。
func TestHeartbeatWithoutAPoolReportLeavesRecordsAlone(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) }}
	node := ha.NodeInfo{NodeID: "node-5", APIURL: "http://10.0.0.5:8080", PortalIP: "10.0.0.5", Role: "standby"}
	_ = reg.Register(ctx, node)
	_ = st.Pools().Create(ctx, domain.Pool{ID: storage.PoolID("node-5", "old"), ServerID: "node-5", Name: "old"})
	if err := reg.Register(ctx, node); err != nil {
		t.Fatal(err)
	}
	if got := poolNamesOf(t, reg, "node-5"); len(got) != 1 {
		t.Fatalf("没有上报就不该动记录: %v", got)
	}
}

type reportAdmin struct {
	storage.PoolAdmin
	statuses map[string]domain.PoolStatus
}

func (a reportAdmin) ServerID() string { return "node-5" }
func (a reportAdmin) PoolStatus(_ context.Context, name string) (domain.PoolStatus, error) {
	st, ok := a.statuses[name]
	if !ok {
		return domain.PoolStatus{}, errors.New("no such pool")
	}
	return st, nil
}

// 节点侧上报本机已导入的产品池（含盘和布局），以及本机库里记着的自己的池。
func TestPoolReportSaysWhatThisNodeHasAndKnows(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	reg := ClusterRegistry{Store: st, Now: func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) }}
	for _, id := range []string{"node-5", "node-3"} {
		_ = reg.Register(ctx, ha.NodeInfo{NodeID: id, APIURL: "http://" + id + ":8080", PortalIP: id})
	}
	_ = st.Pools().Create(ctx, domain.Pool{ID: storage.PoolID("node-5", "data"), ServerID: "node-5", Name: "data"})
	_ = st.Pools().Create(ctx, domain.Pool{ID: storage.PoolID("node-5", "old"), ServerID: "node-5", Name: "old"})
	_ = st.Pools().Create(ctx, domain.Pool{ID: storage.PoolID("node-3", "data"), ServerID: "node-3", Name: "data"})
	svc := PoolService{Store: st, Storage: reportAdmin{statuses: map[string]domain.PoolStatus{
		"data":   {Name: "data", Capacity: 60 << 30, Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []domain.PoolDiskStatus{{Path: "/dev/sdb1", Role: domain.PoolDiskRoleData}}},
		"backup": {Name: "backup", Capacity: 60 << 30, Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []domain.PoolDiskStatus{{Path: "/dev/sdc1", Role: domain.PoolDiskRoleData}}},
	}}}

	report, err := svc.PoolReport(ctx, []string{"data", "backup", "vanished"})
	if err != nil {
		t.Fatal(err)
	}
	var present []string
	for _, p := range report.Present {
		present = append(present, p.Pool.Name)
		if p.Pool.Name == "backup" && (len(p.Disks) != 1 || p.Disks[0].Path != "/dev/sdc1" || p.Pool.Capacity == 0) {
			t.Fatalf("上报要带上盘和容量: %+v", p)
		}
	}
	sort.Strings(present)
	known := append([]string{}, report.Known...)
	sort.Strings(known)
	if strings.Join(present, ",") != "backup,data" || strings.Join(known, ",") != "data,old" {
		t.Fatalf("present=%v known=%v（读不出状态的 vanished 不算导入着；别的节点的记录不算本机记着的）", present, known)
	}
}
