package assets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

type importFilesFixture struct {
	svc     ImportFileService
	st      *store.SQLStore
	dir     string
	deleted int
}

func newImportFiles(t *testing.T) *importFilesFixture {
	t.Helper()
	f := &importFilesFixture{st: newImageTestStore(t), dir: filepath.Join(t.TempDir(), "imports")}
	if err := os.MkdirAll(filepath.Join(f.dir, ".uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.svc = ImportFileService{
		Store:       f.st,
		ImportDir:   func(context.Context) (string, error) { return f.dir, nil },
		AfterDelete: func(context.Context) { f.deleted++ },
	}
	return f
}

func (f *importFilesFixture) put(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDeleteImportFileRemovesItAndAsksSpaceToBeReleased(t *testing.T) {
	f := newImportFiles(t)
	path := f.put(t, "Win11 中文版.iso")
	other := f.put(t, "CentOS.iso")
	if err := f.svc.Delete(context.Background(), "Win11 中文版.iso"); err != nil {
		t.Fatal(err)
	}
	if exists(path) {
		t.Fatal("文件还在")
	}
	if !exists(other) {
		t.Fatal("误删了别的文件")
	}
	if f.deleted != 1 {
		t.Fatalf("删除后没有安排释放空间：%d", f.deleted)
	}
}

// 页面最多晚 10 秒看到变化，两个人同时点、或者列表还没刷新，再删一次都不该报错。
func TestDeleteImportFileAlreadyGoneIsNotAnError(t *testing.T) {
	f := newImportFiles(t)
	if err := f.svc.Delete(context.Background(), "gone.iso"); err != nil {
		t.Fatal(err)
	}
	if f.deleted != 0 {
		t.Fatal("什么都没删，不用安排释放空间")
	}
}

// 这个接口只能动 imports 下的成品文件：名字来自请求，不能走到目录外，也不能删上传中的分片。
func TestDeleteImportFileOnlyTouchesFilesInsideImports(t *testing.T) {
	f := newImportFiles(t)
	outside := filepath.Join(filepath.Dir(f.dir), "outside.iso")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../outside.iso", "a/../../outside.iso", "/etc/passwd", ".uploads", ".hidden", "sub/x.iso"} {
		err := f.svc.Delete(context.Background(), name)
		if !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("%q 应当被拒绝：%v", name, err)
		}
	}
	if !exists(outside) || !exists(filepath.Join(f.dir, ".uploads")) || !exists(filepath.Join(f.dir, ".hidden")) {
		t.Fatal("拒绝的请求动了文件")
	}
}

func TestDeleteImportFileRefusesWhatIsNotAPlainFile(t *testing.T) {
	f := newImportFiles(t)
	if err := os.MkdirAll(filepath.Join(f.dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := f.put(t, "real.iso")
	if err := os.Symlink(target, filepath.Join(f.dir, "link.iso")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"folder", "link.iso"} {
		if err := f.svc.Delete(context.Background(), name); !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("%q 不是普通文件，应当被拒绝：%v", name, err)
		}
	}
	if !exists(filepath.Join(f.dir, "folder")) || !exists(filepath.Join(f.dir, "link.iso")) || !exists(target) {
		t.Fatal("拒绝的请求动了文件")
	}
}

// 导入和导出任务不记录文件路径，分不清是不是这个文件，有任务在跑就都拦下。
func TestDeleteImportFileWaitsForImportAndExportTasks(t *testing.T) {
	for _, typ := range []domain.TaskType{domain.TaskTypeImportImage, domain.TaskTypeExportImage} {
		t.Run(string(typ), func(t *testing.T) {
			f := newImportFiles(t)
			path := f.put(t, "win.iso")
			ctx := context.Background()
			task := domain.Task{ID: "t1", Type: typ, TargetRef: "win11", Status: domain.TaskStatusRunning, CreatedAt: time.Now()}
			if err := f.st.Tasks().Create(ctx, task); err != nil {
				t.Fatal(err)
			}
			err := f.svc.Delete(ctx, "win.iso")
			if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win.iso") || !strings.HasPrefix(err.Error(), "请") {
				t.Fatalf("要先说该做什么，并点名文件：%v", err)
			}
			if !exists(path) || f.deleted != 0 {
				t.Fatal("被拒绝时文件不能动")
			}
			task.Status = domain.TaskStatusSuccess
			if err := f.st.Tasks().Update(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Delete(ctx, "win.iso"); err != nil {
				t.Fatalf("任务结束后应当能删：%v", err)
			}
		})
	}
}

// 别的任务（比如建还原点）和导入文件无关，不该挡住。
func TestDeleteImportFileIgnoresUnrelatedTasks(t *testing.T) {
	f := newImportFiles(t)
	f.put(t, "win.iso")
	ctx := context.Background()
	if err := f.st.Tasks().Create(ctx, domain.Task{ID: "t1", Type: domain.TaskTypeCreateReduction, Status: domain.TaskStatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(ctx, "win.iso"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteImportFileRefusesWhileAnInstallUsesIt(t *testing.T) {
	cases := map[string]func(dir string) string{
		"文件库里的相对路径":   func(string) string { return "imports/win.iso" },
		"没记相对路径的绝对路径": func(dir string) string { return filepath.Join(dir, "win.iso") },
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			f := newImportFiles(t)
			path := f.put(t, "win.iso")
			ctx := context.Background()
			sess := domain.InstallSession{ID: "i1", MAC: "005056aabbcc", Token: "tok", Name: "教室一号机", Phase: "installing", SourcePath: source(f.dir)}
			if err := f.st.InstallSessions().Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			err := f.svc.Delete(ctx, "win.iso")
			if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "教室一号机") || !strings.Contains(err.Error(), "win.iso") {
				t.Fatalf("要点名哪台机器在用：%v", err)
			}
			if !exists(path) {
				t.Fatal("被拒绝时文件不能动")
			}
			sess.Phase = "ready"
			if err := f.st.InstallSessions().Update(ctx, sess); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Delete(ctx, "win.iso"); err != nil {
				t.Fatalf("装完以后镜像已经独立，应当能删：%v", err)
			}
		})
	}
}

func TestDeleteImportFileIgnoresInstallsOfOtherFiles(t *testing.T) {
	f := newImportFiles(t)
	f.put(t, "win.iso")
	ctx := context.Background()
	// 旧导入目录里的同名文件是另一个文件。
	for i, source := range []string{"imports/other.iso", "/var/lib/ndiskless/imports/win.iso"} {
		sess := domain.InstallSession{ID: "i" + string(rune('0'+i)), MAC: "00505600000" + string(rune('0'+i)), Token: "t" + string(rune('0'+i)), Phase: "installing", SourcePath: source}
		if err := f.st.InstallSessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Delete(ctx, "win.iso"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteImportFileWithoutAPoolTellsWhatToDo(t *testing.T) {
	f := newImportFiles(t)
	f.svc.ImportDir = func(context.Context) (string, error) { return "", errs.Invalid("请先创建存储池") }
	if err := f.svc.Delete(context.Background(), "win.iso"); !errors.Is(err, errs.ErrInvalid) {
		t.Fatal(err)
	}
}
