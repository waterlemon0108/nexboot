package local

import (
	"context"
	"testing"
)

// 重建副本里明确删除的对象才回收；分叉副本整份不动，未知的派生镜像及其依赖的来源保留。
func TestDeleteAsideImagePreservesUnlistedObjectsAndTheirBaselines(t *testing.T) {
	z := newFakePool()
	d, r := "tank/nd-diverged-1", "tank/nd-rebuilding-2"
	z.datasets = []string{
		"tank/nd/keep",
		d, d + "/db", d + "/files", d + "/win10", d + "/win10_default", d + "/win10_fork",
		r, r + "/db", r + "/win10", r + "/win10_default", r + "/saved",
		"tank/run/IDENT-000000000001",
	}
	z.snaps = map[string]bool{d + "/win10@0": true, d + "/win10_default@1": true, r + "/win10@0": true, r + "/win10_default@1": true}
	z.origin = map[string]string{
		d + "/win10_default":          d + "/win10@0",
		d + "/win10_fork":             d + "/win10@0", // 库里没记的派生配置，也是这个镜像的
		r + "/win10_default":          r + "/win10@0",
		r + "/saved":                  r + "/win10_default@1", // 由这个镜像的配置另存出的新镜像
		"tank/run/IDENT-000000000001": d + "/win10_default@1",
	}
	a := New("node-a", z, &fakeExporter{})

	_ = a.DeleteAsideImage(context.Background(), "win10", []string{"win10_default"})
	r = "tank/nd-diverged-2"
	for _, kept := range []string{d, d + "/win10", d + "/win10_default", d + "/win10_fork", "tank/run/IDENT-000000000001", r, r + "/win10", r + "/win10_default", r + "/saved"} {
		if !z.present(kept) {
			t.Fatalf("%s must remain: %v", kept, z.destroyed)
		}
	}
}
