package assets

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
	"github.com/tianwei/diskless/internal/xlsx"
)

type superLockFixture struct {
	ctx          context.Context
	st           store.Store
	group        domain.Group
	super, plain domain.Terminal
	disk         domain.GroupDisk
	terminals    TerminalService
	groups       GroupService
	disks        GroupDiskService
	reductions   ReductionService
	configs      ConfigService
}

func newSuperLockFixture(t *testing.T) superLockFixture {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 10)
	seedExtraGroup(t, ctx, st, "grp-2", "二区", "192.168.1.100", 10)
	now := time.Now().UTC()
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-2", ConfigID: "cfg-1", Name: "@r2", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	super := domain.Terminal{ID: "terminal-AABBCCDDEE01", MAC: "AABBCCDDEE01", Name: "主播-01", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true, State: domain.TerminalStateOffline}
	plain := domain.Terminal{ID: "terminal-AABBCCDDEE02", MAC: "AABBCCDDEE02", Name: "散座-01", IP: "192.168.1.11", GroupID: group.ID, State: domain.TerminalStateOffline}
	for _, term := range []domain.Terminal{super, plain} {
		if err := st.Terminals().Create(ctx, term); err != nil {
			t.Fatal(err)
		}
	}
	disk := seedDataDisk(t, ctx, st, group, "gd-1", "img-data", "cfg-data", "D:")
	return superLockFixture{ctx: ctx, st: st, group: group, super: super, plain: plain, disk: disk,
		terminals: TerminalService{Store: st}, groups: newGroupService(st), disks: GroupDiskService{Store: st},
		reductions: ReductionService{Store: st, Storage: &fakeReductionStorage{}},
		configs:    ConfigService{Store: st, Storage: &fakeConfigStorage{}}}
}

func requireSuperLocked(t *testing.T, err error, who string) {
	t.Helper()
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "请先取消超管机「"+who+"」的超管，再") || !strings.Contains(err.Error(), "（要保留改动，先关机后点「关机后存还原点」）") {
		t.Fatalf("应拒绝并点名超管机，err = %v", err)
	}
}

// 超管模式开着时锁住机器换组、所在分组和所占配置，不依赖是否有未保存的修改。
func TestSuperModeLocksItsGroupAndConfigs(t *testing.T) {
	cases := []struct {
		name string
		run  func(superLockFixture) error
	}{
		{"超管机换分组", func(f superLockFixture) error {
			_, err := f.terminals.Update(f.ctx, f.super.ID, TerminalRequest{MAC: f.super.MAC, IP: "192.168.1.100", GroupID: "grp-2", Name: f.super.Name, IsSuper: true})
			return err
		}},
		{"批量移动带上超管机", func(f superLockFixture) error {
			_, err := f.terminals.Move(f.ctx, MoveTerminalsRequest{TerminalIDs: []string{f.plain.ID, f.super.ID}, GroupID: "grp-2"})
			return err
		}},
		{"修改分组", func(f superLockFixture) error {
			_, err := f.groups.Update(f.ctx, f.group.ID, GroupRequest{Name: "改名", IsDefault: f.group.IsDefault, StartIP: f.group.StartIP, ClientMax: f.group.ClientMax, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: "cfg-1"})
			return err
		}},
		{"换组同时取消超管", func(f superLockFixture) error {
			_, err := f.terminals.Update(f.ctx, f.super.ID, TerminalRequest{MAC: f.super.MAC, IP: "192.168.1.100", GroupID: "grp-2", Name: f.super.Name})
			return err
		}},
		{"删除分组", func(f superLockFixture) error { return f.groups.Delete(f.ctx, f.group.ID) }},
		{"删除数据盘", func(f superLockFixture) error { return f.disks.Delete(f.ctx, f.disk.ID) }},
		{"加数据盘", func(f superLockFixture) error {
			_, err := f.disks.Create(f.ctx, f.group.ID, GroupDiskRequest{MountTarget: "E:", ImageID: "img-data", ConfigID: "cfg-data"})
			return err
		}},
		{"应用还原点", func(f superLockFixture) error { _, err := f.reductions.SetCurrent(f.ctx, "red-2"); return err }},
		{"删除还原点", func(f superLockFixture) error { _, err := f.reductions.Delete(f.ctx, "red-2"); return err }},
		{"合并还原点", func(f superLockFixture) error {
			_, err := f.reductions.Merge(f.ctx, "cfg-1", MergeReductionsRequest{KeepReductionID: "red-2", DeleteReductionIDs: []string{"red-1"}})
			return err
		}},
		{"新建还原点", func(f superLockFixture) error {
			_, err := f.reductions.Create(f.ctx, "cfg-1", CreateReductionRequest{Name: "新点"})
			return err
		}},
		{"合并配置", func(f superLockFixture) error { _, err := f.configs.Merge(f.ctx, "cfg-1"); return err }},
		{"删除配置", func(f superLockFixture) error { _, err := f.configs.Delete(f.ctx, "cfg-1"); return err }},
		{"覆盖原镜像", func(f superLockFixture) error { _, err := f.configs.OverwriteImage(f.ctx, "red-2"); return err }},
		{"删除镜像", func(f superLockFixture) error { return (ImageService{Store: f.st}).DeleteImage(f.ctx, "img-1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperLockFixture(t)
			if strings.Contains(tc.name, "批量移动") {
				if err := f.st.Servers().Create(f.ctx, domain.Server{ID: "srv-test", Name: "test", IP: "192.168.1.2", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
					t.Fatal(err)
				}
				for i, term := range []domain.Terminal{f.super, f.plain} {
					if err := f.st.ClientClones().Create(f.ctx, domain.ClientClone{ID: []string{"super-clone", "plain-clone"}[i], TerminalMAC: term.MAC, Kind: domain.CloneKindPersistent, ConfigID: "cfg-1", ReductionID: "red-1", ServerID: "srv-test", Target: "test-target", VolPath: "/dev/zvol/test/clone"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := superLockState(t, f)
			requireSuperLocked(t, tc.run(f), f.super.Name)
			if after := superLockState(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("拒绝操作后不应改变客户机、分组、配置、还原点或创建任务")
			}
		})
	}
}

func TestSuperModeCannotStartDuringConfigMutation(t *testing.T) {
	f := newSuperLockFixture(t)
	if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
		t.Fatal(err)
	}
	slow := &blockingCreate{fakeReductionStorage: &fakeReductionStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	creating := ReductionService{Store: f.st, Storage: slow, Async: true}
	task, err := creating.Create(f.ctx, "cfg-1", CreateReductionRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(slow.release); waitTaskDone(t, f.st, task.TaskID) })
	select {
	case <-slow.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("还原点任务没有进入存储边界")
	}
	_, err = f.terminals.EnableSuper(f.ctx, f.super.ID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "还原点") {
		t.Fatalf("配置正在改写时应拒绝启用超管并提示等待，err=%v", err)
	}
}

// 比较操作者可见的目录状态，拒绝必须发生在任务创建和任何持久化修改之前。
func superLockState(t *testing.T, f superLockFixture) []any {
	t.Helper()
	terminals, e1 := f.st.Terminals().List(f.ctx)
	groups, e2 := f.st.Groups().List(f.ctx)
	disks, e3 := f.st.GroupDisks().List(f.ctx)
	configs, e4 := f.st.Configs().List(f.ctx)
	reductions, e5 := f.st.Reductions().List(f.ctx)
	tasks, e6 := f.st.Tasks().List(f.ctx)
	clones, e7 := f.st.ClientClones().List(f.ctx)
	for _, err := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return []any{terminals, groups, disks, configs, reductions, tasks, clones}
}

func TestSuperModeLocksDataConfigAcrossGroups(t *testing.T) {
	cases := []struct {
		name string
		run  func(superLockFixture) error
	}{
		{"新建", func(f superLockFixture) error {
			_, err := f.reductions.Create(f.ctx, "cfg-data", CreateReductionRequest{Name: "data-next"})
			return err
		}},
		{"应用", func(f superLockFixture) error { _, err := f.reductions.SetCurrent(f.ctx, "data-next"); return err }},
		{"删除", func(f superLockFixture) error { _, err := f.reductions.Delete(f.ctx, "data-next"); return err }},
		{"合并还原点", func(f superLockFixture) error {
			_, err := f.reductions.Merge(f.ctx, "cfg-data", MergeReductionsRequest{KeepReductionID: "data-next", DeleteReductionIDs: []string{"cfg-data_0"}})
			return err
		}},
		{"删除配置", func(f superLockFixture) error { _, err := f.configs.Delete(f.ctx, "cfg-data"); return err }},
		{"合并配置", func(f superLockFixture) error { _, err := f.configs.Merge(f.ctx, "cfg-data"); return err }},
		{"覆盖镜像", func(f superLockFixture) error { _, err := f.configs.OverwriteImage(f.ctx, "data-next"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperLockFixture(t)
			if err := f.st.Reductions().Create(f.ctx, domain.Reduction{ID: "data-next", ConfigID: "cfg-data", Name: "@next", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}); err != nil {
				t.Fatal(err)
			}
			// 另一个组也用同一数据配置，锁的是配置占用，而不是调用方的组。
			if err := f.st.GroupDisks().Create(f.ctx, domain.GroupDisk{ID: "gd-shared", GroupID: "grp-2", ConfigID: "cfg-data", ImageID: "img-data", MountTarget: "D:"}); err != nil {
				t.Fatal(err)
			}
			before := superLockState(t, f)
			requireSuperLocked(t, tc.run(f), f.super.Name)
			if !reflect.DeepEqual(before, superLockState(t, f)) {
				t.Fatal("数据配置被拒绝操作仍有修改")
			}
		})
	}
}

func TestSuperModeLocksSiblingConfigsAffectedByMerge(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "合并", true: "覆盖"}[overwrite], func(t *testing.T) {
			f := newSuperLockFixture(t)
			seedSiblingConfig(t, f.ctx, f.st, "img-1")
			red := domain.Reduction{ID: "sibling-red", ConfigID: "img-1-sibling", Name: "@0", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}
			if err := f.st.Reductions().Create(f.ctx, red); err != nil {
				t.Fatal(err)
			}
			before := superLockState(t, f)
			var err error
			if overwrite {
				_, err = f.configs.OverwriteImage(f.ctx, red.ID)
			} else {
				_, err = f.configs.Merge(f.ctx, red.ConfigID)
			}
			requireSuperLocked(t, err, f.super.Name)
			if !reflect.DeepEqual(before, superLockState(t, f)) {
				t.Fatal("不能删改超管占用的兄弟配置")
			}
		})
	}
}

// 默认标志只决定新机器归哪组，删除默认组时转给超管机所在组不受超管锁定影响。
func TestSuperModeAllowsDefaultTransferOnGroupDelete(t *testing.T) {
	f := newSuperLockFixture(t)
	f.super.GroupID, f.super.IP = "grp-2", "192.168.1.100"
	if err := f.st.Terminals().Update(f.ctx, f.super); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Terminals().Delete(f.ctx, f.plain.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.groups.Delete(f.ctx, f.group.ID); err != nil {
		t.Fatalf("删除默认组应可用：%v", err)
	}
	next, err := f.st.Groups().Get(f.ctx, "grp-2")
	if err != nil {
		t.Fatal(err)
	}
	if !next.IsDefault {
		t.Fatal("默认应转给剩下的分组")
	}
}

func TestSuperModeCannotEscapeByChangingGroupConfig(t *testing.T) {
	f := newSuperLockFixture(t)
	seedGroupTriple(t, f.ctx, f.st, "img-other", "cfg-other", "red-other")
	_, err := f.groups.Update(f.ctx, f.group.ID, GroupRequest{Name: f.group.Name, IsDefault: true, StartIP: f.group.StartIP, ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-other", SystemConfigID: "cfg-other"})
	requireSuperLocked(t, err, f.super.Name)
}

func TestSuperModeAllowsIndependentChangesAndClosing(t *testing.T) {
	cases := []struct {
		name string
		run  func(superLockFixture) error
	}{
		{"同组普通机改名", func(f superLockFixture) error {
			_, err := f.terminals.Update(f.ctx, f.plain.ID, TerminalRequest{MAC: f.plain.MAC, IP: f.plain.IP, GroupID: f.group.ID, Name: "普通机新名"})
			return err
		}},
		{"同组普通机换组", func(f superLockFixture) error {
			_, err := f.terminals.Move(f.ctx, MoveTerminalsRequest{TerminalIDs: []string{f.plain.ID}, GroupID: "grp-2"})
			return err
		}},
		{"无关组修改", func(f superLockFixture) error {
			_, err := f.groups.Update(f.ctx, "grp-2", GroupRequest{Name: "其他组新名", StartIP: "192.168.1.100", ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: "cfg-1"})
			return err
		}},
		{"无关组新建", func(f superLockFixture) error {
			_, err := f.groups.Create(f.ctx, GroupRequest{Name: "三区", StartIP: "192.168.1.200", ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: "cfg-1"})
			return err
		}},
		{"无关组加数据盘", func(f superLockFixture) error {
			_, err := f.disks.Create(f.ctx, "grp-2", GroupDiskRequest{MountTarget: "D:", ImageID: "img-data", ConfigID: "cfg-data"})
			return err
		}},
		{"读取配置", func(f superLockFixture) error { _, err := f.configs.List(f.ctx, "img-1"); return err }},
		{"从镜像另存", func(f superLockFixture) error {
			_, err := f.configs.CreateFromImage(f.ctx, "img-1", CreateConfigRequest{Name: "另存"})
			return err
		}},
		{"从配置另存", func(f superLockFixture) error {
			_, err := f.configs.CreateFromConfig(f.ctx, "cfg-1", CreateConfigFromConfigRequest{Name: "另存", ReductionID: "red-1"})
			return err
		}},
		{"把占用组设为默认", func(f superLockFixture) error { _, err := f.groups.SetDefault(f.ctx, f.group.ID); return err }},
		{"把别的组设为默认", func(f superLockFixture) error { _, err := f.groups.SetDefault(f.ctx, "grp-2"); return err }},
		{"新建默认组", func(f superLockFixture) error {
			_, err := f.groups.Create(f.ctx, GroupRequest{Name: "三区", IsDefault: true, StartIP: "192.168.1.200", ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: "cfg-1"})
			return err
		}},
		{"改别的组为默认", func(f superLockFixture) error {
			_, err := f.groups.Update(f.ctx, "grp-2", GroupRequest{Name: "二区", IsDefault: true, StartIP: "192.168.1.100", ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: "cfg-1"})
			return err
		}},
		{"批量移动到超管机本来所在的组", func(f superLockFixture) error {
			_, err := f.terminals.Move(f.ctx, MoveTerminalsRequest{TerminalIDs: []string{f.plain.ID, f.super.ID}, GroupID: f.group.ID})
			return err
		}},
		{"编辑表单只切换默认", func(f superLockFixture) error {
			if _, err := f.groups.SetDefault(f.ctx, "grp-2"); err != nil {
				return err
			}
			g, err := f.st.Groups().Get(f.ctx, f.group.ID)
			if err != nil {
				return err
			}
			_, err = f.groups.Update(f.ctx, g.ID, GroupRequest{Name: g.Name, IsDefault: true, StartIP: g.StartIP, ClientMax: g.ClientMax,
				Gateway: g.Gateway, Netmask: g.Netmask, DNS1: g.DNS1, DNS2: g.DNS2, SystemImageID: g.SystemImageID, SystemConfigID: g.SystemConfigID})
			return err
		}},
		{"关闭超管", func(f superLockFixture) error { _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); return err }},
		{"停机保存", func(f superLockFixture) error {
			f.terminals.Storage = &fakeSuperStopStorage{reduction: domain.Reduction{ID: "saved", ConfigID: "cfg-1", Name: "@saved", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}}
			_, err := f.terminals.StopSuper(f.ctx, f.super.ID, SuperStopRequest{ReductionName: "saved"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperLockFixture(t)
			if err := tc.run(f); err != nil {
				t.Fatalf("独立操作或明确关闭流程应可用：%v", err)
			}
		})
	}
}

func TestSuperModeRefusalUsesMACWithoutName(t *testing.T) {
	f := newSuperLockFixture(t)
	f.super.Name = "  "
	if err := f.st.Terminals().Update(f.ctx, f.super); err != nil {
		t.Fatal(err)
	}
	_, err := f.reductions.SetCurrent(f.ctx, "red-2")
	requireSuperLocked(t, err, "AA:BB:CC:DD:EE:01")
	if strings.Contains(err.Error(), f.super.ID) {
		t.Fatal("拒绝提示不应暴露内部ID")
	}
}

func TestSuperModeCanStartDuringUnrelatedConfigMutation(t *testing.T) {
	f := newSuperLockFixture(t)
	if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
		t.Fatal(err)
	}
	seedSiblingConfig(t, f.ctx, f.st, "img-1")
	red := domain.Reduction{ID: "sibling-red", ConfigID: "img-1-sibling", Name: "@0", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}
	if err := f.st.Reductions().Create(f.ctx, red); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.st.Configs().Get(f.ctx, red.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultReductionID = &red.ID
	if err := f.st.Configs().Update(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.groups.Update(f.ctx, f.group.ID, GroupRequest{Name: f.group.Name, IsDefault: true, StartIP: f.group.StartIP, ClientMax: 10, Gateway: f.group.Gateway, Netmask: f.group.Netmask, SystemImageID: "img-1", SystemConfigID: cfg.ID}); err != nil {
		t.Fatal(err)
	}
	slow := &blockingCreate{fakeReductionStorage: &fakeReductionStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	task, err := (ReductionService{Store: f.st, Storage: slow, Async: true}).Create(f.ctx, "cfg-1", CreateReductionRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(slow.release); waitTaskDone(t, f.st, task.TaskID) })
	select {
	case <-slow.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未进入存储边界")
	}
	if _, err := f.terminals.EnableSuper(f.ctx, f.super.ID); err != nil {
		t.Fatalf("同镜像内无关配置的任务不应阻止超管：%v", err)
	}
}

// Merge/Overwrite 会修改整个镜像的配置，pending guard 不能把它当成只读另存。
type blockingSuperLockMerge struct {
	*fakeConfigStorage
	entered, release chan struct{}
}

func (s *blockingSuperLockMerge) MergeConfig(ctx context.Context, req storage.MergeConfigReq) (storage.MergeConfigResult, error) {
	close(s.entered)
	<-s.release
	return s.fakeConfigStorage.MergeConfig(ctx, req)
}

func TestSuperModeCannotStartDuringWholeImageMutation(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "合并", true: "覆盖"}[overwrite], func(t *testing.T) {
			f := newSuperLockFixture(t)
			if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reductions.SetCurrent(f.ctx, "red-2"); err != nil {
				t.Fatal(err)
			}
			slow := &blockingSuperLockMerge{fakeConfigStorage: &fakeConfigStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
			merging := ConfigService{Store: f.st, Storage: slow, Async: true}
			var task ConfigTaskResult
			var err error
			if overwrite {
				task, err = merging.OverwriteImage(f.ctx, "red-2")
			} else {
				task, err = merging.Merge(f.ctx, "cfg-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { close(slow.release); waitTaskDone(t, f.st, task.TaskID) })
			select {
			case <-slow.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("任务未进入存储边界")
			}
			_, err = f.terminals.EnableSuper(f.ctx, f.super.ID)
			if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "任务完成") {
				t.Fatalf("整镜像改写期间应拒绝启用超管：%v", err)
			}
		})
	}
}

func TestSuperModeCanStartDuringSaveAs(t *testing.T) {
	for _, fromConfig := range []bool{false, true} {
		t.Run(map[bool]string{false: "从镜像", true: "从配置"}[fromConfig], func(t *testing.T) {
			f := newSuperLockFixture(t)
			if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
				t.Fatal(err)
			}
			slow := &fakeConfigStorage{startedCreate: make(chan struct{}), blockCreate: make(chan struct{})}
			entered := slow.startedCreate
			copying := ConfigService{Store: f.st, Storage: slow, Async: true}
			var task ConfigOperationResult
			var err error
			if fromConfig {
				task, err = copying.CreateFromConfig(f.ctx, "cfg-1", CreateConfigFromConfigRequest{Name: "copy", ReductionID: "red-1"})
			} else {
				task, err = copying.CreateFromImage(f.ctx, "img-1", CreateConfigRequest{Name: "copy"})
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { close(slow.blockCreate); waitTaskDone(t, f.st, task.TaskID) })
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("另存未进入存储边界")
			}
			if _, err := f.terminals.EnableSuper(f.ctx, f.super.ID); err != nil {
				t.Fatalf("只读来源的另存不应阻止启用超管：%v", err)
			}
		})
	}
}

// 慢物理删除只占其镜像的claim，不能阻塞其它组正常启用超管。
type blockingSuperLockImageDelete struct {
	*fakeImageStorage
	entered, release chan struct{}
}

func (s *blockingSuperLockImageDelete) DeleteImage(ctx context.Context, imageID string, configIDs []string) error {
	close(s.entered)
	<-s.release
	return s.fakeImageStorage.DeleteImage(ctx, imageID, configIDs)
}

func TestSuperModeCanStartWhileUnrelatedImageDeletionIsSlow(t *testing.T) {
	f := newSuperLockFixture(t)
	if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
		t.Fatal(err)
	}
	seedGroupTriple(t, f.ctx, f.st, "img-unused", "cfg-unused", "red-unused")
	slow := &blockingSuperLockImageDelete{fakeImageStorage: &fakeImageStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	deleted, enabled := make(chan error, 1), make(chan error, 1)
	enablePending := false
	go func() { deleted <- (ImageService{Store: f.st, Storage: slow}).DeleteImage(f.ctx, "img-unused") }()
	t.Cleanup(func() {
		close(slow.release)
		select {
		case err := <-deleted:
			if err != nil {
				t.Errorf("无关镜像删除失败：%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("删除没有退出存储边界")
		}
		if enablePending {
			select {
			case <-enabled:
			case <-time.After(3 * time.Second):
				t.Error("启用没有结束")
			}
		}
	})
	select {
	case <-slow.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("删除未进入存储边界")
	}
	enablePending = true
	go func() { _, err := f.terminals.EnableSuper(f.ctx, f.super.ID); enabled <- err }()
	select {
	case err := <-enabled:
		enablePending = false
		if err != nil {
			t.Fatalf("无关镜像删除不应阻止启用超管：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("启用被无关镜像的慢删除阻塞")
	}
}

func TestSuperModeCannotBeImportedDuringConfigMutation(t *testing.T) {
	f := newSuperLockFixture(t)
	if _, err := f.terminals.DisableSuper(f.ctx, f.super.ID); err != nil {
		t.Fatal(err)
	}
	slow := &blockingCreate{fakeReductionStorage: &fakeReductionStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	task, err := (ReductionService{Store: f.st, Storage: slow, Async: true}).Create(f.ctx, "cfg-1", CreateReductionRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(slow.release); waitTaskDone(t, f.st, task.TaskID) })
	select {
	case <-slow.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未进入存储边界")
	}
	data, err := xlsx.Write([][]string{
		terminalImportHeader,
		{"00:11:22:33:44:66", "192.168.1.12", "普通导入", f.group.ID, "false", "offline"},
		{"00:11:22:33:44:77", "192.168.1.13", "超管导入", f.group.ID, "true", "offline"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := superLockState(t, f)
	result, err := f.terminals.Import(f.ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || len(result.Errors) != 1 || result.Errors[0].Row != 3 || !strings.Contains(result.Errors[0].Error, "任务完成") {
		t.Fatalf("配置改写时超管行应提示等待，整份不提交：%#v", result)
	}
	if !reflect.DeepEqual(before, superLockState(t, f)) {
		t.Fatal("不能部分提交普通导入行")
	}
}

type hookedSuperStopStorage struct {
	*fakeSuperStopStorage
	during func()
}

func (s hookedSuperStopStorage) SuperStop(ctx context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	s.during()
	return s.fakeSuperStopStorage.SuperStop(ctx, req)
}

// 保存要等机器关机，可能要一两分钟；期间对终端的改动（如改名）不能被保存结束时写回的旧记录盖掉。
func TestSuperSaveKeepsChangesMadeWhileWaiting(t *testing.T) {
	f := newSuperLockFixture(t)
	bundle, at := "bundle-1", time.Now().UTC()
	f.super.PendingBundleID, f.super.PendingBundleAt = &bundle, &at
	if err := f.st.Terminals().Update(f.ctx, f.super); err != nil {
		t.Fatal(err)
	}
	f.terminals.Storage = hookedSuperStopStorage{
		fakeSuperStopStorage: &fakeSuperStopStorage{reduction: domain.Reduction{ID: "saved", ConfigID: "cfg-1", Name: "@saved", CreatedAt: at, Status: domain.ReductionStatusReady}},
		during: func() {
			cur, _ := f.st.Terminals().Get(f.ctx, f.super.ID)
			cur.Name = "主播-01-改名"
			_ = f.st.Terminals().Update(f.ctx, cur)
		},
	}
	if _, err := f.terminals.StopSuper(f.ctx, f.super.ID, SuperStopRequest{ReductionName: "saved"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Terminals().Get(f.ctx, f.super.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "主播-01-改名" {
		t.Fatalf("保存把等待期间的改动盖掉了：%+v", got)
	}
	if got.PendingBundleID != nil {
		t.Fatalf("驱动包应随保存固化：%+v", got)
	}
}
