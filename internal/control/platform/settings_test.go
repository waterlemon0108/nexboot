package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/store"
)

func TestSystemSettingsServiceUsesFallbackUntilSaved(t *testing.T) {
	ctx := context.Background()
	st := newSettingsTestStore(t)
	service := SystemSettingsService{Store: st, FallbackImportDir: "/boot-default/imports"}

	got, err := service.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != "/boot-default/imports" || got.DefaultImportDir != "/boot-default/imports" {
		t.Fatalf("fallback settings = %#v", got)
	}
}

func TestSystemSettingsServiceSaveValidatesAndPersistsImportDir(t *testing.T) {
	ctx := context.Background()
	st := newSettingsTestStore(t)
	root := t.TempDir()
	target := filepath.Join(root, "imports")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	service := SystemSettingsService{Store: st, FallbackImportDir: "/boot-default/imports"}

	got, err := service.Save(ctx, SystemSettingsRequest{ImportDir: target})
	if err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != target {
		t.Fatalf("saved settings = %#v", got)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("target dir not ready: info=%#v err=%v", info, err)
	}

	got, err = service.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != target {
		t.Fatalf("persisted settings = %#v", got)
	}
}

func TestSystemSettingsServiceRejectsUnsafeImportDir(t *testing.T) {
	service := SystemSettingsService{Store: newSettingsTestStore(t), FallbackImportDir: "/boot-default/imports"}
	for _, dir := range []string{"", "relative/imports", string(filepath.Separator), "/proc/ndiskless"} {
		if _, err := service.Save(context.Background(), SystemSettingsRequest{ImportDir: dir}); !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("Save(%q) err=%v, want invalid", dir, err)
		}
	}
	missingParent := filepath.Join(t.TempDir(), "missing", "imports")
	if _, err := service.Save(context.Background(), SystemSettingsRequest{ImportDir: missingParent}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("missing parent err=%v, want invalid", err)
	}
}

func newSettingsTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 复制限速：读取时按部署默认值解析三种状态；保存不能覆盖同一行里的其它设置。
func TestReplicationRateReadAndSavePreservesRow(t *testing.T) {
	ctx := context.Background()
	st := newSettingsTestStore(t)
	svc := SystemSettingsService{Store: st, FallbackImportDir: "/fb"}

	// 从未设置：取默认值
	mbps, source, err := svc.ReplicationRate(ctx, 100)
	if err != nil || mbps != 100 || source != "default" {
		t.Fatalf("mbps=%d source=%q err=%v", mbps, source, err)
	}

	// 先写入其它设置，再把限速设为 0（不限）
	dir := t.TempDir()
	if _, err := svc.Save(ctx, SystemSettingsRequest{ImportDir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveReplicationRate(ctx, 0); err != nil {
		t.Fatal(err)
	}
	mbps, source, _ = svc.ReplicationRate(ctx, 100)
	if mbps != 0 || source != "setting" {
		t.Fatalf("explicit unlimited: mbps=%d source=%q", mbps, source)
	}
	view, err := svc.Get(ctx)
	if err != nil || view.ImportDir != dir {
		t.Fatalf("import dir clobbered: %#v err=%v", view, err)
	}

	if err := svc.SaveReplicationRate(ctx, -5); err == nil {
		t.Fatal("negative rate must be refused")
	}
}
