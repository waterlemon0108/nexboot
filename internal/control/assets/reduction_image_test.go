package assets

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

type copyingImageStorage struct {
	fakeImageStorage
	copyReq storage.CopyReductionReq
	copyErr error
	onCopy  func(storage.CopyReductionReq)
}

func (s *copyingImageStorage) CopyReductionToImage(_ context.Context, req storage.CopyReductionReq) (storage.ImportImageResult, error) {
	s.copyReq = req
	if s.onCopy != nil {
		s.onCopy(req)
	}
	if s.copyErr != nil {
		return storage.ImportImageResult{}, s.copyErr
	}
	now := time.Now().UTC()
	red := "vmdk2_default_0"
	return storage.ImportImageResult{
		Image:     domain.Image{ID: "vmdk2", Name: req.Name, OSType: req.OSType, Purpose: req.Purpose, State: domain.ImageStateNormal, Origin: domain.ImageOriginImported, CreatedAt: now},
		Config:    domain.Config{ID: "vmdk2_default", ImageID: "vmdk2", Name: "default", DefaultReductionID: &red, CreatedAt: now},
		Reduction: domain.Reduction{ID: red, ConfigID: "vmdk2_default", Name: "@0", Status: domain.ReductionStatusReady, CreatedAt: now},
	}, nil
}

func seedRestorePoint(t *testing.T) *store.SQLStore {
	t.Helper()
	return seedRestorePointInto(t, newImageTestStore(t))
}

func seedRestorePointInto(t *testing.T, st *store.SQLStore) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	red := "vmdk_default_r1"
	for _, err := range []error{
		st.Images().Create(ctx, domain.Image{ID: "vmdk", Name: "vmdk", OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeSystem, State: domain.ImageStateNormal, MountScriptVersion: "v-win", CreatedAt: now}),
		st.Configs().Create(ctx, domain.Config{ID: "vmdk_default", ImageID: "vmdk", Name: "default", DefaultReductionID: &red, CreatedAt: now}),
		st.Reductions().Create(ctx, domain.Reduction{ID: red, ConfigID: "vmdk_default", Name: "@r1", DisplayName: "还原点1", Status: domain.ReductionStatusReady, CreatedAt: now}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestStreamReductionExportsThatRestorePoint(t *testing.T) {
	agent := &copyingImageStorage{}
	svc := ImageService{Store: seedRestorePoint(t), Storage: agent}
	name, err := svc.ReductionExportName(context.Background(), "vmdk_default_r1", false)
	if err != nil || name != "vmdk-还原点1.zfs" {
		t.Fatalf("name = %q err = %v", name, err)
	}
	if err := svc.StreamReduction(context.Background(), "vmdk_default_r1", &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if agent.exportReq.ConfigID != "vmdk_default" || agent.exportReq.Snapshot != "r1" {
		t.Fatalf("export req = %#v", agent.exportReq)
	}
}

func TestSaveReductionAsImageKeepsTheSourcesKind(t *testing.T) {
	ctx := context.Background()
	st := seedRestorePoint(t)
	agent := &copyingImageStorage{}
	svc := ImageService{Store: st, Storage: agent}
	res, err := svc.SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: " 教学机-办公版 "})
	if err != nil {
		t.Fatal(err)
	}
	if agent.copyReq.ConfigID != "vmdk_default" || agent.copyReq.Snapshot != "r1" || agent.copyReq.Name != "教学机-办公版" ||
		agent.copyReq.OSType != domain.OSTypeWindows || agent.copyReq.Purpose != domain.ImagePurposeSystem {
		t.Fatalf("copy req = %#v", agent.copyReq)
	}
	img, err := st.Images().Get(ctx, "vmdk2")
	if err != nil || img.Name != "教学机-办公版" || img.MountScriptVersion != "v-win" {
		t.Fatalf("image = %#v err = %v", img, err)
	}
	task, err := st.Tasks().Get(ctx, res.TaskID)
	if err != nil || task.Type != domain.TaskTypeCopyImage || task.Status != domain.TaskStatusSuccess {
		t.Fatalf("task = %#v err = %v", task, err)
	}
}

// 另存为期间新镜像数据集先于库行出现，任务目标须是新镜像，一致性检查才不会把它当残留。
func TestSaveReductionAsImageRecordsTheNewImageWhileCopying(t *testing.T) {
	ctx := context.Background()
	st := seedRestorePoint(t)
	var midRunRef string
	agent := &copyingImageStorage{onCopy: func(req storage.CopyReductionReq) {
		req.OnTarget("vmdk2")
		req.OnProgress(50, "复制 50%")
		active, err := st.Tasks().ListActive(ctx)
		if err != nil || len(active) != 1 {
			t.Fatalf("active = %#v err = %v", active, err)
		}
		midRunRef = active[0].TargetRef
	}}
	if _, err := (ImageService{Store: st, Storage: agent}).SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "教学机-办公版"}); err != nil {
		t.Fatal(err)
	}
	if midRunRef != "vmdk2" {
		t.Fatalf("TargetRef while copying = %q", midRunRef)
	}
}

func TestSaveReductionAsImageRefusesUpFront(t *testing.T) {
	ctx := context.Background()
	st := seedRestorePoint(t)
	svc := ImageService{Store: st, Storage: &copyingImageStorage{}}
	if _, err := svc.SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "  "}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("blank name err = %v", err)
	}
	if _, err := svc.SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "vmdk"}); !errors.Is(err, ErrImageExists) {
		t.Fatalf("duplicate name err = %v", err)
	}
	tight := &copyingImageStorage{fakeImageStorage: fakeImageStorage{exportSize: 10 << 30, space: &storage.SpaceUsage{PoolAvailable: 1 << 30}}}
	if _, err := (ImageService{Store: st, Storage: tight}).SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "x"}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("no room err = %v", err)
	}
	if _, err := svc.SaveReductionAsImage(ctx, "missing", SaveAsImageRequest{Name: "x"}); !errs.IsNotFound(err) {
		t.Fatalf("unknown restore point err = %v", err)
	}
}

// 另存为在拷贝期间同样没有库行：同名的导入要当场拒绝，并在另存为结束后释放名字。
func TestImportIsRefusedWhileASaveAsOfTheSameNameCopies(t *testing.T) {
	ctx := context.Background()
	st := seedRestorePoint(t)
	entered, release := make(chan struct{}), make(chan struct{})
	agent := &copyingImageStorage{onCopy: func(storage.CopyReductionReq) {
		close(entered)
		<-release
	}}
	res, err := (ImageService{Store: st, Storage: agent, Async: true}).SaveReductionAsImage(ctx, "vmdk_default_r1", SaveAsImageRequest{Name: "办公版"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	_, err = (ImageService{Store: st, Storage: &fakeImageStorage{}}).ImportImage(ctx, ImportImageRequest{Name: "办公版", SourcePath: "/var/lib/ndiskless/imports/a.zfs", OSType: domain.OSTypeWindows})
	close(release)
	waitTaskDone(t, st, res.TaskID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "办公版") || !strings.Contains(err.Error(), "另存为") {
		t.Fatalf("另存为进行中时同名导入应点名拒绝，得到 %v", err)
	}
}
