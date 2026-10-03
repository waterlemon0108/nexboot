package assets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListImportSourcesFiltersAndLabels(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"win11.vmdk", "base.zfs", "disk.qcow2", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := ImageService{ImportDir: dir}.ListImportSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ImportDir != dir || res.Total != 3 {
		t.Fatalf("result = %#v", res)
	}
	formats := map[string]string{}
	for _, it := range res.Items {
		formats[it.Name] = it.Format
	}
	if formats["win11.vmdk"] != "vmdk" || formats["base.zfs"] != "zfs-send" || formats["disk.qcow2"] != "qcow2" {
		t.Fatalf("formats = %#v", formats)
	}
	if _, ok := formats["notes.txt"]; ok {
		t.Fatal("notes.txt should be filtered out")
	}
}

func TestListImportSourcesMissingDirReturnsError(t *testing.T) {
	res, err := ImageService{ImportDir: filepath.Join(t.TempDir(), "nope")}.ListImportSources(context.Background())
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, result = %#v", err, res)
	}
}

func TestListImportSourcesUsesImportDirResolver(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "win11.img"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := ImageService{
		ImportDir: "/ignored/imports",
		ImportDirResolver: func(context.Context) (string, error) {
			return dir, nil
		},
	}

	res, err := service.ListImportSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ImportDir != dir || res.Total != 1 || res.Items[0].Name != "win11.img" {
		t.Fatalf("result = %#v", res)
	}
}
