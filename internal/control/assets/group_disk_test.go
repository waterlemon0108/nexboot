package assets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// seedDataImage 添加一个带默认配置和 @0 的数据盘镜像，供分组数据盘使用。
func seedDataImage(t *testing.T, ctx context.Context, st store.Store, imageID, configID string, os domain.OSType) {
	t.Helper()
	now := time.Now().UTC()
	red := configID + "_0"
	if err := st.Images().Create(ctx, domain.Image{ID: imageID, Name: imageID, OSType: os, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeData, Origin: domain.ImageOriginBlank, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: configID, ImageID: imageID, Name: "default", DefaultReductionID: &red, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: red, ConfigID: configID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
}

func TestGroupDiskServiceRejectsDuplicateMountTarget(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedDataImage(t, ctx, st, "games", "games_default", domain.OSTypeWindows)
	service := GroupDiskService{Store: st}

	// Windows 分组里 "d" 规范化为 "D:"，所以 "D:" 与它冲突。
	disk, err := service.Create(ctx, group.ID, GroupDiskRequest{MountTarget: "d", ImageID: "games", ConfigID: "games_default"})
	if err != nil {
		t.Fatal(err)
	}
	if disk.MountTarget != "D:" {
		t.Fatalf("disk = %#v", disk)
	}
	_, err = service.Create(ctx, group.ID, GroupDiskRequest{MountTarget: "D:", ImageID: "games", ConfigID: "games_default"})
	if !errors.Is(err, ErrGroupDiskExists) {
		t.Fatalf("duplicate err = %v", err)
	}

	result, err := service.List(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Items[0].ID != disk.ID {
		t.Fatalf("result = %#v", result)
	}
}

func TestGroupDiskServiceRejectsConfigImageMismatch(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedDataImage(t, ctx, st, "img-2", "img-2_default", domain.OSTypeLinux)

	// 数据盘镜像搭配别的镜像的配置：不匹配。
	_, err := (GroupDiskService{Store: st}).Create(ctx, group.ID, GroupDiskRequest{MountTarget: "E:", ImageID: "img-2", ConfigID: "cfg-1"})
	if !errors.Is(err, ErrGroupDiskMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}
}

func TestGroupDiskServiceRejectsBadWindowsLetter(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := GroupDiskService{Store: st}

	for _, target := range []string{"C:", "/data", "DE", "1"} {
		if _, err := service.Create(ctx, group.ID, GroupDiskRequest{MountTarget: target, ImageID: "img-1", ConfigID: "cfg-1"}); !errors.Is(err, ErrGroupDiskBadLetter) {
			t.Fatalf("target %q err = %v", target, err)
		}
	}
}

func TestGroupDiskServiceLinuxMountTargetIsAPlainAbsolutePath(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	sys, err := st.Images().Get(ctx, group.SystemImageID)
	if err != nil {
		t.Fatal(err)
	}
	sys.OSType = domain.OSTypeLinux
	if err := st.Images().Update(ctx, sys); err != nil {
		t.Fatal(err)
	}
	seedDataImage(t, ctx, st, "games", "games_default", domain.OSTypeLinux)
	service := GroupDiskService{Store: st}

	for _, target := range []string{"data", "/", "/etc", "/usr/local", "/boot/x", "/a/../b", "/data/", "D:"} {
		if _, err := service.Create(ctx, group.ID, GroupDiskRequest{MountTarget: target, ImageID: "games", ConfigID: "games_default"}); !errors.Is(err, ErrGroupDiskBadPath) {
			t.Fatalf("target %q err = %v", target, err)
		}
	}
	disk, err := service.Create(ctx, group.ID, GroupDiskRequest{MountTarget: "/data/games", ImageID: "games", ConfigID: "games_default"})
	if err != nil || disk.MountTarget != "/data/games" {
		t.Fatalf("disk = %#v err = %v", disk, err)
	}
}
