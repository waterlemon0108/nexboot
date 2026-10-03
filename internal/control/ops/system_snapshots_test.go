package ops

import (
	"context"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// 复制和备份标记是记账快照，恢复时不能当成还原点收养，否则每轮复制都会多出一个还原点。
func TestRecoverCatalogueSkipsSystemSnapshots(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakeInventoryStorage{
		inv: storage.PoolInventory{
			Datasets: []string{"win11", "win11_default", "db", "files"},
			Snapshots: []string{
				"win11@0", "win11@rep-1755600000", "win11@ndbackup-x1",
				"win11_default@0", "win11_default@office",
				"win11_default@rep-1755600000", "win11_default@ndbackup-x1",
			},
			Origins: map[string]string{"win11_default": "win11@0"},
		},
		osType: domain.OSTypeWindows,
	}
	svc := ReconcileService{Store: st, Storage: agent}

	report, err := svc.RecoverCatalogue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// db 和 files 是目录容器自己的子数据集，不是镜像。
	if report.Images != 1 {
		t.Fatalf("images = %d, want 1 (db/files must not be adopted)", report.Images)
	}
	reds, err := st.Reductions().ListByConfig(ctx, "win11_default")
	if err != nil {
		t.Fatal(err)
	}
	for _, red := range reds {
		if red.Name == "@rep-1755600000" || red.Name == "@ndbackup-x1" {
			t.Fatalf("system snapshot adopted as restore point: %#v", red)
		}
	}
	if len(reds) != 2 { // @0 与 @office
		t.Fatalf("reductions = %#v", reds)
	}
}

// 一致性检查不能把记账快照和容器的保留子数据集报成漂移。
func TestCheckIgnoresSystemSnapshotsAndReservedChildren(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets: []string{"img", "img_default", "imports", "db", "files"},
		Snapshots: []string{
			"img@0", "img@rep-2", "img_default@0", "img_default@r1",
			"img_default@rep-2", "img_default@ndbackup-7",
		},
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || len(report.Issues) != 0 {
		t.Fatalf("report = %#v", report.Issues)
	}
}
