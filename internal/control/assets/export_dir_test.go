package assets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// 导出到服务器目录走本机盘、不经网络，但文件落在当前服务的节点上，
// 所以结果必须写明是哪台节点。
func TestExportImageToDirWritesIntoTheImportDir(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	dir := t.TempDir()
	seedExportImage(t, st, "win11")
	svc := ImageService{Store: st, Storage: &fakeImageStorage{exportData: []byte("stream")}, ImportDir: dir, Async: false}

	res, err := svc.ExportImageToDir(ctx, ImageExportRequest{ImageID: "win11"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TaskID == "" {
		t.Fatalf("没有留下任务：%#v", res)
	}
	if !strings.HasSuffix(res.Path, ".zfs") || filepath.Dir(res.Path) != dir {
		t.Fatalf("落点不对：%q", res.Path)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("文件没写出来：%v", err)
	}
	task, err := st.Tasks().Get(ctx, res.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskStatusSuccess {
		t.Fatalf("任务 = %#v", task)
	}
}

// 同名文件已存在时不能静默覆盖，那份可能正被别人取用。
func TestExportImageToDirRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	dir := t.TempDir()
	seedExportImage(t, st, "win11")
	svc := ImageService{Store: st, Storage: &fakeImageStorage{exportData: []byte("stream")}, ImportDir: dir, Async: false}

	if _, err := svc.ExportImageToDir(ctx, ImageExportRequest{ImageID: "win11"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ExportImageToDir(ctx, ImageExportRequest{ImageID: "win11"})
	if err == nil {
		t.Fatal("同名文件被覆盖了")
	}
	if !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("错误没说清原因：%v", err)
	}
}

func seedExportImage(t *testing.T, st *store.SQLStore, id string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.Images().Create(context.Background(), domain.Image{
		ID: id, Name: id, OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// 还原点也能盘对盘导出到服务器目录，落点和镜像导出一样写明是哪台节点。
func TestExportReductionToDirWritesTheRestorePoint(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	dir := t.TempDir()
	seedGroupTriple(t, ctx, st, "win11", "cfg-1", "red-1")
	fake := &fakeImageStorage{exportData: []byte("stream")}
	svc := ImageService{Store: st, Storage: fake, ImportDir: dir, NodeAddr: "192.168.10.3", Async: false}

	res, err := svc.ExportReductionToDir(ctx, "red-1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(res.Path) != dir || !strings.HasPrefix(filepath.Base(res.Path), "win11-") || !strings.HasSuffix(res.Path, ".zfs") {
		t.Fatalf("落点不对：%q", res.Path)
	}
	if res.Node != "192.168.10.3" {
		t.Fatalf("没写明是哪台：%#v", res)
	}
	if fake.exportReq.ConfigID != "cfg-1" || fake.exportReq.Snapshot != "0" {
		t.Fatalf("导出的不是这个还原点：%#v", fake.exportReq)
	}
	if b, err := os.ReadFile(res.Path); err != nil || string(b) != "stream" {
		t.Fatalf("文件没写对：%q %v", b, err)
	}
	if task, err := st.Tasks().Get(ctx, res.TaskID); err != nil || task.Status != domain.TaskStatusSuccess {
		t.Fatalf("任务 = %#v %v", task, err)
	}
}
