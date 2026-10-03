package assets

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestReductionServiceCreate(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	storage := &fakeReductionStorage{}

	result, err := (ReductionService{Store: st, Storage: storage}).Create(ctx, "cfg-1", CreateReductionRequest{Name: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || result.Reduction.Name != "@r1" {
		t.Fatalf("result = %#v", result)
	}
	if storage.createdConfig != "cfg-1" || storage.createdName != "@r1" {
		t.Fatalf("storage = %#v", storage)
	}
	if _, err := st.Reductions().Get(ctx, result.Reduction.ID); err != nil {
		t.Fatal(err)
	}
}

// 操作者输入任意名字（含中文、空格），交给 ZFS 的快照名都必须是它能接受的。
func TestReductionServiceAcceptsAnyNameAndHandsZFSASafeOne(t *testing.T) {
	ctx := context.Background()
	safe := regexp.MustCompile(`^@[A-Za-z0-9._:-]+$`)
	for _, name := range []string{"r1", "2026.07.28", "v1-0", "snap_2", "a:b",
		"还原点", "a b", "a/b", "a@b", "a+b", "装完office"} {
		st := seedReductionStore(t)
		agent := &fakeReductionStorage{}
		result, err := (ReductionService{Store: st, Storage: agent}).Create(ctx, "cfg-1", CreateReductionRequest{Name: name})
		if err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
		if !safe.MatchString(agent.createdName) {
			t.Fatalf("name %q reached ZFS as %q", name, agent.createdName)
		}
		if result.Reduction.DisplayName != name {
			t.Fatalf("name %q shown as %q", name, result.Reduction.DisplayName)
		}
	}
}

// 开头的「@」是快照分隔符：输入「@r1」指的就是 r1，不能另建一个。
func TestReductionServiceTreatsALeadingAtAsTheSeparator(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	result, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "@r1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reduction.DisplayName != "r1" {
		t.Fatalf("display name = %q", result.Reduction.DisplayName)
	}
	// 基线是「@0」，输入「@0」应判为重名。
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "@0"}); !errors.Is(err, ErrReductionExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestReductionServiceRejectsEmptyAndOverlongNames(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "   "}); !errors.Is(err, ErrReductionNameRequired) {
		t.Fatalf("blank name: err = %v", err)
	}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: strings.Repeat("装", 61)}); !errors.Is(err, ErrReductionNameTooLong) {
		t.Fatalf("overlong name: err = %v", err)
	}
}

// 还原点 ID 由小写名派生，「r1」之后建「R1」须判重名，而不是插入时报 UNIQUE 约束错误。
func TestReductionServiceCreateRejectsDuplicateIgnoringCase(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "R1"}); !errors.Is(err, ErrReductionExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestReductionServiceDeleteRejectsInUse(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	if err := st.Groups().Create(ctx, domain.Group{ID: "g-1", Name: "group", StartIP: "10.0.0.1", ClientMax: 10, Gateway: "10.0.0.254", Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-2"}); err != nil {
		t.Fatal(err)
	}

	_, err := (ReductionService{Store: st, Storage: &fakeReductionStorage{}}).Delete(ctx, "red-2")
	if !errors.Is(err, ErrReductionInUse) {
		t.Fatalf("err = %v", err)
	}
}

func TestReductionServiceDeleteRepairsDefault(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	// 快照丢失的当前点删掉后，指针要落到仍存在的点上。
	storage := &fakeReductionStorage{missingSnapshots: map[string]bool{"0": true}}

	result, err := (ReductionService{Store: st, Storage: storage}).Delete(ctx, "red-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || storage.deletedConfig != "cfg-1" || storage.deletedName != "@0" {
		t.Fatalf("result=%#v storage=%#v", result, storage)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "red-2" {
		t.Fatalf("default = %#v", cfg.DefaultReductionID)
	}
	if _, err := st.Reductions().Get(ctx, "red-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("red-1 err = %v", err)
	}
}

// 应用还原点会移动配置指针，跟随该配置的分组一起切换，其它配置的分组不动；对旧点再应用即回滚。
func TestReductionServiceSetCurrentRetargetsFollowingGroups(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	now := time.Now().UTC()
	// 本配置两个分组，另一配置一个分组（不应移动）。
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-2", ImageID: "img-1", Name: "other", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-o", ConfigID: "cfg-2", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []domain.Group{
		{ID: "g-follow", Name: "follow", StartIP: "192.168.1.10", ClientMax: 10, Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-1"},
		{ID: "g-pinned", Name: "other", StartIP: "192.168.2.10", ClientMax: 10, Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-2", SystemReductionID: "red-o"},
	} {
		if err := st.Groups().Create(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	_ = now
	service := ReductionService{Store: st, Storage: &fakeReductionStorage{}}

	res, err := service.SetCurrent(ctx, "red-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfigID != "cfg-1" || res.ReductionID != "red-2" {
		t.Fatalf("result = %#v", res)
	}
	cfg, _ := st.Configs().Get(ctx, "cfg-1")
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "red-2" {
		t.Fatalf("config current = %v", cfg.DefaultReductionID)
	}
	follow, _ := st.Groups().Get(ctx, "g-follow")
	pinned, _ := st.Groups().Get(ctx, "g-pinned")
	if follow.SystemReductionID != "red-2" {
		t.Fatalf("group on cfg-1 = %#v, want retargeted to red-2", follow)
	}
	if pinned.SystemReductionID != "red-o" {
		t.Fatalf("group on another config = %#v, want left alone", pinned)
	}
	// 回滚：反方向再应用一次。
	if _, err := service.SetCurrent(ctx, "red-1"); err != nil {
		t.Fatal(err)
	}
	follow, _ = st.Groups().Get(ctx, "g-follow")
	if follow.SystemReductionID != "red-1" {
		t.Fatalf("rollback: following group = %#v", follow)
	}
	// 只有就绪的还原点能应用。
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-3", ConfigID: "cfg-1", Name: "@3", CreatedAt: now, Status: domain.ReductionStatusCreating}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetCurrent(ctx, "red-3"); err == nil {
		t.Fatal("a creating restore point became current")
	}
}

// 新建还原点时可同时应用，跟随的分组也一并切换。
func TestReductionServiceCreateCanSetCurrent(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	if err := st.Groups().Create(ctx, domain.Group{ID: "g-follow", Name: "follow", StartIP: "192.168.1.10", ClientMax: 10, Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-1"}); err != nil {
		t.Fatal(err)
	}
	service := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	res, err := service.Create(ctx, "cfg-1", CreateReductionRequest{Name: "cad", SetCurrent: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := st.Configs().Get(ctx, "cfg-1")
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != res.Reduction.ID {
		t.Fatalf("config current = %v, want the new %s", cfg.DefaultReductionID, res.Reduction.ID)
	}
	g, _ := st.Groups().Get(ctx, "g-follow")
	if g.SystemReductionID != res.Reduction.ID {
		t.Fatalf("following group = %#v", g)
	}
	// 不带标志时指针保持不变。
	res2, err := service.Create(ctx, "cfg-1", CreateReductionRequest{Name: "cad2"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = st.Configs().Get(ctx, "cfg-1")
	if *cfg.DefaultReductionID == res2.Reduction.ID {
		t.Fatal("plain create moved the pointer")
	}
}

// 当前点被删或合并掉后修复配置指针，跟随的分组必须一起移动，否则会引用已删除的行。
func TestReductionServiceDeleteRepairRetargetsFollowers(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	if err := st.Groups().Create(ctx, domain.Group{ID: "g-follow", Name: "follow", StartIP: "192.168.1.10", ClientMax: 10, Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-1"}); err != nil {
		t.Fatal(err)
	}
	// 分组引用 red-1 会挡住删除，所以先应用 red-2 再删 red-1，与操作者的步骤一致。
	service := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	if _, err := service.SetCurrent(ctx, "red-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, "red-1"); err != nil {
		t.Fatal(err)
	}
	g, _ := st.Groups().Get(ctx, "g-follow")
	if g.SystemReductionID != "red-2" {
		t.Fatalf("follower = %#v", g)
	}
}

func TestReductionServiceMergeKeepsLatest(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	storage := &fakeReductionStorage{}

	result, err := (ReductionService{Store: st, Storage: storage}).Merge(ctx, "cfg-1", MergeReductionsRequest{KeepReductionID: "red-2"})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" {
		t.Fatal("task id is empty")
	}
	if len(storage.mergedNames) != 1 || storage.mergedNames[0] != "@0" {
		t.Fatalf("storage = %#v", storage)
	}
	if _, err := st.Reductions().Get(ctx, "red-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("red-1 err = %v", err)
	}
	if _, err := st.Reductions().Get(ctx, "red-2"); err != nil {
		t.Fatalf("red-2 err = %v", err)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "red-2" {
		t.Fatalf("default = %#v", cfg.DefaultReductionID)
	}
}

// 合并掉无关还原点时不改操作者选的默认点，只有默认点本身被删才移动。
func TestReductionServiceMergeLeavesSurvivingDefaultAlone(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	now := time.Now().UTC()
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-3", ConfigID: "cfg-1", Name: "@2", CreatedAt: now.Add(2 * time.Second), Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	// cfg-1 默认是 red-1；合并删 red-2、保留 red-3。
	if _, err := (ReductionService{Store: st, Storage: &fakeReductionStorage{}}).Merge(ctx, "cfg-1", MergeReductionsRequest{
		KeepReductionID:    "red-3",
		DeleteReductionIDs: []string{"red-2"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "red-1" {
		t.Fatalf("default = %#v, want red-1 untouched", cfg.DefaultReductionID)
	}
}

// 库里不记 fork 克隆了哪个还原点，守卫须问池；否则会在 ZFS 报 "snapshot has dependent clones ... use '-R'"。
func TestReductionServiceRejectsDeleteWhenAForkDependsOnIt(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-fork", ImageID: "img-1", Name: "派生配置", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	storage := &fakeReductionStorage{dependents: map[string][]string{"@1": {"tank/cfg-fork"}}}

	_, err := (ReductionService{Store: st, Storage: storage}).Delete(ctx, "red-2")
	if !errors.Is(err, ErrReductionHasDependents) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "派生配置") {
		t.Fatalf("error should name the dependent config: %v", err)
	}
	if storage.deletedName != "" {
		t.Fatalf("storage was asked to destroy the snapshot: %#v", storage)
	}
	if _, err := st.Reductions().Get(ctx, "red-2"); err != nil {
		t.Fatalf("reduction must survive: %v", err)
	}
}

func TestReductionServiceRejectsMergeWhenAForkDependsOnADoomedReduction(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	storage := &fakeReductionStorage{dependents: map[string][]string{"@0": {"tank/CLIENT-AABBCCDDEEFF"}}}

	_, err := (ReductionService{Store: st, Storage: storage}).Merge(ctx, "cfg-1", MergeReductionsRequest{KeepReductionID: "red-2"})
	if !errors.Is(err, ErrReductionHasDependents) {
		t.Fatalf("err = %v", err)
	}
	// 客户机克隆要报成所属机器，而不是数据集名。
	if !strings.Contains(err.Error(), "AA:BB:CC:DD:EE:FF") {
		t.Fatalf("error should name the machine holding it: %v", err)
	}
	if storage.mergedNames != nil {
		t.Fatalf("storage was asked to merge: %#v", storage)
	}
}

// ZFS 逐个销毁快照且不能回滚：中途失败时已销毁快照的行必须一起删掉。
func TestReductionServiceMergePersistsSnapshotsAlreadyDestroyed(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	now := time.Now().UTC()
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-3", ConfigID: "cfg-1", Name: "@2", CreatedAt: now.Add(2 * time.Second), Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	// 「@0」销毁成功，「@1」失败，模拟被 fork 占用的快照。
	storage := &fakeReductionStorage{failMergeAfter: 1, err: errors.New("snapshot has dependent clones")}

	if _, err := (ReductionService{Store: st, Storage: storage}).Merge(ctx, "cfg-1", MergeReductionsRequest{KeepReductionID: "red-3"}); err == nil {
		t.Fatal("expected merge to fail")
	}
	if _, err := st.Reductions().Get(ctx, "red-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("destroyed reduction must not survive in the store: %v", err)
	}
	if _, err := st.Reductions().Get(ctx, "red-2"); err != nil {
		t.Fatalf("reduction whose snapshot survived must stay: %v", err)
	}
}

func seedReductionStore(t *testing.T) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "file:"+filepath.Join(t.TempDir(), "reduction.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	defaultID := "red-1"
	if err := st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-1", ImageID: "img-1", Name: "default", DefaultReductionID: &defaultID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-1", ConfigID: "cfg-1", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-2", ConfigID: "cfg-1", Name: "@1", CreatedAt: now.Add(time.Second), Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	return st
}

type fakeReductionStorage struct {
	storage.StorageAgent
	createdConfig string
	createdName   string
	deletedConfig string
	deletedName   string
	mergedConfig  string
	mergedNames   []string
	err           error
	// failMergeAfter 大于 0 时先销毁这么多快照再失败，模拟 ZFS 的部分完成。
	failMergeAfter int
	dependents     map[string][]string
	// missingSnapshots 中的快照名表示行还在、池上快照已丢失。
	missingSnapshots map[string]bool
}

func (s *fakeReductionStorage) CreateReduction(_ context.Context, configID string, name string) (domain.Reduction, error) {
	s.createdConfig = configID
	s.createdName = name
	if s.err != nil {
		return domain.Reduction{}, s.err
	}
	// ID 须按真实 agent 的方式派生，固定 ID 会让第二个还原点主键冲突。
	snapshot := strings.TrimPrefix(normalizeReductionName(name), "@")
	return domain.Reduction{
		ID:        storage.ReductionID(configID, snapshot),
		ConfigID:  configID,
		Name:      normalizeReductionName(name),
		CreatedAt: time.Now().UTC(),
		Status:    domain.ReductionStatusReady,
	}, nil
}

func (s *fakeReductionStorage) DeleteReduction(_ context.Context, configID string, name string) error {
	s.deletedConfig = configID
	s.deletedName = name
	return s.err
}

func (s *fakeReductionStorage) MergeReduction(_ context.Context, configID string, deleteNames []string) ([]string, error) {
	s.mergedConfig = configID
	s.mergedNames = append([]string{}, deleteNames...)
	if s.failMergeAfter > 0 {
		done := deleteNames
		if len(done) > s.failMergeAfter {
			done = done[:s.failMergeAfter]
		}
		return append([]string{}, done...), s.err
	}
	if s.err != nil {
		return nil, s.err
	}
	return append([]string{}, deleteNames...), nil
}

func (s *fakeReductionStorage) ReductionDependents(_ context.Context, configID string, name string) ([]string, error) {
	if s.missingSnapshots[strings.TrimPrefix(name, "@")] {
		return nil, storage.ErrSnapshotMissing
	}
	return s.dependents[name], nil
}

// 显示名保留操作者原样输入的「装完office」，只有底层快照名折成 ascii（与配置、镜像一致）。
func TestReductionServiceKeepsTheNameTheOperatorTyped(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	agent := &fakeReductionStorage{}

	result, err := (ReductionService{Store: st, Storage: agent}).Create(ctx, "cfg-1", CreateReductionRequest{Name: "装完office"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reduction.DisplayName != "装完office" {
		t.Fatalf("display name = %q, want it kept as typed", result.Reduction.DisplayName)
	}
	for _, r := range agent.createdName {
		if r > 127 {
			t.Fatalf("snapshot name %q reached ZFS with non-ascii", agent.createdName)
		}
	}
	stored, err := st.Reductions().Get(ctx, result.Reduction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DisplayName != "装完office" || stored.Name != agent.createdName {
		t.Fatalf("stored = %#v, storage created %q", stored, agent.createdName)
	}
}

// 折成同一 ascii 的两个名字不能落到同一快照，否则第二个还原点会静默变成第一个。
func TestReductionServiceSeparatesNamesThatFoldTogether(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{}}

	first, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "装完"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "完装"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Reduction.Name == second.Reduction.Name {
		t.Fatalf("both restore points became %q", first.Reduction.Name)
	}
	if first.Reduction.ID == second.Reduction.ID {
		t.Fatalf("both rows became %q", first.Reduction.ID)
	}
}

// 查重针对显示名：同名「装完office」要拒绝，ascii 名仍不区分大小写。
func TestReductionServiceRejectsDuplicateDisplayNames(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "装完office"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "装完office"}); !errors.Is(err, ErrReductionExists) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "Office装完"}); err != nil {
		t.Fatalf("a different name must still be allowed: %v", err)
	}
}

// 快照已丢失的还原点即使是当前应用点也必须删得掉，否则分组每次开机都失败且无法恢复。
// 「正在使用」只拦快照还在的还原点。
func TestDeleteAllowsRemovingACurrentPointWhoseSnapshotIsGone(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	if err := st.Groups().Create(ctx, domain.Group{ID: "g-1", Name: "group", StartIP: "10.0.0.1",
		ClientMax: 10, Gateway: "10.0.0.254", Netmask: "255.255.255.0",
		SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-1"}); err != nil {
		t.Fatal(err)
	}
	ag := &fakeReductionStorage{missingSnapshots: map[string]bool{"0": true, "1": true}} // red-1=@0, red-2=@1

	// 快照还在时照常拒绝。
	if _, err := (ReductionService{Store: st, Storage: &fakeReductionStorage{}}).Delete(ctx, "red-1"); err == nil || !strings.Contains(err.Error(), "先应用") {
		t.Fatalf("好的当前点仍应拒绝删除：%v", err)
	}

	// 快照已丢失但仍被引用：不替运维改指向，但错误要说清下一步。
	_, err := (ReductionService{Store: st, Storage: ag}).Delete(ctx, "red-1")
	if err == nil {
		t.Fatal("还被引用时不该直接删掉：外键会挡住，而且那等于替运维换了开机用的还原点")
	}
	for _, want := range []string{"快照", "已丢失", "当前应用点"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("要说清楚是什么坏了、下一步做什么，实际：%v", err)
		}
	}

	// 无人引用时直接删掉。
	if _, err := (ReductionService{Store: st, Storage: ag}).Delete(ctx, "red-2"); err != nil {
		t.Fatalf("无人引用、快照又已丢失的还原点必须删得掉：%v", err)
	}
}

// 超管机编辑配置期间，不能从镜像页给该配置建还原点：保存会覆盖掉新点，两者必丢其一，所以挡在源头。
func TestReductionCreateRefusedWhileASuperMachineEditsTheConfig(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	now := time.Now().UTC()
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-data", ImageID: "img-1", Name: "游戏盘", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group := domain.Group{ID: "grp-1", Name: "default", IsDefault: true, StartIP: "192.168.1.10", ClientMax: 5,
		Gateway: "192.168.1.1", Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1", SystemReductionID: "red-1"}
	if err := st.Groups().Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd-1", GroupID: group.ID, ConfigID: "cfg-data", ImageID: "img-1", MountTarget: "D:"}); err != nil {
		t.Fatal(err)
	}
	super := domain.Terminal{ID: "t-1", Name: "教师机", MAC: "001122334455", IP: "192.168.1.10", GroupID: group.ID,
		IsSuper: true, State: domain.TerminalStateOffline}
	if err := st.Terminals().Create(ctx, super); err != nil {
		t.Fatal(err)
	}

	pool := &fakeReductionStorage{}
	svc := ReductionService{Store: st, Storage: pool}
	for _, configID := range []string{"cfg-1", "cfg-data"} { // 系统盘配置和它那块数据盘的配置都算
		_, err := svc.Create(ctx, configID, CreateReductionRequest{Name: "r9"})
		if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "教师机") {
			t.Fatalf("%s：应当拒绝并指出是哪台超管机，得到 %v", configID, err)
		}
	}
	if pool.createdConfig != "" {
		t.Fatalf("被拒绝了却建了快照：%#v", pool)
	}

	// 不再是超管机后照常允许。
	super.IsSuper = false
	if err := st.Terminals().Update(ctx, super); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "cfg-1", CreateReductionRequest{Name: "r9"}); err != nil {
		t.Fatalf("没有超管机在编辑就该放行：%v", err)
	}
}
