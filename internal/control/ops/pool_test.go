package ops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestPoolServiceCreateStatusDestroy(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	storage := &fakePoolStorage{
		pool:   domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2"},
		status: domain.PoolStatus{Name: "tank2", Health: "ONLINE", Capacity: 1000, Used: 250},
	}
	service := PoolService{Store: st, Storage: storage, Now: func() time.Time { return now }}

	result, err := service.Create(ctx, PoolRequest{Name: "tank2", Disks: []string{"/dev/sdb", " /dev/sdc "}})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || result.Pool.ID != "pool-tank2" || result.Pool.Health != "ONLINE" {
		t.Fatalf("result = %#v", result)
	}
	if storage.createdName != "tank2" || !reflect.DeepEqual(storage.createdDisks, []string{"/dev/sdb", "/dev/sdc"}) {
		t.Fatalf("storage create = %q %#v", storage.createdName, storage.createdDisks)
	}
	pool, err := st.Pools().Get(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Capacity != 1000 || pool.Used != 250 {
		t.Fatalf("pool = %#v", pool)
	}
	if _, err := st.Servers().Get(ctx, "server-a"); err != nil {
		t.Fatalf("server missing: %v", err)
	}
	disks, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 || disks[0].Path != "/dev/sdb" || disks[1].Path != "/dev/sdc" {
		t.Fatalf("pool disks = %#v", disks)
	}
	createTask, err := st.Tasks().Get(ctx, result.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if createTask.Type != domain.TaskTypeCreatePool || createTask.Status != domain.TaskStatusSuccess || createTask.Progress != 100 {
		t.Fatalf("create task = %#v", createTask)
	}

	list, err := service.List(ctx, "server-a")
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Items[0].Health != "ONLINE" || list.Items[0].Capacity != 1000 {
		t.Fatalf("list = %#v", list)
	}

	destroy, err := service.Destroy(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if storage.destroyedName != "tank2" {
		t.Fatalf("destroyed = %q", storage.destroyedName)
	}
	if _, err := st.Pools().Get(ctx, "pool-tank2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pool should be removed, err=%v", err)
	}
	destroyTask, err := st.Tasks().Get(ctx, destroy.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if destroyTask.Type != domain.TaskTypeDestroyPool || destroyTask.Status != domain.TaskStatusSuccess || destroyTask.Progress != 100 {
		t.Fatalf("destroy task = %#v", destroyTask)
	}
}

// 布局建池后改不了，必须在提交时校验并说明还差几块盘，而不是交给 zpool 报错。
func TestPoolServiceCreateValidatesLayout(t *testing.T) {
	ctx := context.Background()
	disks := func(n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, "/dev/sd"+string(rune('b'+i)))
		}
		return out
	}
	cases := []struct {
		name    string
		req     PoolRequest
		wantErr string // "" 表示接受
		spec    storage.PoolSpec
	}{
		{"default is stripe", PoolRequest{Name: "p", Disks: disks(2)}, "",
			storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: disks(2)}},
		{"mirror default width 2", PoolRequest{Name: "p", Layout: "mirror", Disks: disks(4)}, "",
			storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: disks(4)}},
		{"mirror width 3", PoolRequest{Name: "p", Layout: "mirror", GroupWidth: 3, Disks: disks(6)}, "",
			storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 3, Disks: disks(6)}},
		{"mirror odd count", PoolRequest{Name: "p", Layout: "mirror", Disks: disks(3)}, "镜像池按 2 块一组，请再选 1 块", storage.PoolSpec{}},
		{"mirror too few", PoolRequest{Name: "p", Layout: "mirror", Disks: disks(1)}, "镜像池按 2 块一组，请再选 1 块", storage.PoolSpec{}},
		{"mirror width 4 unsupported", PoolRequest{Name: "p", Layout: "mirror", GroupWidth: 4, Disks: disks(4)}, "镜像只支持 2 路或 3 路", storage.PoolSpec{}},
		{"raidz2 too few", PoolRequest{Name: "p", Layout: "raidz2", Disks: disks(3)}, "raidz2 至少 4 块盘，请再选 1 块", storage.PoolSpec{}},
		{"raidz1 too few", PoolRequest{Name: "p", Layout: "raidz1", Disks: disks(2)}, "raidz1 至少 3 块盘，请再选 1 块", storage.PoolSpec{}},
		{"raidz3 ok is one group", PoolRequest{Name: "p", Layout: "raidz3", Disks: disks(5)}, "",
			storage.PoolSpec{Layout: domain.PoolLayoutRaidz3, GroupWidth: 5, Disks: disks(5)}},
		{"unknown layout", PoolRequest{Name: "p", Layout: "raid10", Disks: disks(4)}, "不支持的存储池布局 raid10", storage.PoolSpec{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newImageTestStore(t)
			fake := &fakePoolStorage{pool: domain.Pool{ID: "pool-p", ServerID: "server-a"}, status: domain.PoolStatus{Name: "p", Health: "ONLINE"}}
			service := PoolService{Store: st, Storage: fake, Now: time.Now}
			_, err := service.Create(ctx, tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, errs.ErrInvalid) {
					t.Fatalf("err = %v, want invalid containing %q", err, tc.wantErr)
				}
				if fake.createdName != "" {
					t.Fatal("pool was created despite the refusal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fake.createdSpec, tc.spec) {
				t.Fatalf("spec = %#v, want %#v", fake.createdSpec, tc.spec)
			}
			pool, err := st.Pools().Get(ctx, "pool-p")
			if err != nil {
				t.Fatal(err)
			}
			if pool.Layout != tc.spec.Layout || pool.GroupWidth != tc.spec.GroupWidth {
				t.Fatalf("stored layout = %q/%d, want %q/%d", pool.Layout, pool.GroupWidth, tc.spec.Layout, tc.spec.GroupWidth)
			}
		})
	}
}

// 加盘按布局：stripe 单块、mirror 整对、raidz 整组；其他数量在提交时拒绝并说明差几块，
// 不留给 zpool 报 "mismatched replication level"。
func TestPoolServiceAddDiskFollowsLayout(t *testing.T) {
	ctx := context.Background()
	newSvc := func(t *testing.T, layout domain.PoolLayout, width int) (PoolService, *fakePoolStorage) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb"}, Layout: layout, GroupWidth: width}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{status: domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: layout, GroupWidth: width}}
		return PoolService{Store: st, Storage: fake}, fake
	}
	cases := []struct {
		name    string
		layout  domain.PoolLayout
		width   int
		disks   []string
		wantErr string
		spec    storage.PoolSpec
	}{
		{"stripe takes any count", domain.PoolLayoutStripe, 1, []string{"/dev/sdc"}, "", storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []string{"/dev/sdc"}}},
		{"mirror takes whole pairs", domain.PoolLayoutMirror, 2, []string{"/dev/sdc", "/dev/sdd"}, "", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{"/dev/sdc", "/dev/sdd"}}},
		{"mirror refuses a lone disk", domain.PoolLayoutMirror, 2, []string{"/dev/sdc"}, "镜像池按 2 块一组加盘，请再选 1 块", storage.PoolSpec{}},
		{"three-way mirror refuses two", domain.PoolLayoutMirror, 3, []string{"/dev/sdc", "/dev/sdd"}, "镜像池按 3 块一组加盘，请再选 1 块", storage.PoolSpec{}},
		{"raidz takes a whole group", domain.PoolLayoutRaidz2, 4, []string{"a", "b", "c", "d"}, "", storage.PoolSpec{Layout: domain.PoolLayoutRaidz2, GroupWidth: 4, Disks: []string{"a", "b", "c", "d"}}},
		{"raidz refuses a partial group", domain.PoolLayoutRaidz2, 4, []string{"a", "b"}, "raidz2 池按整组加盘，每组 4 块，请再选 2 块", storage.PoolSpec{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, fake := newSvc(t, tc.layout, tc.width)
			_, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Disks: tc.disks})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, errs.ErrInvalid) {
					t.Fatalf("err = %v, want invalid containing %q", err, tc.wantErr)
				}
				if fake.addName != "" {
					t.Fatal("zpool add ran despite the refusal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fake.addSpec, tc.spec) {
				t.Fatalf("spec = %#v, want %#v", fake.addSpec, tc.spec)
			}
		})
	}
}

// attach 给存储盘加一路镜像：需要目标、恰好一块不小于原盘的新盘、非 raidz 布局；
// raidz 在提交时就拒绝。
func TestPoolServiceAttachDisk(t *testing.T) {
	ctx := context.Background()
	newSvc := func(t *testing.T, layout domain.PoolLayout) (PoolService, *fakePoolStorage) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb"}, ReadCacheDisks: []string{"/dev/nvme0n1"}, Layout: layout, GroupWidth: 2}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{
			status: domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: layout, GroupWidth: 2},
			disks: []storage.DiskInfo{
				{Path: "/dev/sda", Size: 4000, InUse: true}, {Path: "/dev/sdb", Size: 4000, InUse: true},
				{Path: "/dev/sdz", Size: 4000}, {Path: "/dev/sdy", Size: 2000},
			},
		}
		return PoolService{Store: st, Storage: fake}, fake
	}
	t.Run("stripe member gets a mirror disk", func(t *testing.T) {
		svc, fake := newSvc(t, domain.PoolLayoutStripe)
		if _, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "/dev/sda", Disks: []string{"/dev/sdz"}}); err != nil {
			t.Fatal(err)
		}
		if fake.attachName != "t" || fake.attachTarget != "/dev/sda" || fake.attachDisk != "/dev/sdz" || fake.addName != "" {
			t.Fatalf("attach = %q %q %q (add %q)", fake.attachName, fake.attachTarget, fake.attachDisk, fake.addName)
		}
	})
	refused := []struct {
		name   string
		layout domain.PoolLayout
		req    PoolDiskRequest
		kind   error
		msg    string
	}{
		{"needs a target", domain.PoolLayoutStripe, PoolDiskRequest{Mode: "attach", Disks: []string{"/dev/sdz"}}, errs.ErrInvalid, "请指定要加镜像的盘"},
		{"exactly one disk", domain.PoolLayoutStripe, PoolDiskRequest{Mode: "attach", Target: "/dev/sda", Disks: []string{"/dev/sdz", "/dev/sdy"}}, errs.ErrInvalid, "一次只能给一块盘加一块镜像盘"},
		{"target must be a data disk", domain.PoolLayoutStripe, PoolDiskRequest{Mode: "attach", Target: "/dev/nvme0n1", Disks: []string{"/dev/sdz"}}, errs.ErrInvalid, "/dev/nvme0n1 不是 t 的存储盘"},
		{"new disk must not be smaller", domain.PoolLayoutStripe, PoolDiskRequest{Mode: "attach", Target: "/dev/sda", Disks: []string{"/dev/sdy"}}, errs.ErrInvalid, "镜像盘不能比原盘小"},
		{"raidz cannot take another disk", domain.PoolLayoutRaidz1, PoolDiskRequest{Mode: "attach", Target: "/dev/sda", Disks: []string{"/dev/sdz"}}, errs.ErrConflict, "raidz 无法追加校验盘"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			svc, fake := newSvc(t, tc.layout)
			_, err := svc.AddDisk(ctx, "pool-t", tc.req)
			if err == nil || !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.kind, tc.msg)
			}
			if fake.attachName != "" || fake.addName != "" {
				t.Fatal("zpool ran despite the refusal")
			}
		})
	}
}

// 移盘按盘所在位置：stripe 成员或整组 remove，mirror 成员 detach，raidz 拒绝。
// 归属以池的实时结构为准，不看记录。
func TestPoolServiceRemoveDiskFollowsLayout(t *testing.T) {
	ctx := context.Background()
	mirrorStatus := domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutMirror, GroupWidth: 2,
		Vdevs: []domain.PoolVdev{
			{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}, {Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}}},
			{Name: "mirror-1", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/dev/sdc", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}, {Path: "/dev/sdd", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}}},
		},
		Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}, {Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}, {Path: "/dev/sdc", Role: domain.PoolDiskRoleData, Vdev: "mirror-1"}, {Path: "/dev/sdd", Role: domain.PoolDiskRoleData, Vdev: "mirror-1"}}}
	stripeStatus := domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutStripe, GroupWidth: 1,
		Vdevs: []domain.PoolVdev{
			{Name: "/dev/sda", Kind: "disk", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Vdev: "/dev/sda"}}},
			{Name: "/dev/sdb", Kind: "disk", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Vdev: "/dev/sdb"}}},
		},
		Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Vdev: "/dev/sda"}, {Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Vdev: "/dev/sdb"}}}
	raidzStatus := domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutRaidz1, GroupWidth: 3,
		Vdevs: []domain.PoolVdev{{Name: "raidz1-0", Kind: "raidz1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Vdev: "raidz1-0"}, {Path: "/dev/sdb", Vdev: "raidz1-0"}, {Path: "/dev/sdc", Vdev: "raidz1-0"}}}},
		Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Vdev: "raidz1-0"}, {Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Vdev: "raidz1-0"}, {Path: "/dev/sdc", Role: domain.PoolDiskRoleData, Vdev: "raidz1-0"}}}
	newSvc := func(t *testing.T, status domain.PoolStatus) (PoolService, *fakePoolStorage, *store.SQLStore) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb"}, Layout: status.Layout, GroupWidth: status.GroupWidth}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{status: status}
		return PoolService{Store: st, Storage: fake}, fake, st
	}
	t.Run("stripe member is removed with migration", func(t *testing.T) {
		svc, fake, st := newSvc(t, stripeStatus)
		res, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdb"})
		if err != nil {
			t.Fatal(err)
		}
		if fake.removeName != "t" || fake.removeDisk != "/dev/sdb" || fake.detachName != "" {
			t.Fatalf("remove = %q %q detach = %q", fake.removeName, fake.removeDisk, fake.detachName)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeMigrateDisk)
	})
	t.Run("mirror member is detached", func(t *testing.T) {
		svc, fake, st := newSvc(t, mirrorStatus)
		res, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdb"})
		if err != nil {
			t.Fatal(err)
		}
		if fake.detachName != "t" || fake.detachDisk != "/dev/sdb" || fake.removeName != "" {
			t.Fatalf("detach = %q %q remove = %q", fake.detachName, fake.detachDisk, fake.removeName)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeDetachDisk)
	})
	t.Run("whole mirror group is removed by its name", func(t *testing.T) {
		svc, fake, st := newSvc(t, mirrorStatus)
		res, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "mirror-1"})
		if err != nil {
			t.Fatal(err)
		}
		if fake.removeName != "t" || fake.removeDisk != "mirror-1" || fake.detachName != "" {
			t.Fatalf("remove = %q %q", fake.removeName, fake.removeDisk)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeMigrateDisk)
	})
	t.Run("raidz refuses", func(t *testing.T) {
		svc, fake, _ := newSvc(t, raidzStatus)
		_, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdb"})
		if err == nil || !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "raidz 不能移盘") || !strings.Contains(err.Error(), "换盘") {
			t.Fatalf("err = %v", err)
		}
		if fake.removeName != "" || fake.detachName != "" {
			t.Fatal("zpool ran despite the refusal")
		}
	})
	t.Run("unknown disk is refused by name", func(t *testing.T) {
		svc, fake, _ := newSvc(t, mirrorStatus)
		_, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdx"})
		if err == nil || !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "/dev/sdx") {
			t.Fatalf("err = %v", err)
		}
		if fake.removeName != "" || fake.detachName != "" {
			t.Fatal("zpool ran despite the refusal")
		}
	})
}

// stripe 升级 mirror：一个任务内逐对 attach。首次 attach 前整体校验（raidz、目标非存储盘、
// 盘重复、盘太小）；中途失败点名那一对，已 attach 的保留。
func TestPoolServiceMirrorUpgrade(t *testing.T) {
	ctx := context.Background()
	newSvc := func(t *testing.T, layout domain.PoolLayout) (PoolService, *fakePoolStorage, *store.SQLStore) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb"}, Layout: layout, GroupWidth: 1}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{
			status: domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: layout, GroupWidth: 1},
			disks: []storage.DiskInfo{
				{Path: "/dev/sda", Size: 4000, InUse: true}, {Path: "/dev/sdb", Size: 4000, InUse: true},
				{Path: "/dev/sdc", Size: 4000}, {Path: "/dev/sdd", Size: 4000}, {Path: "/dev/sde", Size: 1000},
			},
		}
		return PoolService{Store: st, Storage: fake}, fake, st
	}
	pairs := []MirrorPair{{Target: "/dev/sda", Disk: "/dev/sdc"}, {Target: "/dev/sdb", Disk: "/dev/sdd"}}

	t.Run("attaches every pair in order", func(t *testing.T) {
		svc, fake, st := newSvc(t, domain.PoolLayoutStripe)
		res, err := svc.MirrorUpgrade(ctx, "pool-t", MirrorUpgradeRequest{Pairs: pairs})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fake.attachCalls, [][2]string{{"/dev/sda", "/dev/sdc"}, {"/dev/sdb", "/dev/sdd"}}) {
			t.Fatalf("attach calls = %#v", fake.attachCalls)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeMirrorUpgrade)
	})
	t.Run("a failure names the pair and keeps what is done", func(t *testing.T) {
		svc, fake, st := newSvc(t, domain.PoolLayoutStripe)
		fake.attachErrAt, fake.attachErr = 2, storage.CommandError{Name: "zpool", Output: "cannot attach /dev/sdd to /dev/sdb: device is busy"}
		res, err := svc.MirrorUpgrade(ctx, "pool-t", MirrorUpgradeRequest{Pairs: pairs})
		if err == nil {
			t.Fatal("upgrade succeeded")
		}
		if len(fake.attachCalls) != 2 {
			t.Fatalf("attach calls = %#v, want it to stop at the failure", fake.attachCalls)
		}
		task, terr := st.Tasks().Get(ctx, res.TaskID)
		if terr != nil {
			t.Fatal(terr)
		}
		if task.Status != domain.TaskStatusFailed || !strings.Contains(task.Error, "第 2 对") || !strings.Contains(task.Error, "/dev/sdb") || !strings.Contains(task.Error, "已完成 1 对") {
			t.Fatalf("task = %#v", task)
		}
	})
	refused := []struct {
		name   string
		layout domain.PoolLayout
		req    MirrorUpgradeRequest
		kind   error
		msg    string
	}{
		{"needs pairs", domain.PoolLayoutStripe, MirrorUpgradeRequest{}, errs.ErrInvalid, "请为每块存储盘选一块镜像盘"},
		{"raidz has no stripe members", domain.PoolLayoutRaidz1, MirrorUpgradeRequest{Pairs: pairs}, errs.ErrConflict, "raidz"},
		{"target must be a data disk", domain.PoolLayoutStripe, MirrorUpgradeRequest{Pairs: []MirrorPair{{Target: "/dev/sdx", Disk: "/dev/sdc"}}}, errs.ErrInvalid, "/dev/sdx 不是 t 的存储盘"},
		{"a disk is used once", domain.PoolLayoutStripe, MirrorUpgradeRequest{Pairs: []MirrorPair{{Target: "/dev/sda", Disk: "/dev/sdc"}, {Target: "/dev/sdb", Disk: "/dev/sdc"}}}, errs.ErrInvalid, "/dev/sdc 被选了两次"},
		{"a disk is not smaller than its target", domain.PoolLayoutStripe, MirrorUpgradeRequest{Pairs: []MirrorPair{{Target: "/dev/sda", Disk: "/dev/sde"}}}, errs.ErrInvalid, "镜像盘不能比原盘小"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			svc, fake, _ := newSvc(t, tc.layout)
			_, err := svc.MirrorUpgrade(ctx, "pool-t", tc.req)
			if err == nil || !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.kind, tc.msg)
			}
			if len(fake.attachCalls) != 0 {
				t.Fatal("zpool attach ran despite the refusal")
			}
		})
	}
}

// special 必须 2～3 路镜像、整组移除；热备按盘增删。都按池的实时结构校验，且各用独立任务类型。
func TestPoolServiceSpecialAndSpare(t *testing.T) {
	ctx := context.Background()
	status := domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutMirror, GroupWidth: 2,
		Vdevs: []domain.PoolVdev{
			{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Vdev: "mirror-0"}, {Path: "/dev/sdb", Vdev: "mirror-0"}}},
			{Name: "mirror-3", Kind: "mirror", Role: domain.PoolDiskRoleSpecial, Disks: []domain.PoolDiskStatus{{Path: "/dev/nvme0n1", Vdev: "mirror-3"}, {Path: "/dev/nvme1n1", Vdev: "mirror-3"}}},
			{Name: "/dev/sdz", Kind: "disk", Role: domain.PoolDiskRoleSpare, Disks: []domain.PoolDiskStatus{{Path: "/dev/sdz"}}},
		}}
	newSvc := func(t *testing.T) (PoolService, *fakePoolStorage, *store.SQLStore) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb"}, Layout: domain.PoolLayoutMirror, GroupWidth: 2}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{status: status}
		return PoolService{Store: st, Storage: fake}, fake, st
	}
	t.Run("special is added mirrored", func(t *testing.T) {
		svc, fake, st := newSvc(t)
		res, err := svc.AddSpecial(ctx, "pool-t", PoolDiskRequest{Disks: []string{"/dev/nvme2n1", "/dev/nvme3n1"}})
		if err != nil {
			t.Fatal(err)
		}
		if fake.specialName != "t" || !reflect.DeepEqual(fake.specialDisks, []string{"/dev/nvme2n1", "/dev/nvme3n1"}) {
			t.Fatalf("special = %q %v", fake.specialName, fake.specialDisks)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeAddSpecial)
	})
	t.Run("special refuses one disk or four", func(t *testing.T) {
		svc, fake, _ := newSvc(t)
		for _, disks := range [][]string{{"/dev/nvme2n1"}, {"a", "b", "c", "d"}} {
			_, err := svc.AddSpecial(ctx, "pool-t", PoolDiskRequest{Disks: disks})
			if err == nil || !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "元数据盘必须镜像") {
				t.Fatalf("disks %v: err = %v", disks, err)
			}
		}
		if fake.specialName != "" {
			t.Fatal("special added despite the refusal")
		}
	})
	t.Run("special is removed as a group", func(t *testing.T) {
		svc, fake, st := newSvc(t)
		res, err := svc.RemoveSpecial(ctx, "pool-t", PoolDiskRequest{Disk: "mirror-3"})
		if err != nil {
			t.Fatal(err)
		}
		if fake.removeSpecialName != "t" || fake.removeSpecialGroup != "mirror-3" {
			t.Fatalf("remove special = %q %q", fake.removeSpecialName, fake.removeSpecialGroup)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeRemoveSpecial)
		// special 组的成员盘不等于组本身；数据组也不是 special。
		for _, disk := range []string{"/dev/nvme0n1", "mirror-0"} {
			if _, err := svc.RemoveSpecial(ctx, "pool-t", PoolDiskRequest{Disk: disk}); err == nil || !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("%s: err = %v", disk, err)
			}
		}
	})
	t.Run("spares come and go by disk", func(t *testing.T) {
		svc, fake, st := newSvc(t)
		res, err := svc.AddSpare(ctx, "pool-t", PoolDiskRequest{Disks: []string{"/dev/sdy"}})
		if err != nil {
			t.Fatal(err)
		}
		if fake.spareName != "t" || !reflect.DeepEqual(fake.spareDisks, []string{"/dev/sdy"}) {
			t.Fatalf("spare = %q %v", fake.spareName, fake.spareDisks)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeAddSpare)
		res, err = svc.RemoveSpare(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdz"})
		if err != nil {
			t.Fatal(err)
		}
		if fake.removeSpareName != "t" || fake.removeSpareDisk != "/dev/sdz" {
			t.Fatalf("remove spare = %q %q", fake.removeSpareName, fake.removeSpareDisk)
		}
		assertPoolTask(t, st, res.TaskID, domain.TaskTypeRemoveSpare)
		if _, err := svc.RemoveSpare(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sda"}); err == nil || !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "不是 t 的热备盘") {
			t.Fatalf("data disk as spare: err = %v", err)
		}
	})
}

// raidz 组只在本机支持时才能单盘扩容；池信息如实标出，拒绝时说明缺什么。
func TestPoolServiceRaidzExpansion(t *testing.T) {
	ctx := context.Background()
	newSvc := func(t *testing.T, expandable bool, note string) (PoolService, *fakePoolStorage) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, Layout: domain.PoolLayoutRaidz1, GroupWidth: 3}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{status: domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutRaidz1, GroupWidth: 3, RaidzExpandable: expandable, RaidzExpandNote: note,
			Vdevs: []domain.PoolVdev{{Name: "raidz1-0", Kind: "raidz1", Role: domain.PoolDiskRoleData, Disks: []domain.PoolDiskStatus{{Path: "/dev/sda"}, {Path: "/dev/sdb"}, {Path: "/dev/sdc"}}}}}}
		return PoolService{Store: st, Storage: fake}, fake
	}
	t.Run("supported: attach to the group", func(t *testing.T) {
		svc, fake := newSvc(t, true, "")
		if _, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "raidz1-0", Disks: []string{"/dev/sdd"}}); err != nil {
			t.Fatal(err)
		}
		if fake.attachName != "t" || fake.attachTarget != "raidz1-0" || fake.attachDisk != "/dev/sdd" {
			t.Fatalf("attach = %q %q %q", fake.attachName, fake.attachTarget, fake.attachDisk)
		}
		got, err := svc.Get(ctx, "pool-t")
		if err != nil {
			t.Fatal(err)
		}
		if !got.RaidzExpandable {
			t.Fatalf("item = %#v", got)
		}
	})
	t.Run("unsupported: refused with the reason", func(t *testing.T) {
		svc, fake := newSvc(t, false, "本机 ZFS 用户态版本 2.2.2 不支持 raidz 单盘扩容（需 ≥ 2.3）")
		_, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "raidz1-0", Disks: []string{"/dev/sdd"}})
		if err == nil || !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "2.2.2") {
			t.Fatalf("err = %v", err)
		}
		if fake.attachName != "" {
			t.Fatal("attach ran despite the refusal")
		}
		got, _ := svc.Get(ctx, "pool-t")
		if got.RaidzExpandable || !strings.Contains(got.RaidzExpandNote, "2.2.2") {
			t.Fatalf("item = %#v", got)
		}
	})
}

// ZFS 拿到整盘会自动分区并把成员报成 /dev/sda1，而操作者和 zpool 命令用 /dev/sda。
// 所有按成员查找的地方（attach、detach、镜像配对、移除热备）都必须把两者视为同一块盘。
func TestPoolServiceMatchesWholeDiskAndItsPartition(t *testing.T) {
	ctx := context.Background()
	status := domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutMirror, GroupWidth: 2,
		Vdevs: []domain.PoolVdev{
			{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Disks: []domain.PoolDiskStatus{{Path: "/dev/sda1", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}, {Path: "/dev/sdb1", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}}},
			{Name: "/dev/nvme0n1p1", Kind: "disk", Role: domain.PoolDiskRoleSpare, Disks: []domain.PoolDiskStatus{{Path: "/dev/nvme0n1p1", Role: domain.PoolDiskRoleSpare}}},
		},
		Disks: []domain.PoolDiskStatus{{Path: "/dev/sda1", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}, {Path: "/dev/sdb1", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"}, {Path: "/dev/nvme0n1p1", Role: domain.PoolDiskRoleSpare}}}
	newSvc := func(t *testing.T) (PoolService, *fakePoolStorage) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda1", "/dev/sdb1"}, Layout: domain.PoolLayoutMirror, GroupWidth: 2}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePoolStorage{status: status, disks: []storage.DiskInfo{{Path: "/dev/sda", Size: 4000, InUse: true}, {Path: "/dev/sdb", Size: 4000, InUse: true}, {Path: "/dev/sdc", Size: 4000}, {Path: "/dev/sdd", Size: 1000}}}
		return PoolService{Store: st, Storage: fake}, fake
	}
	t.Run("attach by whole-disk name", func(t *testing.T) {
		svc, fake := newSvc(t)
		if _, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "/dev/sda", Disks: []string{"/dev/sdc"}}); err != nil {
			t.Fatal(err)
		}
		if fake.attachTarget != "/dev/sda" || fake.attachDisk != "/dev/sdc" {
			t.Fatalf("attach = %q %q", fake.attachTarget, fake.attachDisk)
		}
		// 容量检查也能通过分区名找到整盘。
		svc2, _ := newSvc(t)
		if _, err := svc2.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "/dev/sda1", Disks: []string{"/dev/sdd"}}); err == nil || !strings.Contains(err.Error(), "镜像盘不能比原盘小") {
			t.Fatalf("size check via partition name: err = %v", err)
		}
	})
	t.Run("detach by whole-disk name", func(t *testing.T) {
		svc, fake := newSvc(t)
		if _, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sdb"}); err != nil {
			t.Fatal(err)
		}
		if fake.detachDisk != "/dev/sdb" {
			t.Fatalf("detach = %q", fake.detachDisk)
		}
	})
	t.Run("mirror pairs by whole-disk name", func(t *testing.T) {
		svc, fake := newSvc(t)
		// 记录里是分区名的 stripe 池，用整盘名升级。
		fake.status = domain.PoolStatus{Name: "t", Health: "ONLINE", Layout: domain.PoolLayoutStripe, GroupWidth: 1}
		if err := svc.Store.Pools().Update(ctx, domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sda1", "/dev/sdb1"}, Layout: domain.PoolLayoutStripe, GroupWidth: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.MirrorUpgrade(ctx, "pool-t", MirrorUpgradeRequest{Pairs: []MirrorPair{{Target: "/dev/sda", Disk: "/dev/sdc"}}}); err != nil {
			t.Fatal(err)
		}
		if len(fake.attachCalls) != 1 || fake.attachCalls[0] != [2]string{"/dev/sda", "/dev/sdc"} {
			t.Fatalf("attach calls = %#v", fake.attachCalls)
		}
	})
	t.Run("spare removal by whole-disk name (nvme partition)", func(t *testing.T) {
		svc, fake := newSvc(t)
		if _, err := svc.RemoveSpare(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/nvme0n1"}); err != nil {
			t.Fatal(err)
		}
		if fake.removeSpareDisk != "/dev/nvme0n1" {
			t.Fatalf("remove spare = %q", fake.removeSpareDisk)
		}
	})
	t.Run("a different disk is still refused", func(t *testing.T) {
		svc, _ := newSvc(t)
		if _, err := svc.AddDisk(ctx, "pool-t", PoolDiskRequest{Mode: "attach", Target: "/dev/sda11", Disks: []string{"/dev/sdc"}}); err == nil {
			t.Fatal("/dev/sda11 accepted as /dev/sda1")
		}
		if _, err := svc.RemoveDisk(ctx, "pool-t", PoolDiskRequest{Disk: "/dev/sd"}); err == nil {
			t.Fatal("/dev/sd accepted as a member")
		}
	})
}

// zpool 保留 vdev 关键字（"mirrors" 会报 "name is reserved"）并有字符规则，提交时就要拦下。
func TestPoolServiceCreateRefusesNamesZFSWouldRefuse(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, want string }{
		{"mirrors", "保留"},
		{"raidz2pool", "保留"},
		{"draid", "保留"},
		{"spare1", "保留"},
		{"log", "保留"},
		{"c0t0d0", "c 加数字"},
		{"1tank", "字母开头"},
		{"tank/data", "只能包含"},
		{"tank pool", "空格"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newImageTestStore(t)
			fake := &fakePoolStorage{pool: domain.Pool{ID: "pool-x", ServerID: "server-a"}, status: domain.PoolStatus{Health: "ONLINE"}}
			_, err := (PoolService{Store: st, Storage: fake, Now: time.Now}).Create(ctx, PoolRequest{Name: tc.name, Disks: []string{"/dev/sdb"}})
			if err == nil || !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("name %q: err = %v, want invalid containing %q", tc.name, err, tc.want)
			}
			if fake.createdName != "" {
				t.Fatal("zpool create ran despite the refusal")
			}
		})
	}
	// 普通名字，包括只是包含关键字的，都允许。
	for _, name := range []string{"tank", "data-pool", "backup_2", "mymirror", "Pool.A:1"} {
		st := newImageTestStore(t)
		fake := &fakePoolStorage{pool: domain.Pool{ID: "pool-x", ServerID: "server-a"}, status: domain.PoolStatus{Health: "ONLINE"}}
		if _, err := (PoolService{Store: st, Storage: fake, Now: time.Now}).Create(ctx, PoolRequest{Name: name, Disks: []string{"/dev/sdb"}}); err != nil {
			t.Fatalf("name %q refused: %v", name, err)
		}
	}
}

func TestPoolServiceRejectsDestroyWhenDataExists(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{poolName: "tank2"} // 镜像在这个池上

	_, err := (PoolService{Store: st, Storage: storage}).Destroy(ctx, "pool-tank2")
	if !errors.Is(err, ErrPoolInUse) {
		t.Fatalf("err = %v", err)
	}
	if storage.destroyedName != "" {
		t.Fatalf("storage should not be called, destroyed=%q", storage.destroyedName)
	}
}

func TestPoolServiceRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-exists", ServerID: "server-a", Name: "tank2"}); err != nil {
		t.Fatal(err)
	}
	service := PoolService{Store: st, Storage: &fakePoolStorage{statusByName: map[string]domain.PoolStatus{
		"tank2": {Capacity: 1, Used: 0, Health: "ONLINE", Name: "tank2"},
	}}}

	_, err := service.Create(ctx, PoolRequest{Name: "tank2", Disks: []string{"/dev/sdb"}})
	if !errors.Is(err, ErrPoolExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestPoolServiceListReadsStatusAndUpdatesCapacity(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	service := PoolService{Store: st, Storage: &fakePoolStorage{statusByName: map[string]domain.PoolStatus{
		"tank-a": {Name: "tank-a", Health: "ONLINE", Capacity: 1024, Used: 100},
		"tank-b": {Name: "tank-b", Health: "DEGRADED", Capacity: 2048, Used: 700},
	}}}
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Servers().Create(ctx, domain.Server{ID: "srv-2", Name: "srv-2", IP: "10.0.0.2", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-a", ServerID: "server-a", Name: "tank-a", Capacity: 1, Used: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-b", ServerID: "srv-2", Name: "tank-b", Capacity: 2, Used: 2}); err != nil {
		t.Fatal(err)
	}

	got, err := service.List(ctx, "server-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 {
		t.Fatalf("got total = %d", got.Total)
	}
	if got.Items[0].Capacity != 1024 || got.Items[0].Used != 100 || got.Items[0].Health != "ONLINE" {
		t.Fatalf("got = %#v", got.Items[0])
	}
	pool, err := st.Pools().Get(ctx, "pool-a")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Capacity != 1024 || pool.Used != 100 {
		t.Fatalf("pool db = %#v", pool)
	}
}

func TestPoolServiceListDisksWrapsStorageResult(t *testing.T) {
	ctx := context.Background()
	disks := []storage.DiskInfo{{Path: "/dev/sdb", Name: "sdb", Type: "disk", Size: 1073741824}}
	storage := &fakePoolStorage{disks: disks}
	service := PoolService{Storage: storage}

	result, err := service.ListDisks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !storage.listDisksCalled || result.Total != 1 || !reflect.DeepEqual(result.Items, disks) {
		t.Fatalf("result=%#v storage=%#v", result, storage)
	}
}

func TestPoolServiceDiskOperationsRefreshPoolAndTasks(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2", Disks: []string{"/dev/sdb"}, Capacity: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := st.PoolDisks().Create(ctx, domain.PoolDisk{ID: "pool-tank2-disk-1", PoolID: "pool-tank2", Path: "/dev/sdb", Role: domain.PoolDiskRoleData}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{}
	service := PoolService{Store: st, Storage: storage}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 2000,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
			{Path: "/dev/sdc", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
		},
	}
	add, err := service.AddDisk(ctx, "pool-tank2", PoolDiskRequest{Disks: []string{"/dev/sdc"}})
	if err != nil {
		t.Fatal(err)
	}
	if storage.addName != "tank2" || !reflect.DeepEqual(storage.addDisks, []string{"/dev/sdc"}) {
		t.Fatalf("add call = %q %#v", storage.addName, storage.addDisks)
	}
	if add.Pool.Capacity != 2000 || len(add.Pool.DiskItems) != 2 {
		t.Fatalf("add result = %#v", add.Pool)
	}
	assertPoolTask(t, st, add.TaskID, domain.TaskTypeAddDisk)
	pool, err := st.Pools().Get(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pool.Disks, []string{"/dev/sdb", "/dev/sdc"}) || pool.Capacity != 2000 {
		t.Fatalf("pool after add = %#v", pool)
	}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 1500,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdc", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
		},
	}
	remove, err := service.RemoveDisk(ctx, "pool-tank2", PoolDiskRequest{Disk: "/dev/sdb"})
	if err != nil {
		t.Fatal(err)
	}
	if storage.removeName != "tank2" || storage.removeDisk != "/dev/sdb" {
		t.Fatalf("remove call = %q %q", storage.removeName, storage.removeDisk)
	}
	if remove.Pool.Capacity != 1500 || !reflect.DeepEqual(remove.Pool.Disks, []string{"/dev/sdc"}) {
		t.Fatalf("remove result = %#v", remove.Pool)
	}
	assertPoolTask(t, st, remove.TaskID, domain.TaskTypeMigrateDisk)

	storage.status = domain.PoolStatus{
		Name:      "tank2",
		Health:    "ONLINE",
		Operation: "resilver",
		Progress:  "50%",
		Capacity:  1500,
		Used:      100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdd", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
		},
	}
	replace, err := service.ReplaceDisk(ctx, "pool-tank2", PoolReplaceDiskRequest{OldDisk: "/dev/sdc", NewDisk: "/dev/sdd"})
	if err != nil {
		t.Fatal(err)
	}
	if storage.replaceName != "tank2" || storage.oldDisk != "/dev/sdc" || storage.newDisk != "/dev/sdd" {
		t.Fatalf("replace call = %#v", storage)
	}
	if !reflect.DeepEqual(replace.Pool.Disks, []string{"/dev/sdd"}) || replace.Pool.DiskItems[0].Status != "ONLINE" || replace.Pool.Operation != "resilver" || replace.Pool.Progress != "50%" {
		t.Fatalf("replace result = %#v", replace.Pool)
	}
	assertPoolTask(t, st, replace.TaskID, domain.TaskTypeReplaceDisk)
	disks, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Path != "/dev/sdd" {
		t.Fatalf("pool disks = %#v", disks)
	}
}

func TestPoolServiceReadCacheOperationsRefreshPoolAndTasks(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2", Disks: []string{"/dev/sdb"}, Capacity: 1000}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{}
	service := PoolService{Store: st, Storage: storage}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 1000,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
			{Path: "/dev/nvme0n1", Role: domain.PoolDiskRoleReadCache, Status: "ONLINE"},
		},
	}
	add, err := service.AddReadCache(ctx, "pool-tank2", PoolReadCacheRequest{Disks: []string{"/dev/nvme0n1"}})
	if err != nil {
		t.Fatal(err)
	}
	if storage.addReadCacheName != "tank2" || !reflect.DeepEqual(storage.addReadCacheDisks, []string{"/dev/nvme0n1"}) {
		t.Fatalf("add read cache call = %#v", storage)
	}
	if !reflect.DeepEqual(add.Pool.ReadCacheDisks, []string{"/dev/nvme0n1"}) || len(add.Pool.DiskItems) != 2 {
		t.Fatalf("add result = %#v", add.Pool)
	}
	assertPoolTask(t, st, add.TaskID, domain.TaskTypeAddReadCache)
	pool, err := st.Pools().Get(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pool.ReadCacheDisks, []string{"/dev/nvme0n1"}) || !reflect.DeepEqual(pool.Disks, []string{"/dev/sdb"}) {
		t.Fatalf("pool after add = %#v", pool)
	}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 1000,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
		},
	}
	remove, err := service.RemoveReadCache(ctx, "pool-tank2", PoolReadCacheRequest{Disk: "/dev/nvme0n1"})
	if err != nil {
		t.Fatal(err)
	}
	if storage.removeReadCacheName != "tank2" || storage.removeReadCacheDisk != "/dev/nvme0n1" {
		t.Fatalf("remove read cache call = %#v", storage)
	}
	if len(remove.Pool.ReadCacheDisks) != 0 || !reflect.DeepEqual(remove.Pool.Disks, []string{"/dev/sdb"}) {
		t.Fatalf("remove result = %#v", remove.Pool)
	}
	assertPoolTask(t, st, remove.TaskID, domain.TaskTypeRemoveReadCache)
	disks, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Path != "/dev/sdb" || disks[0].Role != domain.PoolDiskRoleData {
		t.Fatalf("pool disks = %#v", disks)
	}
}

func TestPoolServiceWriteCacheOperationsRefreshPoolAndTasks(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2", Disks: []string{"/dev/sdb"}, Capacity: 1000}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{}
	service := PoolService{Store: st, Storage: storage}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 1000,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
			{Path: "/dev/nvme0n1", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE"},
		},
	}
	add, err := service.AddWriteCache(ctx, "pool-tank2", PoolWriteCacheRequest{Disks: []string{"/dev/nvme0n1"}})
	if err != nil {
		t.Fatal(err)
	}
	if storage.addWriteCacheName != "tank2" || !reflect.DeepEqual(storage.addWriteCacheDisks, []string{"/dev/nvme0n1"}) {
		t.Fatalf("add write cache call = %#v", storage)
	}
	if !reflect.DeepEqual(add.Pool.WriteCacheDisks, []string{"/dev/nvme0n1"}) || len(add.Pool.DiskItems) != 2 {
		t.Fatalf("add result = %#v", add.Pool)
	}
	assertPoolTask(t, st, add.TaskID, domain.TaskTypeAddWriteCache)
	pool, err := st.Pools().Get(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pool.WriteCacheDisks, []string{"/dev/nvme0n1"}) || !reflect.DeepEqual(pool.Disks, []string{"/dev/sdb"}) {
		t.Fatalf("pool after add = %#v", pool)
	}

	storage.status = domain.PoolStatus{
		Name:     "tank2",
		Health:   "ONLINE",
		Capacity: 1000,
		Used:     100,
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "ONLINE"},
		},
	}
	remove, err := service.RemoveWriteCache(ctx, "pool-tank2", PoolWriteCacheRequest{Disk: "/dev/nvme0n1"})
	if err != nil {
		t.Fatal(err)
	}
	if storage.removeWriteCacheName != "tank2" || storage.removeWriteCacheDisk != "/dev/nvme0n1" {
		t.Fatalf("remove write cache call = %#v", storage)
	}
	if len(remove.Pool.WriteCacheDisks) != 0 || !reflect.DeepEqual(remove.Pool.Disks, []string{"/dev/sdb"}) {
		t.Fatalf("remove result = %#v", remove.Pool)
	}
	assertPoolTask(t, st, remove.TaskID, domain.TaskTypeRemoveWriteCache)
	disks, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Path != "/dev/sdb" || disks[0].Role != domain.PoolDiskRoleData {
		t.Fatalf("pool disks = %#v", disks)
	}
}

func TestPoolServiceFlushWriteCache(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2", Disks: []string{"/dev/sdb"}, Capacity: 1000}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{}
	service := PoolService{Store: st, Storage: storage}

	result, err := service.FlushWriteCache(ctx, "pool-tank2")
	if err != nil {
		t.Fatal(err)
	}
	if storage.flushWriteCacheName != "tank2" {
		t.Fatalf("flush call = %#v", storage)
	}
	if result.TaskID == "" {
		t.Fatalf("result = %#v", result)
	}
	task, err := st.Tasks().Get(context.Background(), result.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Type != domain.TaskTypeFlushCache || task.Status != domain.TaskStatusSuccess || task.Progress != 100 {
		t.Fatalf("flush task = %#v", task)
	}
}

func assertPoolTask(t *testing.T, st store.Store, taskID string, typ domain.TaskType) {
	t.Helper()
	task, err := st.Tasks().Get(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Type != typ || task.Status != domain.TaskStatusSuccess || task.Progress != 100 {
		t.Fatalf("task = %#v", task)
	}
}

type fakePoolStorage struct {
	storage.StorageAgent
	backupPool           string
	poolName             string
	pool                 domain.Pool
	status               domain.PoolStatus
	statusByName         map[string]domain.PoolStatus
	statusErrByName      map[string]error
	disks                []storage.DiskInfo
	listDisksCalled      bool
	createdName          string
	createdDisks         []string
	createdSpec          storage.PoolSpec
	destroyedName        string
	addName              string
	addDisks             []string
	addSpec              storage.PoolSpec
	attachName           string
	attachTarget         string
	attachDisk           string
	detachName           string
	detachDisk           string
	attachCalls          [][2]string
	attachErrAt          int
	attachErr            error
	specialName          string
	specialDisks         []string
	removeSpecialName    string
	removeSpecialGroup   string
	spareName            string
	spareDisks           []string
	removeSpareName      string
	removeSpareDisk      string
	removeName           string
	removeDisk           string
	replaceName          string
	oldDisk              string
	newDisk              string
	addReadCacheName     string
	addReadCacheDisks    []string
	removeReadCacheName  string
	removeReadCacheDisk  string
	addWriteCacheName    string
	addWriteCacheDisks   []string
	removeWriteCacheName string
	removeWriteCacheDisk string
	flushWriteCacheName  string
}

func (s *fakePoolStorage) CreatePool(_ context.Context, name string, spec storage.PoolSpec) (domain.Pool, error) {
	s.createdName = name
	s.createdDisks = append([]string{}, spec.Disks...)
	s.createdSpec = spec
	pool := s.pool
	pool.Name = name
	pool.Disks = append([]string{}, spec.Disks...)
	pool.Layout = spec.Layout
	pool.GroupWidth = spec.GroupWidth
	if pool.ID == "" {
		pool.ID, pool.ServerID = "pool-"+name, "server-a"
	}
	delete(s.statusErrByName, name) // 建出来了，就读得到
	return pool, nil
}

func (s *fakePoolStorage) ListDisks(context.Context) ([]storage.DiskInfo, error) {
	s.listDisksCalled = true
	return s.disks, nil
}

func (s *fakePoolStorage) DataPoolName() string { return s.poolName }

func (s *fakePoolStorage) BackupPoolName(context.Context) (string, error) { return s.backupPool, nil }

func (s *fakePoolStorage) ServerID() string { return "server-a" }

func (s *fakePoolStorage) DestroyPool(_ context.Context, name string) error {
	s.destroyedName = name
	return nil
}

func (s *fakePoolStorage) AddDisk(_ context.Context, name string, spec storage.PoolSpec) error {
	s.addName = name
	s.addDisks = append([]string{}, spec.Disks...)
	s.addSpec = spec
	return nil
}

func (s *fakePoolStorage) AttachDisk(_ context.Context, name, target, disk string) error {
	s.attachName, s.attachTarget, s.attachDisk = name, target, disk
	s.attachCalls = append(s.attachCalls, [2]string{target, disk})
	if s.attachErrAt > 0 && len(s.attachCalls) == s.attachErrAt {
		return s.attachErr
	}
	return nil
}

func (s *fakePoolStorage) AddSpecial(_ context.Context, name string, disks []string) error {
	s.specialName, s.specialDisks = name, append([]string{}, disks...)
	return nil
}
func (s *fakePoolStorage) RemoveSpecial(_ context.Context, name, group string) error {
	s.removeSpecialName, s.removeSpecialGroup = name, group
	return nil
}
func (s *fakePoolStorage) AddSpare(_ context.Context, name string, disks []string) error {
	s.spareName, s.spareDisks = name, append([]string{}, disks...)
	return nil
}
func (s *fakePoolStorage) RemoveSpare(_ context.Context, name, disk string) error {
	s.removeSpareName, s.removeSpareDisk = name, disk
	return nil
}

func (s *fakePoolStorage) DetachDisk(_ context.Context, name, disk string) error {
	s.detachName, s.detachDisk = name, disk
	return nil
}

func (s *fakePoolStorage) RemoveDisk(_ context.Context, name, disk string) error {
	s.removeName = name
	s.removeDisk = disk
	return nil
}

func (s *fakePoolStorage) ReplaceDisk(_ context.Context, name, oldDisk, newDisk string) error {
	s.replaceName = name
	s.oldDisk = oldDisk
	s.newDisk = newDisk
	return nil
}

func (s *fakePoolStorage) AddReadCache(_ context.Context, name string, disks []string) error {
	s.addReadCacheName = name
	s.addReadCacheDisks = append([]string{}, disks...)
	return nil
}

func (s *fakePoolStorage) RemoveReadCache(_ context.Context, name, disk string) error {
	s.removeReadCacheName = name
	s.removeReadCacheDisk = disk
	return nil
}

func (s *fakePoolStorage) AddWriteCache(_ context.Context, name string, disks []string) error {
	s.addWriteCacheName = name
	s.addWriteCacheDisks = append([]string{}, disks...)
	return nil
}

func (s *fakePoolStorage) RemoveWriteCache(_ context.Context, name, disk string) error {
	s.removeWriteCacheName = name
	s.removeWriteCacheDisk = disk
	return nil
}

func (s *fakePoolStorage) FlushWriteCache(_ context.Context, name string) error {
	s.flushWriteCacheName = name
	return nil
}

func (s *fakePoolStorage) PoolStatus(_ context.Context, name string) (domain.PoolStatus, error) {
	if err := s.statusErrByName[name]; err != nil {
		return domain.PoolStatus{}, err
	}
	if s.statusByName != nil {
		if status, ok := s.statusByName[name]; ok {
			status.Name = name
			return status, nil
		}
	}
	status := s.status
	status.Name = name
	return status, nil
}

// 只拦数据池的销毁：别的池（如备份池）不能因为数据池上有镜像就删不掉。
func TestDestroyRefusesOnlyForThePoolTheDataLivesOn(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{
		ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows,
		State: domain.ImageStateNormal, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "server-a", Name: "server-a", IP: "10.0.0.1",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.Pool{
		{ID: "pool-tank", Name: "tank", ServerID: "server-a"},
		{ID: "pool-spare", Name: "spare", ServerID: "server-a"},
	} {
		if err := st.Pools().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	svc := PoolService{Store: st, Storage: &fakePoolStorage{poolName: "tank"}, Async: false}

	// 镜像所在的池仍受保护。
	if _, err := svc.Destroy(ctx, "pool-tank"); !errors.Is(err, ErrPoolInUse) {
		t.Fatalf("destroying the data pool: err = %v, want ErrPoolInUse", err)
	}
	// 其他池与镜像无关，可以销毁。
	if _, err := svc.Destroy(ctx, "pool-spare"); err != nil {
		t.Fatalf("destroying an unrelated pool: %v", err)
	}
}

// 一个池读不到（拔盘、export、断线）时列表不能整体失败：正常池要照常显示，坏池标为未知，
// 仍可在界面上操作。
func TestListReportsAnUnreadablePoolInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "server-a", Name: "server-a", IP: "10.0.0.1",
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.Pool{
		{ID: "pool-tank", Name: "tank", ServerID: "server-a", Capacity: 100, Used: 10},
		{ID: "pool-gone", Name: "gone", ServerID: "server-a"},
	} {
		if err := st.Pools().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	agent := &fakePoolStorage{
		poolName: "tank",
		statusByName: map[string]domain.PoolStatus{
			"tank": {Health: "ONLINE", Capacity: 100, Used: 10},
		},
		statusErrByName: map[string]error{"gone": errors.New("cannot open 'gone': no such pool")},
	}

	res, err := (PoolService{Store: st, Storage: agent}).List(ctx, "")
	if err != nil {
		t.Fatalf("the whole listing failed for one bad pool: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %#v, want both pools listed", res.Items)
	}
	byName := map[string]PoolItem{}
	for _, it := range res.Items {
		byName[it.Name] = it
	}
	if byName["tank"].Health != "ONLINE" {
		t.Fatalf("healthy pool = %#v", byName["tank"])
	}
	// 坏池在列表中，且如实标出状态。
	if got := strings.ToUpper(byName["gone"].Health); got != "UNKNOWN" {
		t.Fatalf("unreadable pool health = %q, want UNKNOWN", got)
	}
}

// 预先手工建好的数据池（--pool / NDISKLESS_POOL）要被补登记，否则界面显示 0 个池、
// 无容量、无法管理。
func TestAdoptDataPoolRegistersAPoolPreparedOutsideTheProduct(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakePoolStorage{
		poolName: "tank",
		statusByName: map[string]domain.PoolStatus{"tank": {
			Health: "ONLINE", Capacity: 500, Used: 120,
			Layout: domain.PoolLayoutRaidz2, GroupWidth: 4,
			Disks: []domain.PoolDiskStatus{
				{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"},
				{Path: "/dev/sdb", Role: domain.PoolDiskRoleReadCache, Status: "ONLINE", Vdev: "/dev/sdb"},
				{Path: "/dev/sdz", Role: domain.PoolDiskRoleSpare, Status: "AVAIL"},
			},
		}},
	}
	svc := PoolService{Store: st, Storage: agent}

	if err := svc.AdoptDataPool(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "tank" {
		t.Fatalf("items = %#v", res.Items)
	}
	item := res.Items[0]
	if item.Capacity != 500 || item.Used != 120 || item.Health != "ONLINE" {
		t.Fatalf("adopted pool reports nothing about itself: %#v", item)
	}
	// 手工建的 raidz2 池按实际布局登记。
	if item.Layout != "raidz2" || item.GroupWidth != 4 {
		t.Fatalf("adopted layout = %q/%d", item.Layout, item.GroupWidth)
	}
	// 磁盘一并登记，各带所在分组，热备盘用自己的角色。
	if len(item.DiskItems) != 3 || item.DiskItems[0].Vdev != "raidz2-0" || item.DiskItems[2].Role != "spare" {
		t.Fatalf("disks = %#v", item.DiskItems)
	}
	stored, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]domain.PoolDisk{}
	for _, d := range stored {
		byPath[d.Path] = d
	}
	if byPath["/dev/sda"].Vdev != "raidz2-0" || byPath["/dev/sdz"].Role != domain.PoolDiskRoleSpare {
		t.Fatalf("stored pool disks = %#v", stored)
	}
}

// 布局以池为准而非记录：手工改成镜像或产品外建的池要按实际布局显示，它决定加盘时的选项。
func TestPoolServiceListAppliesObservedLayoutAndGroups(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	mirror := domain.PoolStatus{Name: "tank", Health: "ONLINE", Capacity: 1000, Used: 100,
		Layout: domain.PoolLayoutMirror, GroupWidth: 2,
		Vdevs: []domain.PoolVdev{
			{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{
				{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"},
				{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "OFFLINE", Vdev: "mirror-0"}}},
			{Name: "/dev/sdz", Kind: "disk", Role: domain.PoolDiskRoleSpare, Status: "AVAIL", Disks: []domain.PoolDiskStatus{
				{Path: "/dev/sdz", Role: domain.PoolDiskRoleSpare, Status: "AVAIL"}}},
		},
		Disks: []domain.PoolDiskStatus{
			{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"},
			{Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Status: "OFFLINE", Vdev: "mirror-0"},
			{Path: "/dev/sdz", Role: domain.PoolDiskRoleSpare, Status: "AVAIL"},
		}}
	service := PoolService{Store: st, Storage: &fakePoolStorage{statusByName: map[string]domain.PoolStatus{"tank": mirror}}}
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	// 记录里仍是创建时的 stripe。
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank", ServerID: "server-a", Name: "tank", Layout: domain.PoolLayoutStripe, GroupWidth: 1, Capacity: 1000, Used: 100}); err != nil {
		t.Fatal(err)
	}

	got, err := service.List(ctx, "server-a")
	if err != nil {
		t.Fatal(err)
	}
	item := got.Items[0]
	if item.Layout != "mirror" || item.GroupWidth != 2 {
		t.Fatalf("item layout = %q/%d, want the observed mirror", item.Layout, item.GroupWidth)
	}
	if len(item.Groups) != 2 || item.Groups[0].Name != "mirror-0" || item.Groups[0].Kind != "mirror" || item.Groups[0].Role != "data" || item.Groups[0].Status != "ONLINE" || len(item.Groups[0].Disks) != 2 || item.Groups[0].Disks[1].Status != "OFFLINE" {
		t.Fatalf("groups = %#v", item.Groups)
	}
	if item.Groups[1].Role != "spare" {
		t.Fatalf("spare group = %#v", item.Groups[1])
	}
	// 热备盘可见，但不算存储盘。
	if !reflect.DeepEqual(item.Disks, []string{"/dev/sda", "/dev/sdb"}) {
		t.Fatalf("data disks = %v", item.Disks)
	}
	pool, err := st.Pools().Get(ctx, "pool-tank")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Layout != domain.PoolLayoutMirror || pool.GroupWidth != 2 {
		t.Fatalf("stored layout = %q/%d, want observed value written back", pool.Layout, pool.GroupWidth)
	}
}

func TestAdoptDataPoolIsIdempotentAndQuietWhenThereIsNoPoolYet(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakePoolStorage{
		poolName:     "tank",
		statusByName: map[string]domain.PoolStatus{"tank": {Health: "ONLINE", Capacity: 500, Used: 120}},
	}
	svc := PoolService{Store: st, Storage: agent}

	for i := 0; i < 3; i++ {
		if err := svc.AdoptDataPool(ctx); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	res, _ := svc.List(ctx, "")
	if len(res.Items) != 1 {
		t.Fatalf("adopted %d times: %#v", len(res.Items), res.Items)
	}

	// 全新机器还没有池，这是向导第一步，不是错误。
	empty := newImageTestStore(t)
	fresh := PoolService{Store: empty, Storage: &fakePoolStorage{
		poolName:        "tank",
		statusErrByName: map[string]error{"tank": errors.New("cannot open 'tank': no such pool")},
	}}
	if err := fresh.AdoptDataPool(ctx); err != nil {
		t.Fatalf("a box without a pool must not fail startup: %v", err)
	}
	if res, _ := fresh.List(ctx, ""); len(res.Items) != 0 {
		t.Fatalf("invented a pool: %#v", res.Items)
	}
}

// 每个池带用途（数据池、备份池或无）；每台最多两个池，建第三个在提交时拒绝。
func TestPoolRolesAndCreateLimit(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	stor := &fakePoolStorage{
		poolName: "tank",
		pool:     domain.Pool{ID: "pool-tank2", ServerID: "server-a", Name: "tank2"},
		statusByName: map[string]domain.PoolStatus{
			"tank":  {Name: "tank", Health: "ONLINE"},
			"tank2": {Name: "tank2", Health: "ONLINE"},
		},
	}
	service := PoolService{Store: st, Storage: stor}

	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-tank", ServerID: "server-a", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	// 只有一个池时可以再建一个。
	if _, err := service.Create(ctx, PoolRequest{Name: "tank2", Disks: []string{"/dev/sdc"}}); err != nil {
		t.Fatalf("second pool refused: %v", err)
	}

	// 用途是推导出来的：不是数据池的那个就是备份池。
	stor.backupPool = "tank2"
	list, err := service.List(ctx, "server-a")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, item := range list.Items {
		roles[item.Name] = item.Role
	}
	if roles["tank"] != "data" || roles["tank2"] != "backup" {
		t.Fatalf("roles = %#v", roles)
	}

	// 已有两个池时拒绝第三个，并给出操作者能看懂的提示。
	_, err = service.Create(ctx, PoolRequest{Name: "tank3", Disks: []string{"/dev/sdd"}})
	if err == nil {
		t.Fatal("third pool was allowed")
	}
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("third pool error = %v", err)
	}
}

// 建池成功后必须把新池名交给进程（持久化 + 运行期绑定），否则仍在启动时绑定的 tank 里找镜像。
func TestCreatePoolCallsReloadWithNewPool(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	storage := &fakePoolStorage{
		pool:   domain.Pool{ID: "pool-mypool", ServerID: "server-a", Name: "mypool"},
		status: domain.PoolStatus{Name: "mypool", Health: "ONLINE", Capacity: 1000, Used: 0},
	}
	var reloaded string
	service := PoolService{Store: st, Storage: storage, Now: time.Now,
		Reload: func(_ context.Context, pool string) error { reloaded = pool; return nil }}
	if _, err := service.Create(ctx, PoolRequest{Name: "mypool", Disks: []string{"/dev/sdb"}}); err != nil {
		t.Fatal(err)
	}
	if reloaded != "mypool" {
		t.Fatalf("Reload 收到 %q, want mypool", reloaded)
	}
}

// 池名按 (节点, 池名) 查重：目录库整体复制且集群要求各节点数据池同名，只看名字会被别的节点挡住。
func TestCreatePoolNameUniquePerNodeNotClusterWide(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	// 另一台节点的同名池已在库里（复制过来的）。
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-b", Name: "server-b", IP: "10.0.0.2", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-server-b--ndpool", ServerID: "server-b", Name: "ndpool"}); err != nil {
		t.Fatal(err)
	}
	storage := &fakePoolStorage{
		pool:   domain.Pool{ID: "pool-server-a--ndpool", ServerID: "server-a", Name: "ndpool"},
		status: domain.PoolStatus{Health: "ONLINE", Capacity: 1000},
	}
	service := PoolService{Store: st, Storage: storage, Now: time.Now}
	if _, err := service.Create(ctx, PoolRequest{Name: "ndpool", Disks: []string{"/dev/sdb"}}); err != nil {
		t.Fatalf("别的节点有同名池不该挡住本机建池：%v", err)
	}
	// 本机自己已有同名池才是冲突。
	if _, err := service.Create(ctx, PoolRequest{Name: "ndpool", Disks: []string{"/dev/sdc"}}); !errors.Is(err, ErrPoolExists) {
		t.Fatalf("本机重名应报 ErrPoolExists，得到 %v", err)
	}
}

// 只有本机还没有可用数据池时才绑定新池；已有数据池时再建的池不能顶替它，
// 否则集群下发的池名和镜像导入都会落到备份池。
func TestCreateSecondPoolKeepsExistingDataPool(t *testing.T) {
	ctx := context.Background()

	// 数据池 ndpool 在线：再建 ndbak，不绑定。
	st := newImageTestStore(t)
	storage := &fakePoolStorage{
		poolName: "ndpool",
		pool:     domain.Pool{ID: "pool-ndbak", ServerID: "server-a", Name: "ndbak"},
		status:   domain.PoolStatus{Health: "ONLINE", Capacity: 1000},
	}
	reloaded := ""
	service := PoolService{Store: st, Storage: storage, Now: time.Now,
		Reload: func(_ context.Context, pool string) error { reloaded = pool; return nil }}
	if _, err := service.Create(ctx, PoolRequest{Name: "ndbak", Disks: []string{"/dev/sdc"}}); err != nil {
		t.Fatal(err)
	}
	if reloaded != "" {
		t.Fatalf("已有数据池 ndpool 在线，建 ndbak 却把数据池换成了 %q", reloaded)
	}

	// 配了 tank 但盘上没有（无池装机）：建 mypool 要绑定。
	st2 := newImageTestStore(t)
	storage2 := &fakePoolStorage{
		poolName:        "tank",
		pool:            domain.Pool{ID: "pool-mypool", ServerID: "server-a", Name: "mypool"},
		status:          domain.PoolStatus{Health: "ONLINE", Capacity: 1000},
		statusErrByName: map[string]error{"tank": errors.New("cannot open 'tank': no such pool")},
	}
	reloaded = ""
	service2 := PoolService{Store: st2, Storage: storage2, Now: time.Now,
		Reload: func(_ context.Context, pool string) error { reloaded = pool; return nil }}
	if _, err := service2.Create(ctx, PoolRequest{Name: "mypool", Disks: []string{"/dev/sdb"}}); err != nil {
		t.Fatal(err)
	}
	if reloaded != "mypool" {
		t.Fatalf("无池装机建 mypool 应绑定，Reload 收到 %q", reloaded)
	}
}

// 池已不在但记录还在时要报 MISSING 并清零容量，不能显示 UNKNOWN 加旧容量，
// 那会把不存在的池算进总容量。
func TestPoolListReportsAPoolThatIsGoneFromTheNode(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-data1", Name: "data1", ServerID: "server-a", Capacity: 96 << 30, Used: 39 << 30}); err != nil {
		t.Fatal(err)
	}
	agent := &fakePoolStorage{poolName: "data1", statusErrByName: map[string]error{
		"data1": storage.CommandError{Name: "zpool", Output: "cannot open 'data1': no such pool\n"},
	}}
	svc := PoolService{Store: st, Storage: agent}

	res, err := svc.List(ctx, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("items = %#v, err = %v", res.Items, err)
	}
	got := res.Items[0]
	if got.Health != PoolHealthMissing || got.Capacity != 0 || got.Used != 0 {
		t.Fatalf("消失的池 = health %q capacity %d used %d，应为 %s 且不带旧容量", got.Health, got.Capacity, got.Used, PoolHealthMissing)
	}
	item, err := svc.Get(ctx, "pool-data1")
	if err != nil || item.Health != PoolHealthMissing {
		t.Fatalf("详情也要能打开并报不在了: %#v, err = %v", item, err)
	}
}

// 数据池已不存在时不再拦「正在使用中」，否则记录永远删不掉，同名池也建不回来。
func TestDestroyRemovesTheRecordOfADataPoolThatIsGone(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-data1", Name: "data1", ServerID: "server-a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	agent := &fakePoolStorage{poolName: "data1", statusErrByName: map[string]error{
		"data1": storage.CommandError{Name: "zpool", Output: "cannot open 'data1': no such pool\n"},
	}}

	if _, err := (PoolService{Store: st, Storage: agent}).Destroy(ctx, "pool-data1"); err != nil {
		t.Fatalf("池已不在，销毁应只删记录: %v", err)
	}
	if _, err := st.Pools().Get(ctx, "pool-data1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("记录应已删除: %v", err)
	}
}

// 建池时选用途：每台各一个数据池和备份池，已有的那种不能再建；只有数据池会被绑定。
func TestCreatePoolHonoursTheChosenRole(t *testing.T) {
	ctx := context.Background()
	restarts := 0
	newSvc := func(t *testing.T, rows []domain.Pool, dataPoolOnDisk bool) (PoolService, *string) {
		st := newImageTestStore(t)
		if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			if err := st.Pools().Create(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		agent := &fakePoolStorage{poolName: "data", status: domain.PoolStatus{Health: "ONLINE", Capacity: 1000}}
		if !dataPoolOnDisk {
			agent.statusErrByName = map[string]error{"data": storage.CommandError{Name: "zpool", Output: "cannot open 'data': no such pool"}}
		}
		reloaded := new(string)
		return PoolService{Store: st, Storage: agent, Now: time.Now,
			Reload:  func(_ context.Context, pool string) error { *reloaded = pool; return nil },
			Restart: func() { restarts++ }}, reloaded
	}
	dataRow := domain.Pool{ID: "pool-data", ServerID: "server-a", Name: "data"}
	bakRow := domain.Pool{ID: "pool-bak", ServerID: "server-a", Name: "bak"}

	svc, _ := newSvc(t, []domain.Pool{dataRow}, true)
	if _, err := svc.Create(ctx, PoolRequest{Name: "data2", Role: PoolRoleData, Disks: []string{"/dev/sdc"}}); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "data") {
		t.Fatalf("已有数据池再建数据池应被拒并点名: %v", err)
	}
	svc, _ = newSvc(t, []domain.Pool{dataRow, bakRow}, true)
	if _, err := svc.Create(ctx, PoolRequest{Name: "bak2", Role: PoolRoleBackup, Disks: []string{"/dev/sdc"}}); !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "bak") {
		t.Fatalf("两种都有时应被拒并点名: %v", err)
	}
	svc, _ = newSvc(t, []domain.Pool{bakRow}, false)
	if _, err := svc.Create(ctx, PoolRequest{Name: "bak2", Role: PoolRoleBackup, Disks: []string{"/dev/sdc"}}); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "bak") {
		t.Fatalf("已有备份池再建备份池应被拒并点名: %v", err)
	}
	svc, _ = newSvc(t, nil, false)
	if _, err := svc.Create(ctx, PoolRequest{Name: "x", Role: "cache", Disks: []string{"/dev/sdc"}}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("未知类型应被拒: %v", err)
	}
	if _, err := svc.Create(ctx, PoolRequest{Name: "data", Role: PoolRoleBackup, Disks: []string{"/dev/sdc"}}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("备份池用了本机数据池的名字应被拒: %v", err)
	}

	// 无池的节点先建备份池：不能把它绑成数据池
	svc, reloaded := newSvc(t, nil, false)
	if _, err := svc.Create(ctx, PoolRequest{Name: "bak", Role: PoolRoleBackup, Disks: []string{"/dev/sdb"}}); err != nil {
		t.Fatal(err)
	}
	if *reloaded != "" || restarts != 0 {
		t.Fatalf("建备份池不该绑定为数据池、也不该重启，Reload 收到 %q，重启 %d 次", *reloaded, restarts)
	}
	// 同一节点再建数据池：绑定
	if _, err := svc.Create(ctx, PoolRequest{Name: "data", Role: PoolRoleData, Disks: []string{"/dev/sdc"}}); err != nil {
		t.Fatal(err)
	}
	if *reloaded != "data" {
		t.Fatalf("建数据池应绑定，Reload 收到 %q", *reloaded)
	}
	// 复制组件在启动时按池名建好，绑定新数据池后要重启一次
	if restarts != 1 {
		t.Fatalf("绑定新数据池后应重启一次，实际 %d 次", restarts)
	}
}

// 池记录属于别的节点时，写操作必须拒绝，不能按池名动本机的同名池（会销毁本机的 backup 池）。
func TestPoolWritesRefuseAnotherNodesPool(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seen := time.Now().UTC()
	for _, s := range []domain.Server{
		{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "server-b", Name: "server-b", IP: "10.0.0.5", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &seen},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-b-backup", Name: "backup", ServerID: "server-b"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakePoolStorage{poolName: "tank"}
	svc := PoolService{Store: st, Storage: fake, Async: false}

	_, err := svc.Destroy(ctx, "pool-b-backup")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "10.0.0.5") {
		t.Fatalf("err = %v，应拒绝并点名池所在的节点", err)
	}
	if fake.destroyedName != "" {
		t.Fatalf("本机的同名池被销毁了：%q", fake.destroyedName)
	}
	if _, err := svc.AddDisk(ctx, "pool-b-backup", PoolDiskRequest{Disks: []string{"/dev/sdz"}}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("加盘 err = %v", err)
	}
	if fake.addName != "" {
		t.Fatalf("本机的同名池被加了盘：%q", fake.addName)
	}
	if _, err := st.Pools().Get(ctx, "pool-b-backup"); err != nil {
		t.Fatalf("被拒时记录应原样保留：%v", err)
	}
}

// 节点已离线（重装、报废）时销毁只删记录、不碰磁盘，否则「移出集群」会卡在「名下还有存储池」。

func TestDestroyingAnOfflineNodesPoolOnlyDropsTheRecord(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	stale := time.Now().UTC().Add(-time.Hour)
	for _, s := range []domain.Server{
		{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "server-b", Name: "server-b", IP: "10.0.0.5", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &stale},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-b-backup", Name: "backup", ServerID: "server-b"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakePoolStorage{poolName: "tank"}
	if _, err := (PoolService{Store: st, Storage: fake, Async: false}).Destroy(ctx, "pool-b-backup"); err != nil {
		t.Fatal(err)
	}
	if fake.destroyedName != "" {
		t.Fatalf("动了本机的磁盘：%q", fake.destroyedName)
	}
	if _, err := st.Pools().Get(ctx, "pool-b-backup"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("离线节点的池记录应已清掉：%v", err)
	}
}

// 集群里数据池常同名：别的节点的池记录不能按池名读本机状态再写回，否则写入者的容量和磁盘会盖到别人的记录上。
func TestPoolListAndGetLeaveAnotherNodesRecordAlone(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	for _, s := range []domain.Server{
		{ID: "server-a", Name: "server-a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "server-b", Name: "server-b", IP: "10.0.0.5", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
	} {
		if err := st.Servers().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []domain.Pool{
		{ID: "pool-a", ServerID: "server-a", Name: "data", Capacity: 1, Used: 1},
		{ID: "pool-b", ServerID: "server-b", Name: "data", Capacity: 2048, Used: 700, Disks: []string{"/dev/sdb"}},
	} {
		if err := st.Pools().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	svc := PoolService{Store: st, Storage: &fakePoolStorage{statusByName: map[string]domain.PoolStatus{
		"data": {Health: "ONLINE", Capacity: 1024, Used: 100, Disks: []domain.PoolDiskStatus{{Path: "/dev/sda", Role: domain.PoolDiskRoleData, Status: "ONLINE"}}},
	}}}

	got, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range got.Items {
		switch item.ID {
		case "pool-a":
			if item.Capacity != 1024 || item.Health != "ONLINE" {
				t.Fatalf("本机池应读实时状态：%#v", item)
			}
		case "pool-b":
			if item.Capacity != 2048 || item.Used != 700 || item.Health != poolHealthUnknown {
				t.Fatalf("别的节点的池应原样返回、健康未知：%#v", item)
			}
		}
	}
	item, err := svc.Get(ctx, "pool-b")
	if err != nil {
		t.Fatal(err)
	}
	if item.Capacity != 2048 || item.Health != poolHealthUnknown {
		t.Fatalf("Get 别的节点的池 = %#v", item)
	}
	pool, err := st.Pools().Get(ctx, "pool-b")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Capacity != 2048 || pool.Used != 700 || len(pool.Disks) != 1 || pool.Disks[0] != "/dev/sdb" {
		t.Fatalf("别的节点的池记录被本机状态改写：%#v", pool)
	}
}
