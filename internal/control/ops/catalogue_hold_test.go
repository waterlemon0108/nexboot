package ops

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// 打标记要等进行中的目录变更做完：超管保存先挪走配置再改名进来，中间拍下的
// 标记里缺这个配置，备机据此接任就丢了配置。
func TestMarkWaitsForACatalogueChangeInProgress(t *testing.T) {
	ctx := context.Background()
	z := &fakeReplZFS{dbCopyDir: t.TempDir(), guids: map[string]zfs.GUIDInventory{
		"tank/nd": {Entries: []zfs.GUIDEntry{{Name: "tank/nd/win11", GUID: "1"}}},
	}}
	rep := &Replicator{
		ZFS: z, Root: "tank/nd", Gate: ha.NewOpenGate(), Now: replTestClock(),
		DBSnapshot: func(context.Context, string) error { return nil },
	}
	done := storage.ChangeCatalogue()
	marked := make(chan error, 1)
	go func() {
		_, err := rep.MarkOnce(ctx)
		marked <- err
	}()
	select {
	case <-marked:
		t.Fatal("目录变更还没做完就打了标记")
	case <-time.After(100 * time.Millisecond):
	}
	done()
	select {
	case err := <-marked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("变更结束后标记没有继续")
	}
	if len(z.snapshots) != 1 {
		t.Fatalf("snapshots = %v", z.snapshots)
	}
}
