package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func factsAgent(t *testing.T) (*Agent, *fakePool, string) {
	t.Helper()
	z := newFakePool()
	z.poolNames = []string{"tank", "backup"}
	z.poolFacts = map[string]storage.PoolFacts{
		"tank":   {Pool: "tank", Used: 100, Datasets: []storage.PoolDatasetFact{{Name: "nd/files", Used: 4096}}},
		"backup": {Pool: "backup", Used: 50},
	}
	dir := t.TempDir()
	a := New("node-a", z, &fakeExporter{})
	a.filesDirFn = func(string) string { return dir }
	return a, z, dir
}

func TestPoolFactsReportsTheDataPoolWithItsFiles(t *testing.T) {
	a, _, dir := factsAgent(t)
	writeFile(t, filepath.Join(dir, "imports", "win.iso"), 8192)
	writeFile(t, filepath.Join(dir, "imports", ".uploads", "half.part"), 4096)
	writeFile(t, filepath.Join(dir, "winpe", "boot.wim"), 16384)
	writeFile(t, filepath.Join(dir, "winpe", "sub", "bcd"), 4096)

	got, err := a.PoolFacts(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if got.Node != "node-a" || got.DataPool != "tank" || got.Files == nil || !got.Files.Readable {
		t.Fatalf("got %+v", got)
	}
	if len(got.Files.Imports) != 1 || got.Files.Imports[0].Name != "win.iso" || got.Files.Imports[0].Size != 8192 {
		t.Fatalf("导入文件不对：%+v", got.Files.Imports)
	}
	dirs := map[string]int64{}
	for _, d := range got.Files.Dirs {
		dirs[d.Name] = d.Size
	}
	if dirs["winpe"] != 20480 || dirs["imports/.uploads"] != 4096 {
		t.Fatalf("目录合计不对：%+v", got.Files.Dirs)
	}
	if _, listed := dirs["imports"]; listed {
		t.Fatalf("导入文件不该再算进目录：%+v", got.Files.Dirs)
	}
}

// 备份池没有文件库，也不能去读数据池的。
func TestPoolFactsLeavesFilesOutForTheBackupPool(t *testing.T) {
	a, _, dir := factsAgent(t)
	writeFile(t, filepath.Join(dir, "imports", "win.iso"), 8192)
	got, err := a.PoolFacts(context.Background(), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if got.Pool != "backup" || got.DataPool != "tank" || got.Files != nil {
		t.Fatalf("got %+v", got)
	}
}

// 备机的副本经 recv -u 收下时可能没挂载，挂载点读起来是空目录，不能显示成「没有文件」。
func TestPoolFactsDoesNotMistakeAnUnmountedFilesDatasetForEmpty(t *testing.T) {
	a, z, _ := factsAgent(t)
	z.poolFacts["tank"] = storage.PoolFacts{Pool: "tank", Datasets: []storage.PoolDatasetFact{{Name: "nd/files", Used: 13730394112}}}
	got, err := a.PoolFacts(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if got.Files == nil || got.Files.Readable {
		t.Fatalf("没挂载的文件库被当成了空：%+v", got.Files)
	}
}

func TestPoolFactsRefusesAPoolThisNodeDoesNotHave(t *testing.T) {
	a, _, _ := factsAgent(t)
	if _, err := a.PoolFacts(context.Background(), "gone"); err == nil {
		t.Fatal("不存在的池不能当成空池")
	}
}
