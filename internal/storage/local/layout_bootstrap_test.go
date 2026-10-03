package local

import (
	"context"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// 经产品建的数据池立即是 v2 布局，导入前容器已存在；备份池不存目录，不建容器。
func TestCreatePoolInitializesLayoutOnDataPoolOnly(t *testing.T) {
	p := newFakePool()
	agent := New("server-a", p)

	if _, err := agent.CreatePool(context.Background(), "tank", storage.PoolSpec{Disks: []string{"/dev/sda"}}); err != nil {
		t.Fatal(err)
	}
	if p.layoutEnsured != 1 {
		t.Fatalf("data pool: layoutEnsured = %d, want 1", p.layoutEnsured)
	}

	if _, err := agent.CreatePool(context.Background(), "backup", storage.PoolSpec{Disks: []string{"/dev/sdb"}}); err != nil {
		t.Fatal(err)
	}
	if p.layoutEnsured != 1 {
		t.Fatalf("backup pool must not get containers: layoutEnsured = %d, want still 1", p.layoutEnsured)
	}
}
