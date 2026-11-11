package local

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// mountedFilesFloor：文件库数据集占用超过它、目录却是空的，说明没挂载（备机副本经 recv -u 收下）。
const mountedFilesFloor = 1 << 20

// PoolFacts 报告本节点一个池里的全部内容。只读，不建也不挂任何东西。
func (a *Agent) PoolFacts(ctx context.Context, pool string) (storage.PoolFacts, error) {
	names, err := a.zfs.ListPoolNames(ctx)
	if err != nil {
		return storage.PoolFacts{}, err
	}
	if !slices.Contains(names, pool) {
		return storage.PoolFacts{}, fmt.Errorf("这台服务器上没有存储池 %s", pool)
	}
	facts, err := a.zfs.ListPoolFacts(ctx, pool)
	if err != nil {
		return storage.PoolFacts{}, err
	}
	facts.Node, facts.DataPool = a.server, a.zfs.PoolName()
	if pool == a.zfs.PoolName() {
		files := a.readFiles(pool, facts.Datasets)
		facts.Files = &files
	}
	return facts, nil
}

func (a *Agent) readFiles(pool string, datasets []storage.PoolDatasetFact) storage.PoolFilesFact {
	dirOf := a.filesDirFn
	if dirOf == nil {
		dirOf = zfs.FilesDir
	}
	dir := dirOf(pool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return storage.PoolFilesFact{}
	}
	if len(entries) == 0 {
		for _, d := range datasets {
			if d.Name == storage.CatalogueRoot+"/files" && d.Used > mountedFilesFloor {
				return storage.PoolFilesFact{}
			}
		}
	}
	files := storage.PoolFilesFact{Readable: true}
	for _, e := range entries {
		if e.Name() == "imports" && e.IsDir() {
			imports, partial := readImports(filepath.Join(dir, "imports"))
			files.Imports = imports
			if partial.Used > 0 || partial.Size > 0 {
				files.Dirs = append(files.Dirs, partial)
			}
			continue
		}
		files.Dirs = append(files.Dirs, sumTree(filepath.Join(dir, e.Name()), e.Name()))
	}
	return files
}

// readImports 列导入目录里的文件。隐藏目录是没传完的分片，合计成一项：它占空间，但不是操作者认识的文件。
func readImports(dir string) ([]storage.PoolFileFact, storage.PoolFileFact) {
	var out []storage.PoolFileFact
	partial := storage.PoolFileFact{Name: "imports/.hidden"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, partial
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			hidden := sumTree(filepath.Join(dir, e.Name()), "imports/"+e.Name())
			partial.Name = hidden.Name
			partial.Size += hidden.Size
			partial.Used += hidden.Used
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, storage.PoolFileFact{Name: e.Name(), Size: info.Size(), Used: physicalSize(info), Modified: info.ModTime().Unix()})
	}
	return out, partial
}

func sumTree(root, name string) storage.PoolFileFact {
	sum := storage.PoolFileFact{Name: name}
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			sum.Size += info.Size()
			sum.Used += physicalSize(info)
		}
		return nil
	})
	return sum
}
