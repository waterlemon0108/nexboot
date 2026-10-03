package assets

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// seedOverwrite 建 img-1：cfg-keep（@0、@r1、@r2，当前应用点为 current）和 cfg-drop。
func seedOverwrite(t *testing.T, current string) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cur := current
	for _, err := range []error{
		st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: t0}),
		st.Configs().Create(ctx, domain.Config{ID: "cfg-keep", ImageID: "img-1", Name: "keep", DefaultReductionID: &cur, CreatedAt: t0}),
		st.Configs().Create(ctx, domain.Config{ID: "cfg-drop", ImageID: "img-1", Name: "drop", CreatedAt: t0}),
		st.Reductions().Create(ctx, domain.Reduction{ID: "cfg-keep_0", ConfigID: "cfg-keep", Name: "@0", Status: domain.ReductionStatusReady, CreatedAt: t0}),
		st.Reductions().Create(ctx, domain.Reduction{ID: "cfg-keep_r1", ConfigID: "cfg-keep", Name: "@r1", DisplayName: "装完office", Status: domain.ReductionStatusReady, CreatedAt: t0.Add(time.Hour)}),
		st.Reductions().Create(ctx, domain.Reduction{ID: "cfg-keep_r2", ConfigID: "cfg-keep", Name: "@r2", DisplayName: "装完游戏", Status: domain.ReductionStatusReady, CreatedAt: t0.Add(2 * time.Hour)}),
		st.Reductions().Create(ctx, domain.Reduction{ID: "cfg-drop_0", ConfigID: "cfg-drop", Name: "@0", Status: domain.ReductionStatusReady, CreatedAt: t0}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func busy(...string) []string { return []string{"192.168.10.4"} }

func TestOverwriteImageFromTheNewestRestorePoint(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r2")
	fake := &fakeConfigStorage{}
	if _, err := (ConfigService{Store: st, Storage: fake}).OverwriteImage(ctx, "cfg-keep_r2"); err != nil {
		t.Fatal(err)
	}
	if fake.mergeReq.RollbackTo != "" || strings.Join(fake.mergeReq.ReductionNames, ",") != "@0,@r1,@r2" || fake.mergeReq.DeleteConfigIDs[0] != "cfg-drop" {
		t.Fatalf("merge req = %#v", fake.mergeReq)
	}
}

func TestOverwriteImageFromAnOlderRestorePointRollsBackFirst(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r2")
	fake := &fakeConfigStorage{}
	if _, err := (ConfigService{Store: st, Storage: fake}).OverwriteImage(ctx, "cfg-keep_r1"); err != nil {
		t.Fatal(err)
	}
	if fake.mergeReq.RollbackTo != "r1" || strings.Join(fake.mergeReq.ReductionNames, ",") != "@0,@r1" {
		t.Fatalf("merge req = %#v", fake.mergeReq)
	}
	reds, _ := st.Reductions().ListByConfig(ctx, "cfg-keep")
	cfg, _ := st.Configs().Get(ctx, "cfg-keep")
	if len(reds) != 1 || cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != reds[0].ID {
		t.Fatalf("after overwrite: reductions=%#v current=%v", reds, cfg.DefaultReductionID)
	}
}

func TestOverwriteAndMergeRefuseWhileTheCatalogueIsCopied(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r2")
	fake := &fakeConfigStorage{}
	svc := ConfigService{Store: st, Storage: fake, Replicating: busy}
	if _, err := svc.OverwriteImage(ctx, "cfg-keep_r2"); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "192.168.10.4") {
		t.Fatalf("overwrite err = %v", err)
	}
	if _, err := svc.Merge(ctx, "cfg-keep"); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "192.168.10.4") {
		t.Fatalf("merge err = %v", err)
	}
	if fake.mergeReq.ConfigID != "" {
		t.Fatal("refused requests must not touch storage")
	}
}

// 当前应用的不是最新还原点时合并必须拒绝，否则会悄悄撤掉那次回滚。
func TestMergeRefusesWhenTheAppliedRestorePointIsNotTheNewest(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r1")
	fake := &fakeConfigStorage{}
	_, err := (ConfigService{Store: st, Storage: fake}).Merge(ctx, "cfg-keep")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "装完office") || !strings.Contains(err.Error(), "装完游戏") {
		t.Fatalf("err = %v", err)
	}
	if fake.mergeReq.ConfigID != "" {
		t.Fatal("refused merge touched storage")
	}
}

// blockingExportStorage 让导出停在发送流的中途，直到测试放行。
type blockingExportStorage struct {
	fakeConfigStorage
	entered chan struct{}
	release chan struct{}
}

func (s *blockingExportStorage) ExportImage(context.Context, storage.ExportImageReq) error {
	close(s.entered)
	<-s.release
	return nil
}

func (s *blockingExportStorage) ExportImageSize(context.Context, string) (int64, error) {
	return 1 << 20, nil
}

// 导出期间镜像上挂着 ndexport- 快照并在持续发送：合并、覆盖、删除镜像都要当场点名拒绝；导出结束后放行。
func TestMergeOverwriteAndDeleteRefuseWhileTheImageIsExported(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r2")
	slow := &blockingExportStorage{entered: make(chan struct{}), release: make(chan struct{})}
	images := ImageService{Store: st, Storage: slow}
	done := make(chan error, 1)
	go func() { done <- images.StreamImage(ctx, "img-1", io.Discard, false) }()
	<-slow.entered

	fake := &fakeConfigStorage{}
	cfgSvc := ConfigService{Store: st, Storage: fake}
	_, mergeErr := cfgSvc.Merge(ctx, "cfg-keep")
	_, overwriteErr := cfgSvc.OverwriteImage(ctx, "cfg-keep_r2")
	deleteErr := (ImageService{Store: st, Storage: &fakeImageStorage{}}).DeleteImage(ctx, "img-1")
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for what, err := range map[string]error{"合并": mergeErr, "覆盖": overwriteErr, "删除": deleteErr} {
		if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win") || !strings.Contains(err.Error(), "导出") {
			t.Fatalf("导出进行中时%s应点名拒绝，得到 %v", what, err)
		}
	}
	if fake.mergeReq.ConfigID != "" {
		t.Fatal("被拒绝的请求不能碰存储")
	}
	if _, err := cfgSvc.Merge(ctx, "cfg-keep"); err != nil {
		t.Fatalf("导出结束后应能合并: %v", err)
	}
}

// 导出到服务器目录是后台任务：任务跑完之前同样要挡住删除镜像。
func TestDeleteImageRefusesWhileAnExportToDirRuns(t *testing.T) {
	ctx := context.Background()
	st := seedOverwrite(t, "cfg-keep_r2")
	slow := &blockingExportStorage{entered: make(chan struct{}), release: make(chan struct{})}
	res, err := (ImageService{Store: st, Storage: slow, ImportDir: t.TempDir(), Async: true}).ExportImageToDir(ctx, ImageExportRequest{ImageID: "img-1"})
	if err != nil {
		t.Fatal(err)
	}
	<-slow.entered
	deleteErr := (ImageService{Store: st, Storage: &fakeImageStorage{}}).DeleteImage(ctx, "img-1")
	close(slow.release)
	waitTaskDone(t, st, res.TaskID)
	if !errors.Is(deleteErr, errs.ErrConflict) || !strings.Contains(deleteErr.Error(), "正在导出") {
		t.Fatalf("导出任务进行中时删除应拒绝，得到 %v", deleteErr)
	}
}
