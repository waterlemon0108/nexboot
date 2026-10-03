package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

type inventoryOnly struct {
	storage.StorageAgent
	datasets []string
}

func (a inventoryOnly) Inventory(context.Context) (storage.PoolInventory, error) {
	return storage.PoolInventory{Datasets: a.datasets}, nil
}

// 接任前读的是收到的库副本本身：副本里有配置「办公」而池里没有，就要报出来；副本文件不动（检查在临时文件上做）。
func TestDBCopyMissingReadsTheReplicatedCopy(t *testing.T) {
	ctx := context.Background()
	copyPath := filepath.Join(t.TempDir(), "ndiskless.db")
	st, err := store.Open(ctx, "file:"+copyPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "img", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "img_office", ImageID: "img", Name: "办公", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got := dbCopyMissing(ctx, copyPath, inventoryOnly{datasets: []string{"img"}}, logger)
	if !reflect.DeepEqual(got, []string{"配置「办公」"}) {
		t.Fatalf("missing = %v", got)
	}
	if got := dbCopyMissing(ctx, copyPath, inventoryOnly{datasets: []string{"img", "img_office"}}, logger); len(got) != 0 {
		t.Fatalf("完整时不应报缺: %v", got)
	}
	if got := dbCopyMissing(ctx, filepath.Join(t.TempDir(), "none.db"), inventoryOnly{}, logger); got != nil {
		t.Fatalf("没有副本时交给 PrepareActiveDB 拒绝，这里不报: %v", got)
	}
}
