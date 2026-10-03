package platform

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/domain"
)

// 池操作任务记在池所在节点的库里，任务列表必须把各节点的池任务一并列出，否则在别的节点建池看不到成败。
func TestClusterTasksListsWhatOtherNodesRan(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	for _, s := range []domain.Server{
		{ID: "self", Name: "self", IP: "10.0.0.3", APIURL: "http://10.0.0.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "peer", Name: "peer", IP: "10.0.0.4", APIURL: "http://10.0.0.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "gone", Name: "gone", IP: "10.0.0.9", APIURL: "http://10.0.0.9:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	at := func(m int) time.Time { return time.Date(2026, 9, 23, 9, m, 0, 0, time.UTC) }
	for _, task := range []domain.Task{
		{ID: "t-local", Type: domain.TaskTypeCreateConfig, Status: domain.TaskStatusSuccess, CreatedAt: at(0)},
		{ID: "t-shared", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusSuccess, CreatedAt: at(1)},
	} {
		if err := st.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	ct := ClusterTasks{
		Store: st, NodeID: "self", Local: assets.ImageService{Store: st},
		Fetch: func(_ context.Context, apiURL string) ([]domain.Task, error) {
			if apiURL != "http://10.0.0.4:8080" {
				return nil, context.DeadlineExceeded
			}
			return []domain.Task{
				{ID: "t-peer-fail", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusFailed, Error: "mountpoint '/data' exists", CreatedAt: at(3)},
				{ID: "t-peer-run", Type: domain.TaskTypeAddDisk, Status: domain.TaskStatusRunning, CreatedAt: at(2)},
				{ID: "t-shared", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusSuccess, CreatedAt: at(1)},
			}, nil
		},
	}

	res, err := ct.ListTasks(ctx, assets.TaskListQuery{Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, it := range res.Items {
		ids = append(ids, it.ID)
	}
	if want := "[t-peer-fail t-peer-run t-shared t-local]"; fmtIDs(ids) != want || res.Total != 4 {
		t.Fatalf("items = %v total = %d，应为 %s 共 4 条（去重、按时间倒序）", ids, res.Total, want)
	}
	if res.Items[0].Node != "10.0.0.4" || res.Items[0].Error == "" || res.Items[2].Node != "" {
		t.Fatalf("别台执行的要标出节点并带着失败原因，本机的不标: %+v", res.Items)
	}
	if len(res.Unreachable) != 1 || res.Unreachable[0] != "10.0.0.9" {
		t.Fatalf("读不到的节点要说出来: %v", res.Unreachable)
	}

	page2, _ := ct.ListTasks(ctx, assets.TaskListQuery{Page: 2, Size: 2})
	if len(page2.Items) != 2 || page2.Items[0].ID != "t-shared" || page2.Items[1].ID != "t-local" {
		t.Fatalf("第 2 页 = %+v", page2.Items)
	}
	failed, _ := ct.ListTasks(ctx, assets.TaskListQuery{Status: "failed", Page: 1, Size: 10})
	if len(failed.Items) != 1 || failed.Items[0].ID != "t-peer-fail" {
		t.Fatalf("按状态筛选也要算上别台的: %+v", failed.Items)
	}

	active, err := ct.ListActiveTasks(ctx)
	if err != nil || len(active) != 1 || active[0].ID != "t-peer-run" || active[0].Node != "10.0.0.4" {
		t.Fatalf("进行中 = %+v err = %v", active, err)
	}
	got, err := ct.GetTask(ctx, "t-peer-fail")
	if err != nil || got.Node != "10.0.0.4" {
		t.Fatalf("按 ID 也要查得到别台的任务: %+v err = %v", got, err)
	}
}

func fmtIDs(ids []string) string {
	out := "["
	for i, id := range ids {
		if i > 0 {
			out += " "
		}
		out += id
	}
	return out + "]"
}

// 节点只把自己执行的池操作任务交出去；其余任务写入者的库里本来就有。
func TestNodePoolTasksHandsOverOnlyPoolWork(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	for i, task := range []domain.Task{
		{ID: "t-cfg", Type: domain.TaskTypeCreateConfig, Status: domain.TaskStatusSuccess},
		{ID: "t-pool", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusFailed},
		{ID: "t-disk", Type: domain.TaskTypeAddDisk, Status: domain.TaskStatusRunning},
	} {
		task.CreatedAt = time.Date(2026, 9, 23, 9, i, 0, 0, time.UTC)
		if err := st.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NodePoolTasks(ctx, st)
	if err != nil || len(got) != 2 || got[0].ID != "t-disk" || got[1].ID != "t-pool" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// 任务目标是数据集 ID，中文名折成 ASCII 后认不出，列表要带可读名称。
func TestTasksCarryReadableTargetNames(t *testing.T) {
	ctx := context.Background()
	st := newClusterTestStore(t)
	now := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	cur := "win11-_default_r1"
	for _, err := range []error{
		st.Images().Create(ctx, domain.Image{ID: "win11-", Name: "Win11 电竞版", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}),
		st.Configs().Create(ctx, domain.Config{ID: "win11-_default", ImageID: "win11-", Name: "default", DefaultReductionID: &cur, CreatedAt: now}),
		st.Reductions().Create(ctx, domain.Reduction{ID: cur, ConfigID: "win11-_default", Name: "@r1", DisplayName: "装完显卡驱动", Status: domain.ReductionStatusReady, CreatedAt: now}),
		st.Groups().Create(ctx, domain.Group{ID: "g1", Name: "主播专区", StartIP: "10.0.0.10", ClientMax: 5, Netmask: "255.255.255.0", SystemImageID: "win11-", SystemConfigID: "win11-_default", SystemReductionID: cur}),
		st.Terminals().Create(ctx, domain.Terminal{ID: "terminal-0050562EF65E", MAC: "0050562EF65E", Name: "主播-01", IP: "10.0.0.11", GroupID: "g1", State: domain.TerminalStateOffline}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []domain.Task{
		{ID: "t-img", Type: domain.TaskTypeImportImage, TargetRef: "win11-", Status: domain.TaskStatusSuccess, CreatedAt: now},
		{ID: "t-cfg", Type: domain.TaskTypeCreateReduction, TargetRef: "win11-_default", Status: domain.TaskStatusSuccess, CreatedAt: now.Add(time.Second)},
		{ID: "t-red", Type: domain.TaskTypeDeleteReduction, TargetRef: cur, Status: domain.TaskStatusSuccess, CreatedAt: now.Add(2 * time.Second)},
		{ID: "t-gone", Type: domain.TaskTypeDeleteConfig, TargetRef: "gone_default", Status: domain.TaskStatusSuccess, CreatedAt: now.Add(3 * time.Second)},
		// 超管停机、数据盘发布的目标是 MAC，客户机 ID 是 terminal-<MAC>。
		{ID: "t-super", Type: domain.TaskTypeSuperStop, TargetRef: "0050562EF65E", Status: domain.TaskStatusSuccess, CreatedAt: now.Add(4 * time.Second)},
	} {
		if err := st.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	ct := ClusterTasks{Store: st, NodeID: "self", Local: assets.ImageService{Store: st},
		Fetch: func(context.Context, string) ([]domain.Task, error) { return nil, nil }}
	res, err := ct.ListTasks(ctx, assets.TaskListQuery{Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range res.Items {
		got[it.ID] = it.TargetName
	}
	want := map[string]string{"t-img": "Win11 电竞版", "t-cfg": "Win11 电竞版 / default",
		"t-red": "Win11 电竞版 / default / 装完显卡驱动", "t-gone": "", "t-super": "主播-01"}
	for id, name := range want {
		if got[id] != name {
			t.Fatalf("%s 的名称 = %q，应为 %q（全部：%v）", id, got[id], name, got)
		}
	}
}
