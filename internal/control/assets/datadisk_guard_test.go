package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func TestDeleteConfigNamesTheDataDiskUsingIt(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := seedConfigInUseBase(ctx, st, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd-1", GroupID: "g-1", MountTarget: "D:", ImageID: "img-1", ConfigID: "cfg-1"}); err != nil {
		t.Fatal(err)
	}
	_, err := (ConfigService{Store: st, Storage: &fakeConfigStorage{}}).Delete(ctx, "cfg-1")
	if !errors.Is(err, ErrConfigInUse) || !strings.Contains(err.Error(), "数据盘 D:") {
		t.Fatalf("err = %v, want it to name the data disk", err)
	}
}

// seedDataDiskOnCfg1 让 cfg-1（当前点 red-1，另有 red-2）成为分组 教学一班 的数据盘 /data，该分组系统盘是另一镜像。
func seedDataDiskOnCfg1(t *testing.T) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st := seedReductionStore(t)
	now := time.Now().UTC()
	sysRed := "red-sys"
	for _, err := range []error{
		st.Images().Create(ctx, domain.Image{ID: "img-sys", Name: "sys", OSType: domain.OSTypeLinux, State: domain.ImageStateNormal, CreatedAt: now}),
		st.Configs().Create(ctx, domain.Config{ID: "cfg-sys", ImageID: "img-sys", Name: "default", DefaultReductionID: &sysRed, CreatedAt: now}),
		st.Reductions().Create(ctx, domain.Reduction{ID: sysRed, ConfigID: "cfg-sys", Name: "@0", Status: domain.ReductionStatusReady, CreatedAt: now}),
		st.Groups().Create(ctx, domain.Group{ID: "g-1", Name: "教学一班", StartIP: "192.168.1.10", ClientMax: 10, Netmask: "255.255.255.0",
			SystemImageID: "img-sys", SystemConfigID: "cfg-sys", SystemReductionID: sysRed}),
		st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd-1", GroupID: "g-1", MountTarget: "/data", ImageID: "img-1", ConfigID: "cfg-1"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// 数据盘按配置当前应用的还原点开机：删掉它，数据盘就悄悄换了内容，删光了就开不了机。
func TestDeletingTheRestorePointADataDiskBootsIsRefused(t *testing.T) {
	ctx := context.Background()
	st := seedDataDiskOnCfg1(t)
	fake := &fakeReductionStorage{}
	_, err := (ReductionService{Store: st, Storage: fake}).Delete(ctx, "red-1")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "教学一班") || !strings.Contains(err.Error(), "/data") {
		t.Fatalf("err = %v", err)
	}
	if fake.deletedName != "" {
		t.Fatal("refused delete touched storage")
	}
	if _, err := (ReductionService{Store: st, Storage: fake}).Delete(ctx, "red-2"); err != nil {
		t.Fatalf("a restore point the data disk does not boot must stay deletable: %v", err)
	}
}

// 镜像的最后一个配置删了，镜像就没法再分配、也体检不了；要删就删镜像。
func TestDeleteRefusesTheImagesLastConfig(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	for _, err := range []error{
		st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}),
		st.Configs().Create(ctx, domain.Config{ID: "cfg-1", ImageID: "img-1", Name: "default", CreatedAt: now}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeConfigStorage{}
	_, err := (ConfigService{Store: st, Storage: fake}).Delete(ctx, "cfg-1")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win") || !strings.Contains(err.Error(), "删除镜像") {
		t.Fatalf("err = %v", err)
	}
	if fake.deleted != "" {
		t.Fatal("refused delete touched storage")
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-2", ImageID: "img-1", Name: "work", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := (ConfigService{Store: st, Storage: fake}).Delete(ctx, "cfg-1"); err != nil {
		t.Fatalf("with a sibling left it must be deletable: %v", err)
	}
}

// 客户机按配置当前应用的还原点开机：删掉它，下次开机内容就悄悄换了。
func TestDeletingTheAppliedRestorePointIsRefused(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t) // cfg-1 当前应用 red-1(@0)，另有 red-2(@1)
	fake := &fakeReductionStorage{}
	_, err := (ReductionService{Store: st, Storage: fake}).Delete(ctx, "red-1")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "default") || !strings.Contains(err.Error(), "先应用") {
		t.Fatalf("err = %v", err)
	}
	if fake.deletedName != "" {
		t.Fatal("refused delete touched storage")
	}
	if _, err := (ReductionService{Store: st, Storage: fake}).Delete(ctx, "red-2"); err != nil {
		t.Fatalf("a restore point not applied must stay deletable: %v", err)
	}
}

func TestDeletingTheOnlyRestorePointIsRefused(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	cfg, _ := st.Configs().Get(ctx, "cfg-1")
	cfg.DefaultReductionID = nil // 没有当前点也一样：删了就没东西可开机
	if err := st.Configs().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Delete(ctx, "red-1"); err != nil {
		t.Fatal(err)
	}
	_, err := (ReductionService{Store: st, Storage: &fakeReductionStorage{}}).Delete(ctx, "red-2")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "唯一") || !strings.Contains(err.Error(), "新建") {
		t.Fatalf("err = %v", err)
	}
}
