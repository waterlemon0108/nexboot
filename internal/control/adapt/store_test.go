package adapt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// newImageTestStore 打开一个临时 sqlite store，各 control 子包测试各自保留一份。
func newImageTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedHealthImage 预置适配测试开机所需的 镜像→配置→还原点 链，是 assets 测试夹具的私有副本。
func seedHealthImage(t *testing.T, ctx context.Context, st *store.SQLStore, now time.Time) {
	t.Helper()
	redID := "win11_0"
	if err := st.Images().Create(ctx, domain.Image{ID: "win11", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "win11_default", ImageID: "win11", Name: "default", DefaultReductionID: &redID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: redID, ConfigID: "win11_default", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
}
