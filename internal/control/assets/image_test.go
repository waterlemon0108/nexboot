package assets

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestImageServiceImportImageWritesDBAndTask(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	service := ImageService{Store: st, Storage: &fakeImageStorage{now: now}, Now: func() time.Time { return now }}

	result, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || result.ImageID != "win11" || result.ConfigID != "win11_default" || result.ReductionID != "win11_0" {
		t.Fatalf("result = %#v", result)
	}
	if _, err := st.Images().Get(ctx, "win11"); err != nil {
		t.Fatalf("image missing: %v", err)
	}
	cfg, err := st.Configs().Get(ctx, "win11_default")
	if err != nil {
		t.Fatalf("config missing: %v", err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "win11_0" {
		t.Fatalf("default reduction = %#v", cfg.DefaultReductionID)
	}
	task, err := st.Tasks().Get(ctx, result.TaskID)
	if err != nil {
		t.Fatalf("task missing: %v", err)
	}
	if task.Status != domain.TaskStatusSuccess || task.Progress != 100 {
		t.Fatalf("task = %#v", task)
	}
}

func TestImageServiceImportImageRecordsItsTargetWhileRunning(t *testing.T) {
	// 导入运行中就要登记目标，否则一致性检查会把它的数据集当成孤儿。
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	var midRunRef string
	fake := &fakeImageStorage{now: now, onImport: func(req storage.ImportImageReq) {
		req.OnTarget("win11")
		req.OnProgress(42, "转换写入 42%")
		tasks, err := st.Tasks().ListActive(ctx)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("active = %#v, %v", tasks, err)
		}
		midRunRef = tasks[0].TargetRef
	}}
	service := ImageService{Store: st, Storage: fake, Now: func() time.Time { return now }}
	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}
	if midRunRef != "win11" {
		t.Fatalf("TargetRef mid-import = %q", midRunRef)
	}
}

// 登记目标时只改目标：新建空盘先报了「准备」进度再登记，整行写回旧副本会让进度条倒退。
func TestRecordTargetKeepsTheStoredProgress(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	task := domain.Task{ID: "t1", Type: domain.TaskTypeCreateBlankImage, Status: domain.TaskStatusRunning, CreatedAt: time.Now().UTC()}
	if err := st.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	stored := task
	stored.Progress, stored.Message = 5, "准备"
	if err := st.Tasks().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	recordTarget(ctx, st, &task, "games")
	got, err := st.Tasks().Get(ctx, "t1")
	if err != nil || got.TargetRef != "games" || got.Progress != 5 || got.Message != "准备" || task.TargetRef != "games" {
		t.Fatalf("stored = %#v local = %#v err = %v", got, task, err)
	}
}

func TestImageServiceImportImageReportsProgressDuringImport(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	var taskID string
	var midRunProgress int
	var midRunMessage string
	var midRunStatus domain.TaskStatus
	storage := &fakeImageStorage{now: now, onImport: func(req storage.ImportImageReq) {
		req.OnProgress(42, "转换写入 42%")
		task, err := st.Tasks().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(task) != 1 {
			t.Fatalf("tasks = %#v", task)
		}
		taskID = task[0].ID
		midRunProgress = task[0].Progress
		midRunMessage = task[0].Message
		midRunStatus = task[0].Status
	}}
	service := ImageService{Store: st, Storage: storage, Now: func() time.Time { return now }}

	result, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != taskID {
		t.Fatalf("task id = %q, want %q", taskID, result.TaskID)
	}
	if midRunProgress != 42 || midRunStatus != domain.TaskStatusRunning || midRunMessage == "" {
		t.Fatalf("mid-run task progress=%d status=%q message=%q", midRunProgress, midRunStatus, midRunMessage)
	}
	final, err := st.Tasks().Get(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskStatusSuccess || final.Progress != 100 || final.Message != "完成" {
		t.Fatalf("final task = %#v", final)
	}
}

func TestImageServiceRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := ImageService{Store: st, Storage: &fakeImageStorage{now: now}, Now: func() time.Time { return now }}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if !errors.Is(err, ErrImageExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestImageServiceMarksTaskFailed(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	service := ImageService{Store: st, Storage: &fakeImageStorage{err: errors.New("zfs failed"), now: now}, Now: func() time.Time { return now }}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if err == nil {
		t.Fatal("import succeeded")
	}
	tasks, listErr := st.Tasks().List(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(tasks) != 1 || tasks[0].Status != domain.TaskStatusFailed {
		t.Fatalf("tasks = %#v", tasks)
	}
	if _, err := st.Images().Get(ctx, "win11"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("image err = %v", err)
	}
}

func TestImageServiceRollsBackStorageWhenDBTxFails(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "other", Name: "other", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "win11_default", ImageID: "other", Name: "taken", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	storage := &fakeImageStorage{now: now}
	service := ImageService{Store: st, Storage: storage, Now: func() time.Time { return now }}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if err == nil {
		t.Fatal("import succeeded")
	}
	if !storage.rollbackCalled || storage.rollbackReq.SourcePath != "/var/lib/ndiskless/imports/win11.zfs" || storage.rollbackImported.Image.ID != "win11" {
		t.Fatalf("rollback = called:%v req:%#v imported:%#v", storage.rollbackCalled, storage.rollbackReq, storage.rollbackImported)
	}
	if _, err := st.Images().Get(ctx, "win11"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("image err = %v", err)
	}
}

func TestImageServiceRejectsImportSourcePathOutsideImportDir(t *testing.T) {
	ctx := context.Background()
	service := ImageService{Store: newImageTestStore(t), Storage: &fakeImageStorage{}}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/tmp/import.zfs", OSType: domain.OSTypeWindows})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "请把文件放到导入目录 /var/lib/ndiskless/imports 下") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageServiceUsesConfiguredImportDir(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{Store: newImageTestStore(t), Storage: storage, ImportDir: "/tank/imports"}

	result, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/tank/imports/win11.zfs.gz", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || storage.req.ImportDir != "/tank/imports" {
		t.Fatalf("result=%#v req=%#v", result, storage.req)
	}

	_, err = service.ImportImage(ctx, ImportImageRequest{Name: "win12", SourcePath: "/var/lib/ndiskless/imports/win12.zfs", OSType: domain.OSTypeWindows})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "请把文件放到导入目录 /tank/imports 下") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageServiceUsesImportDirResolver(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{
		Store:     newImageTestStore(t),
		Storage:   storage,
		ImportDir: "/ignored/imports",
		ImportDirResolver: func(context.Context) (string, error) {
			return "/data/imports", nil
		},
	}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/data/imports/win11.zfs", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if storage.req.ImportDir != "/data/imports" {
		t.Fatalf("import dir = %q", storage.req.ImportDir)
	}
}

func TestImageServiceAcceptsLegacyGzipImportFile(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{Store: newImageTestStore(t), Storage: storage, ImportDir: "/tank/imports"}

	result, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/tank/imports/win11.gzip", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || storage.req.SourcePath != "/tank/imports/win11.gzip" {
		t.Fatalf("result=%#v req=%#v", result, storage.req)
	}
}

func TestImageServiceAcceptsDiskImageImportFile(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{Store: newImageTestStore(t), Storage: storage, ImportDir: "/tank/imports"}

	result, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/tank/imports/win11.vmdk", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || storage.req.SourcePath != "/tank/imports/win11.vmdk" {
		t.Fatalf("result=%#v req=%#v", result, storage.req)
	}
}

// 名称按输入保存，只去首尾空白；转成数据集安全形式是存储层的事，不改变操作者看到的名字。
func TestImageServiceKeepsImportImageNameAsTyped(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{Store: newImageTestStore(t), Storage: storage, ImportDir: "/tank/imports"}

	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "  Windows 10 x64-0 ", SourcePath: "/tank/imports/Windows 10 x64-0.vmdk", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}
	if storage.req.Name != "Windows 10 x64-0" {
		t.Fatalf("name = %q, want it kept", storage.req.Name)
	}
}

func TestImageServiceRejectsImportSourcePathWithInvalidExtension(t *testing.T) {
	ctx := context.Background()
	service := ImageService{Store: newImageTestStore(t), Storage: &fakeImageStorage{}}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/import.iso", OSType: domain.OSTypeWindows})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "不支持 import.iso") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageServiceRejectsInvalidOSTypeBeforeStorage(t *testing.T) {
	ctx := context.Background()
	storage := &fakeImageStorage{}
	service := ImageService{Store: newImageTestStore(t), Storage: storage}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: "plan9"})
	if err == nil || !strings.Contains(err.Error(), "invalid os_type") {
		t.Fatalf("err = %v", err)
	}
	if storage.importCalled {
		t.Fatal("storage was called")
	}
}

func TestImageServiceDeleteRejectsImageWithConfigs(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	img := domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	reduction := domain.Reduction{ID: "red-1", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	group := domain.Group{
		ID:                "grp-1",
		Name:              "g1",
		IsDefault:         true,
		StartIP:           "192.168.10.10",
		ClientMax:         10,
		Gateway:           "192.168.10.1",
		Netmask:           "255.255.255.0",
		DNS1:              "8.8.8.8",
		DNS2:              "",
		SystemImageID:     img.ID,
		SystemConfigID:    cfg.ID,
		SystemReductionID: reduction.ID,
	}
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, reduction); err != nil {
		t.Fatal(err)
	}
	if err := st.Groups().Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	service := ImageService{Store: st, Storage: &fakeImageStorage{}}

	err := service.DeleteImage(ctx, img.ID)
	if !errors.Is(err, ErrImageInUse) {
		t.Fatalf("err = %v", err)
	}
}

func seedImageWithConfig(t *testing.T, st store.Store) (domain.Image, domain.Config) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	img := domain.Image{ID: "ubuntu-vmdk", Name: "ubuntu-vmdk", OSType: domain.OSTypeLinux, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "ubuntu-vmdk_default", ImageID: img.ID, Name: "default", CreatedAt: now}
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return img, cfg
}

func TestImageServiceDeleteRefusesWhileAStandbyReceivesIt(t *testing.T) {
	st := newImageTestStore(t)
	img, cfg := seedImageWithConfig(t, st)
	fake := &fakeImageStorage{}
	var asked []string
	service := ImageService{Store: st, Storage: fake, Replicating: func(ids ...string) []string {
		asked = ids
		return []string{"192.168.10.5"}
	}}
	err := service.DeleteImage(context.Background(), img.ID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "ubuntu-vmdk") || !strings.Contains(err.Error(), "192.168.10.5") {
		t.Fatalf("err = %v", err)
	}
	if fake.deleted != "" {
		t.Fatal("refused delete still reached the pool")
	}
	if fmt.Sprint(asked) != fmt.Sprint([]string{img.ID, cfg.ID}) {
		t.Fatalf("asked about %v", asked)
	}
}

func TestImageServiceDeleteTranslatesABusyPool(t *testing.T) {
	st := newImageTestStore(t)
	img, _ := seedImageWithConfig(t, st)
	fake := &fakeImageStorage{err: storage.CommandError{Name: "zfs", Args: []string{"destroy", "-r", "tank/nd/ubuntu-vmdk"},
		Output: "cannot destroy snapshot tank/nd/ubuntu-vmdk@rep-17: dataset is busy"}}
	service := ImageService{Store: st, Storage: fake}
	err := service.DeleteImage(context.Background(), img.ID)
	if !errors.Is(err, errs.ErrConflict) || strings.Contains(err.Error(), "cannot destroy") || !strings.Contains(err.Error(), "ubuntu-vmdk") {
		t.Fatalf("err = %v", err)
	}
	if _, err := st.Images().Get(context.Background(), img.ID); err != nil {
		t.Fatalf("row gone after a failed destroy: %v", err)
	}
}

func TestImageServiceDeleteRemovesStorageAndDB(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	img := domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	reduction := domain.Reduction{ID: "red-1", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, reduction); err != nil {
		t.Fatal(err)
	}
	storage := &fakeImageStorage{}
	service := ImageService{Store: st, Storage: storage}

	if err := service.DeleteImage(ctx, img.ID); err != nil {
		t.Fatal(err)
	}
	if storage.deleted != img.ID {
		t.Fatalf("deleted = %q", storage.deleted)
	}
	if len(storage.deletedConfigs) != 1 || storage.deletedConfigs[0] != cfg.ID {
		t.Fatalf("deleted configs = %#v", storage.deletedConfigs)
	}
	if _, err := st.Images().Get(ctx, img.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("image err = %v", err)
	}
	if _, err := st.Configs().Get(ctx, cfg.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config err = %v", err)
	}
	if got, err := st.Reductions().ListByConfig(ctx, cfg.ID); err != nil || len(got) != 0 {
		t.Fatalf("reductions = %#v err=%v", got, err)
	}
}

func TestImageServiceGetImageIncludesConfigs(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	img := domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	detail, err := (ImageService{Store: st, Storage: &fakeImageStorage{}}).GetImage(ctx, img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Image.ID != img.ID || len(detail.Configs) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
}

// 列表给出镜像及其配置在池上的实际占用，逻辑大小也从池里取（修正记录为 0 的旧行）；
// 客户机克隆不计入镜像。
func TestImageServiceListImagesReportsRealUsage(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	for _, img := range []domain.Image{
		{ID: "win11", Name: "win11", OSType: domain.OSTypeWindows, Size: 0, State: domain.ImageStateNormal, CreatedAt: now},
		{ID: "ubuntu", Name: "ubuntu", OSType: domain.OSTypeLinux, Size: 40 << 30, State: domain.ImageStateNormal, CreatedAt: now},
	} {
		if err := st.Images().Create(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []domain.Config{
		{ID: "win11_default", ImageID: "win11", Name: "default", CreatedAt: now},
		{ID: "win11_office", ImageID: "win11", Name: "office", CreatedAt: now},
		{ID: "ubuntu_default", ImageID: "ubuntu", Name: "default", CreatedAt: now},
	} {
		if err := st.Configs().Create(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	space := &storage.SpaceUsage{PoolUsed: 50 << 30, PoolAvailable: 150 << 30, Volumes: map[string]storage.VolumeUsage{
		"win11":               {Size: 80 << 30, Used: 30 << 30},
		"win11_default":       {Size: 80 << 30, Used: 8 << 10},
		"win11_office":        {Size: 80 << 30, Used: 2 << 30},
		"CLIENT-AABBCCDDEEFF": {Size: 80 << 30, Used: 5 << 30},
		"ubuntu":              {Size: 40 << 30, Used: 6 << 30},
	}}
	service := ImageService{Store: st, Storage: &fakeImageStorage{space: space}}

	result, err := service.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ImageItem{}
	for _, it := range result.Items {
		byID[it.ID] = it
	}
	win := byID["win11"]
	if win.Size != 80<<30 {
		t.Fatalf("win11 size = %d, want the pool's volsize to stand in for the 0 on record", win.Size)
	}
	if win.Used == nil || *win.Used != 30<<30+8<<10+2<<30 {
		t.Fatalf("win11 used = %v, want image + its configs, not the client clone", win.Used)
	}
	ub := byID["ubuntu"]
	if ub.Size != 40<<30 || ub.Used == nil || *ub.Used != 6<<30 {
		t.Fatalf("ubuntu = %#v", ub)
	}
}

// 问不到池时列表照常返回，占用量为未知。
func TestImageServiceListImagesSurvivesUsageFailure(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Images().Create(ctx, domain.Image{ID: "win11", Name: "win11", OSType: domain.OSTypeWindows, Size: 80 << 30, State: domain.ImageStateNormal}); err != nil {
		t.Fatal(err)
	}
	service := ImageService{Store: st, Storage: &fakeImageStorage{spaceErr: errors.New("zfs list failed")}}

	result, err := service.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Size != 80<<30 || result.Items[0].Used != nil {
		t.Fatalf("items = %#v", result.Items)
	}
}

// 导入源列表带上池剩余空间，提交前就能看出放不下；没有存储代理时不返回该字段。
func TestListImportSourcesReportsPoolAvailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "win11.vmdk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	space := &storage.SpaceUsage{PoolAvailable: 150 << 30, Volumes: map[string]storage.VolumeUsage{}}
	res, err := ImageService{ImportDir: dir, Storage: &fakeImageStorage{space: space}}.ListImportSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.PoolAvailable == nil || *res.PoolAvailable != 150<<30 {
		t.Fatalf("pool available = %v", res.PoolAvailable)
	}
	res, err = ImageService{ImportDir: dir}.ListImportSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.PoolAvailable != nil {
		t.Fatalf("pool available without storage = %v", *res.PoolAvailable)
	}
}

// 源数据超过池剩余空间时在提交阶段拒绝并点名文件与存储池，不建任务；空间足够则放行。
func TestImageServiceImportImageRefusesSourceLargerThanPoolAvailable(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "win11.vmdk")
	if err := os.WriteFile(source, make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeImageStorage{space: &storage.SpaceUsage{PoolAvailable: 4 << 10, Volumes: map[string]storage.VolumeUsage{}}}
	service := ImageService{Store: st, Storage: fake, ImportDir: dir}

	_, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: source, OSType: domain.OSTypeWindows})
	if err == nil {
		t.Fatal("import accepted")
	}
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("err kind = %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "win11.vmdk") || !strings.Contains(msg, "存储池") {
		t.Fatalf("message = %q, want it to name the file and the pool", msg)
	}
	if fake.importCalled {
		t.Fatal("storage import started despite the refusal")
	}
	if tasks, _ := st.Tasks().List(ctx); len(tasks) != 0 {
		t.Fatalf("a task was recorded: %#v", tasks)
	}

	// 空间足够时同一请求放行。
	fake.space.PoolAvailable = 1 << 30
	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: source, OSType: domain.OSTypeWindows}); err != nil {
		t.Fatalf("import with room = %v", err)
	}
}

func TestImageServiceListImagesReturnsEmptyArray(t *testing.T) {
	result, err := (ImageService{Store: newImageTestStore(t), Storage: &fakeImageStorage{}}).ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Items == nil || result.Total != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func newImageTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "image.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type fakeImageStorage struct {
	dependents map[string][]string
	storage.StorageAgent
	// space 是 SpaceUsage 的应答；为 nil 时视为空间充足的空池。
	space            *storage.SpaceUsage
	spaceErr         error
	err              error
	now              time.Time
	deleted          string
	deletedConfigs   []string
	req              storage.ImportImageReq
	importCalled     bool
	rollbackCalled   bool
	rollbackReq      storage.ImportImageReq
	rollbackImported storage.ImportImageResult
	onImport         func(storage.ImportImageReq)
	inspectReq       storage.InspectImageReq
	inspectReport    storage.InspectReport
	inspectErr       error
	scriptInjected   bool
	// exportData 是 ExportImage 输出的流；exportSize 是 ExportImageSize 的返回值（0 表示取较小默认值）。
	exportData []byte
	exportErr  error
	exportSize int64
	exportReq  storage.ExportImageReq
}

func (s *fakeImageStorage) ExportImage(_ context.Context, req storage.ExportImageReq) error {
	s.exportReq = req
	if len(s.exportData) > 0 {
		if _, err := req.W.Write(s.exportData); err != nil {
			return err
		}
		if req.OnProgress != nil {
			req.OnProgress(int64(len(s.exportData)), int64(len(s.exportData)))
		}
	}
	return s.exportErr
}

func (s *fakeImageStorage) ExportImageSize(context.Context, string) (int64, error) {
	if s.exportSize > 0 {
		return s.exportSize, nil
	}
	return 1 << 20, nil
}

func (s *fakeImageStorage) SpaceUsage(context.Context) (storage.SpaceUsage, error) {
	if s.spaceErr != nil {
		return storage.SpaceUsage{}, s.spaceErr
	}
	if s.space != nil {
		return *s.space, nil
	}
	return storage.SpaceUsage{PoolAvailable: 1 << 50, Volumes: map[string]storage.VolumeUsage{}}, nil
}

func (s *fakeImageStorage) InspectImage(_ context.Context, req storage.InspectImageReq) (storage.InspectReport, error) {
	s.inspectReq = req
	if s.inspectErr != nil {
		return storage.InspectReport{}, s.inspectErr
	}
	if s.inspectReport.Level == "" {
		return storage.InspectReport{Level: domain.HealthOK, PartitionStyle: "mbr"}, nil
	}
	return s.inspectReport, nil
}

func (s *fakeImageStorage) ImportImage(_ context.Context, req storage.ImportImageReq) (storage.ImportImageResult, error) {
	s.importCalled = true
	s.req = req
	if s.onImport != nil {
		s.onImport(req)
	}
	if s.err != nil {
		return storage.ImportImageResult{}, s.err
	}
	reductionID := req.Name + "_0"
	return storage.ImportImageResult{
		Image: domain.Image{
			ID:        req.Name,
			Name:      req.Name,
			OSType:    req.OSType,
			State:     domain.ImageStateNormal,
			CreatedAt: s.now,
		},
		Config: domain.Config{
			ID:                 req.Name + "_default",
			ImageID:            req.Name,
			Name:               "default",
			DefaultReductionID: &reductionID,
			CreatedAt:          s.now,
		},
		Reduction: domain.Reduction{
			ID:        reductionID,
			ConfigID:  req.Name + "_default",
			Name:      "@0",
			CreatedAt: s.now,
			Status:    domain.ReductionStatusReady,
		},
		MountScriptInjected: s.scriptInjected,
	}, nil
}

func (s *fakeImageStorage) DeleteImage(_ context.Context, imageID string, configIDs []string) error {
	s.deletedConfigs = append(s.deletedConfigs, configIDs...)
	s.deleted = imageID
	return s.err
}

func (s *fakeImageStorage) ConfigDependents(_ context.Context, configID string) ([]string, error) {
	return s.dependents[configID], nil
}

func (s *fakeImageStorage) DeleteConfig(_ context.Context, configID string) error {
	s.deletedConfigs = append(s.deletedConfigs, configID)
	return s.err
}

func (s *fakeImageStorage) RollbackImportImage(_ context.Context, req storage.ImportImageReq, imported storage.ImportImageResult) error {
	s.rollbackCalled = true
	s.rollbackReq = req
	s.rollbackImported = imported
	return s.err
}

func TestImageServiceRecordsBakedVersionOnlyWhenInjected(t *testing.T) {
	// 开机路径凭记录的版本跳过注入，所以只有存储层确认脚本写入后才能记录，否则客户机没有网关。
	for _, tc := range []struct {
		name     string
		injected bool
		want     string
	}{
		{"bake succeeded", true, "v1-abc"},
		{"bake failed", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			agent := &fakeImageStorage{now: time.Now().UTC(), scriptInjected: tc.injected}
			service := ImageService{
				Store: st, Storage: agent,
				MountScriptBake: func(domain.OSType) ([]byte, string) { return []byte("# script"), "v1-abc" },
			}

			if _, err := service.ImportImage(ctx, ImportImageRequest{
				Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows,
			}); err != nil {
				t.Fatal(err)
			}
			if string(agent.req.MountScript) != "# script" {
				t.Fatalf("script not handed to storage: %q", agent.req.MountScript)
			}
			img, err := st.Images().Get(ctx, "win11")
			if err != nil {
				t.Fatal(err)
			}
			if img.MountScriptVersion != tc.want {
				t.Fatalf("MountScriptVersion = %q, want %q", img.MountScriptVersion, tc.want)
			}
		})
	}
}

func TestImportImageBakesTheScriptForItsOS(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakeImageStorage{now: time.Now().UTC(), scriptInjected: true}
	service := ImageService{Store: st, Storage: agent,
		MountScriptBake: func(os domain.OSType) ([]byte, string) { return []byte("# " + string(os)), "v-" + string(os) }}
	if _, err := service.ImportImage(ctx, ImportImageRequest{
		Name: "ubuntu", SourcePath: "/var/lib/ndiskless/imports/ubuntu.zfs", OSType: domain.OSTypeLinux,
	}); err != nil {
		t.Fatal(err)
	}
	img, err := st.Images().Get(ctx, "ubuntu")
	if string(agent.req.MountScript) != "# linux" || err != nil || img.MountScriptVersion != "v-linux" {
		t.Fatalf("script=%q version=%q err=%v", agent.req.MountScript, img.MountScriptVersion, err)
	}
}

// 中文镜像名原样传给存储层（只去首尾空白），只有数据集标识需要 ASCII。
func TestImportImageKeepsTheNameAsTyped(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	storage := &fakeImageStorage{}
	svc := ImageService{Store: st, Storage: storage, ImportDir: "/imports"}

	if _, err := svc.ImportImage(ctx, ImportImageRequest{
		Name: "  教学镜像win11 ", SourcePath: "/imports/a.vmdk", OSType: domain.OSTypeWindows,
	}); err != nil {
		t.Fatal(err)
	}
	if storage.req.Name != "教学镜像win11" {
		t.Fatalf("name passed to storage = %q, want it kept (trimmed only)", storage.req.Name)
	}
}

// 合并在 promote 与 rename 之间中断时，配置成了镜像数据集的父级。此时删除镜像
// 不能把镜像自己的数据集当成阻碍对象。
func TestImageServiceDeleteWorksWhenAConfigWasPromotedOverIt(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	img := domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	for _, create := range []func() error{
		func() error { return st.Images().Create(ctx, img) },
		func() error { return st.Configs().Create(ctx, cfg) },
		func() error {
			return st.Reductions().Create(ctx, domain.Reduction{ID: "red-1", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady})
		},
	} {
		if err := create(); err != nil {
			t.Fatal(err)
		}
	}
	// promote 之后池的报告：镜像挂在配置下面。
	agent := &fakeImageStorage{dependents: map[string][]string{cfg.ID: {"tank/" + img.ID}}}
	service := ImageService{Store: st, Storage: agent}

	if err := service.DeleteImage(ctx, img.ID); err != nil {
		t.Fatalf("delete refused: %v", err)
	}
	if agent.deleted != img.ID {
		t.Fatalf("storage delete = %q", agent.deleted)
	}
	if _, err := st.Images().Get(ctx, img.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("image row survived: %v", err)
	}
}

// 导入成功后删除浏览器上传产生的源文件，但操作者自己放置的文件不能删。
func TestImportRemovesTheUploadedSourceButNotTheOperatorsOwn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	run := func(t *testing.T, deleteAfter bool) string {
		t.Helper()
		src := filepath.Join(dir, fmt.Sprintf("win-%v.zfs", deleteAfter))
		if err := os.WriteFile(src, []byte("stream"), 0o600); err != nil {
			t.Fatal(err)
		}
		st := newImageTestStore(t)
		now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
		service := ImageService{Store: st, Storage: &fakeImageStorage{now: now}, Now: func() time.Time { return now },
			ImportDir: dir}
		if _, err := service.ImportImage(ctx, ImportImageRequest{
			Name: "win11", SourcePath: src, OSType: domain.OSTypeWindows,
			DeleteSourceAfter: deleteAfter,
		}); err != nil {
			t.Fatal(err)
		}
		return src
	}

	uploaded := run(t, true)
	if _, err := os.Stat(uploaded); !os.IsNotExist(err) {
		t.Fatalf("上传上来的源文件导完之后应当删掉，它白占几十 G：err=%v", err)
	}

	own := run(t, false)
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("运维自己放的文件不能动：%v", err)
	}
}

// 本产品压缩导出是 .zfs.gz，但下载常被改名、其他工具产出 .gz，这些扩展名都要接受。
func TestImportAcceptsGzExtension(t *testing.T) {
	for _, name := range []string{"win10.zfs.gz", "win10.gz", "win10.img.gz", "win10.gzip", "win10.zfs"} {
		if err := validateImportImageSource("/var/lib/nd/import/"+name, "/var/lib/nd/import"); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	if err := validateImportImageSource("/var/lib/nd/import/win10.txt", "/var/lib/nd/import"); !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "win10.txt") {
		t.Error("a .txt was accepted as an image")
	}
}

// 压缩源的空间预检要按解压后大小估算，按文件大小会放过导入途中撑满池的情况。
func TestImportSizeEstimateAccountsForGzip(t *testing.T) {
	dir := t.TempDir()
	raw := bytes.Repeat([]byte("zfs-stream-payload"), 4096) // 73728 字节，高度可压缩
	plain := filepath.Join(dir, "image.zfs")
	if err := os.WriteFile(plain, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(dir, "image.zfs.gz")
	f, err := os.Create(packed)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(packed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= int64(len(raw)) {
		t.Fatalf("test data did not compress: %d >= %d", info.Size(), len(raw))
	}

	got, ok := importStreamSize(packed)
	if !ok {
		t.Fatal("no estimate for a gzip source")
	}
	if got != int64(len(raw)) {
		t.Fatalf("gzip estimate = %d, want the uncompressed %d", got, len(raw))
	}
	if plainSize, ok := importStreamSize(plain); !ok || plainSize <= 0 {
		t.Fatalf("plain estimate = %d ok=%v", plainSize, ok)
	}
}

// blockingImport 让导入停在拷贝阶段，直到测试放行，模拟库行尚未写入的长拷贝。
func blockingImport(now time.Time) (*fakeImageStorage, chan struct{}, chan struct{}) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	return &fakeImageStorage{now: now, onImport: func(storage.ImportImageReq) {
		entered <- struct{}{}
		<-release
	}}, entered, release
}

// 同名导入正在拷贝时（库里还没有行），第二次提交要当场点名拒绝；导入结束后名字回到按库查重。
func TestImportOfANameAlreadyBeingImportedIsRefusedAtSubmit(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	slow, entered, release := blockingImport(now)
	svc := ImageService{Store: st, Storage: slow, Async: true, Now: func() time.Time { return now }}
	first, err := svc.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/a.zfs", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	<-entered

	other := ImageService{Store: st, Storage: &fakeImageStorage{now: now}, Now: func() time.Time { return now }}
	_, err = other.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/b.zfs", OSType: domain.OSTypeWindows})
	_, saveErr := (ImageService{Store: seedRestorePointInto(t, st), Storage: &copyingImageStorage{}}).SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "win11"})
	close(release)
	waitTaskDone(t, st, first.TaskID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win11") || !strings.Contains(err.Error(), "正在导入") {
		t.Fatalf("同名导入进行中时应点名拒绝，得到 %v", err)
	}
	if !errors.Is(saveErr, errs.ErrConflict) || !strings.Contains(saveErr.Error(), "正在导入") {
		t.Fatalf("同名导入进行中时另存为应点名拒绝，得到 %v", saveErr)
	}
	if _, err := other.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/b.zfs", OSType: domain.OSTypeWindows}); !errors.Is(err, ErrImageExists) {
		t.Fatalf("导入完成后应按库里已有同名拒绝，得到 %v", err)
	}
}

// 纯中文名都折成同一个兜底 ID：前一个还在拷贝时，后一个会落到同一数据集上，必须当场拒绝并点名前一个。
func TestImportsWhoseNamesFoldToTheSameIDAreRefused(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	slow, entered, release := blockingImport(now)
	svc := ImageService{Store: st, Storage: slow, Async: true, Now: func() time.Time { return now }}
	first, err := svc.ImportImage(ctx, ImportImageRequest{Name: "教学一班", SourcePath: "/var/lib/ndiskless/imports/a.zfs", OSType: domain.OSTypeWindows})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	_, err = (ImageService{Store: st, Storage: &fakeImageStorage{now: now}}).ImportImage(ctx, ImportImageRequest{Name: "教学二班", SourcePath: "/var/lib/ndiskless/imports/b.zfs", OSType: domain.OSTypeWindows})
	close(release)
	waitTaskDone(t, st, first.TaskID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "教学一班") {
		t.Fatalf("折成同一 ID 的导入应点名前一个拒绝，得到 %v", err)
	}
}

// 导入失败也要释放名字，否则操作者改了参数也重导不了。
func TestFailedImportReleasesTheName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	req := ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/a.zfs", OSType: domain.OSTypeWindows}
	if _, err := (ImageService{Store: st, Storage: &fakeImageStorage{now: now, err: errors.New("zfs failed")}}).ImportImage(ctx, req); err == nil {
		t.Fatal("导入应失败")
	}
	if _, err := (ImageService{Store: st, Storage: &fakeImageStorage{now: now}}).ImportImage(ctx, req); err != nil {
		t.Fatalf("失败后应能重导: %v", err)
	}
}
