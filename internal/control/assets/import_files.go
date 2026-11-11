package assets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// ImportFileService 删除导入目录里的成品文件。文件库随复制到每台，所以只在写入者上删，备机下一轮跟随；
// 闸门挡住了备机上的写请求。
type ImportFileService struct {
	Store     store.Store
	ImportDir func(context.Context) (string, error)
	// AfterDelete 在真的删了文件后调用：页面要马上看到变化，旧复制标记占着的空间也要安排释放。
	AfterDelete func(context.Context)
}

// Delete 删除一个导入文件。已经不在了算成功：页面最多晚 10 秒看到变化。
func (s ImportFileService) Delete(ctx context.Context, name string) error {
	// 名字来自请求。只接受 imports 下一层的文件；点开头的是上传分片和其他内部文件。
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return errs.Invalid(fmt.Sprintf("请从导入文件列表里选择要删除的文件：%q 不是导入文件", name))
	}
	dir, err := s.ImportDir(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读不到导入文件 %s：%w", name, err)
	}
	if !info.Mode().IsRegular() {
		return errs.Invalid(fmt.Sprintf("请到服务器上处理 %s：它不是普通文件，这里只能删导入的文件", name))
	}
	if err := s.refuseInUse(ctx, dir, name); err != nil {
		return err
	}
	done := storage.ChangeCatalogue()
	err = os.Remove(path)
	done()
	if err != nil {
		return fmt.Errorf("删不掉导入文件 %s：%w", name, err)
	}
	slog.Info("import file deleted", "name", name, "size", info.Size())
	if s.AfterDelete != nil {
		s.AfterDelete(ctx)
	}
	return nil
}

func (s ImportFileService) refuseInUse(ctx context.Context, dir, name string) error {
	tasks, err := s.Store.Tasks().ListActive(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		// 任务不记录文件路径，分不清是不是这个文件，只能都等。
		if t.Type == domain.TaskTypeImportImage || t.Type == domain.TaskTypeExportImage {
			return errs.Conflict(fmt.Sprintf("请先等任务中心里的导入或导出任务结束，再删除 %s：它们可能正在读写这个文件", name))
		}
	}
	sessions, err := s.Store.InstallSessions().List(ctx)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sess.Active() && usesImportFile(sess.SourcePath, dir, name) {
			who := sess.Name
			if who == "" {
				who = sess.MAC
			}
			return errs.Conflict(fmt.Sprintf("请先等 %s 装完或取消这次安装，再删除 %s：安装正在用这个文件", who, name))
		}
	}
	return nil
}

// usesImportFile 判断安装记下的路径是不是这个文件：一般是文件库内的相对路径，读不到文件库时记的是绝对路径。
// 旧导入目录里的同名文件是另一个文件。
func usesImportFile(source, dir, name string) bool {
	source = filepath.Clean(source)
	return source == filepath.Join("imports", name) || source == filepath.Join(dir, name)
}
