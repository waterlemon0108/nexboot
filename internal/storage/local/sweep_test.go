package local

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage/zfs"
)

// sweepRunner 伪造 umount 与 dmsetup，并记下每次 dmsetup remove 时池里是否已有克隆被删，用来断言顺序。
type sweepRunner struct {
	pool             *fakePool
	dmList           string
	unmounted        []string
	removed          []string
	removedAfterDrop bool
	// unmountedBeforeDm 是第一次 dmsetup remove 时已卸掉的挂载数。
	unmountedBeforeDm int
}

func (r *sweepRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch {
	case name == "umount":
		r.unmounted = append(r.unmounted, args[len(args)-1])
	case name == "dmsetup" && args[0] == "ls":
		return []byte(r.dmList), nil
	case name == "dmsetup" && args[0] == "remove":
		if len(r.removed) == 0 {
			r.unmountedBeforeDm = len(r.unmounted)
		}
		r.removed = append(r.removed, args[len(args)-1])
		if len(r.pool.destroyed) > 0 {
			r.removedAfterDrop = true
		}
	}
	return nil, nil
}

// 进程崩溃会留下体检、导出、适配回读的临时克隆，以及挂在逻辑卷映射上的只读挂载：
// 它们占住 zvol、挡住合并，run/ 下的克隆又没人再认领。启动时按「卸挂载 → 删映射 → 销毁克隆」清掉，
// 正在用的客户机盘、超管盘和别人的映射一概不碰。
func TestSweepScratchClearsCrashLeftovers(t *testing.T) {
	p := poolWith(
		"tank/nd/img",
		"tank/run/INSPECT-IMG-1",
		"tank/run/EXPORT-CFG-2",
		"tank/run/NDADAPTCHK-AABBCCDDEEFF",
		"tank/run/SCLIENT-AABBCCDDEEFF",
		"tank/run/CLIENT-112233445566",
	)
	p.snaps["tank/nd/img@0"] = true
	p.snaps["tank/run/SCLIENT-AABBCCDDEEFF@ndadaptcheck"] = true
	p.snaps["tank/run/EXPORT-CFG-2@ndexport-9"] = true
	p.origin["tank/run/NDADAPTCHK-AABBCCDDEEFF"] = "tank/run/SCLIENT-AABBCCDDEEFF@ndadaptcheck"
	r := &sweepRunner{pool: p, dmList: "nd-0a1b2c3d-root\t(253:3)\nubuntu--vg-root\t(253:0)\nnd-notours\t(253:9)\n"}
	agent := New("server-a", p)
	agent.runner = r
	agent.procMountsFn = func() []byte {
		return []byte("/dev/mapper/nd-0a1b2c3d-root /tmp/ndiskless-vol-1 ext4 ro 0 0\n" +
			"/dev/mapper/ubuntu--vg-root / ext4 rw 0 0\n" +
			"/dev/zd16p2 /tmp/ndiskless-vol-2 ntfs3 ro 0 0\n" +
			"/dev/zd32p2 /tmp/ndiskless-vol-3 ntfs3 rw 0 0\n")
	}
	agent.zvolMountsFn = func() []zfs.ZvolMount {
		return []zfs.ZvolMount{
			{Device: "/dev/zd16p2", Mountpoint: "/tmp/ndiskless-vol-2", Dataset: "tank/run/INSPECT-IMG-1"},
			{Device: "/dev/zd32p2", Mountpoint: "/tmp/ndiskless-vol-3", Dataset: "tank/run/SCLIENT-AABBCCDDEEFF"},
		}
	}

	got, err := agent.SweepScratch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.removed, []string{"nd-0a1b2c3d-root"}) || r.removedAfterDrop {
		t.Fatalf("dm removed=%v (after a clone destroy: %v), want only ours and before any destroy", r.removed, r.removedAfterDrop)
	}
	if slices.Index(r.unmounted, "/tmp/ndiskless-vol-1") != 0 || r.unmountedBeforeDm < 1 {
		t.Fatalf("the mount on the mapping must go before the mapping: unmounted=%v", r.unmounted)
	}
	if !slices.Contains(r.unmounted, "/tmp/ndiskless-vol-2") || slices.Contains(r.unmounted, "/tmp/ndiskless-vol-3") || slices.Contains(r.unmounted, "/") {
		t.Fatalf("unmounted = %v, want only the scratch mounts", r.unmounted)
	}
	for _, gone := range []string{"tank/run/INSPECT-IMG-1", "tank/run/EXPORT-CFG-2", "tank/run/NDADAPTCHK-AABBCCDDEEFF"} {
		if p.present(gone) {
			t.Fatalf("%s left behind: %v", gone, p.state())
		}
	}
	for _, kept := range []string{"tank/nd/img", "tank/run/SCLIENT-AABBCCDDEEFF", "tank/run/CLIENT-112233445566"} {
		if !p.present(kept) {
			t.Fatalf("%s must not be touched: %v", kept, p.state())
		}
	}
	if p.snaps["tank/run/SCLIENT-AABBCCDDEEFF@ndadaptcheck"] || !p.snaps["tank/nd/img@0"] {
		t.Fatalf("snapshots = %v", p.state())
	}
	if len(got.Clones) != 3 || len(got.Mappings) != 1 || len(got.Unmounted) != 2 || len(got.Snapshots) != 1 {
		t.Fatalf("report = %#v", got)
	}
	if !strings.Contains(strings.Join(got.Snapshots, ","), "@ndadaptcheck") {
		t.Fatalf("report = %#v", got)
	}
}

// 干净的节点上什么都不做，也不报错。
func TestSweepScratchOnACleanNodeDoesNothing(t *testing.T) {
	p := poolWith("tank/nd/img", "tank/run/SCLIENT-AABBCCDDEEFF")
	r := &sweepRunner{pool: p, dmList: "No devices found\n"}
	agent := New("server-a", p)
	agent.runner = r
	agent.procMountsFn = func() []byte { return []byte("/dev/sda1 / ext4 rw 0 0\n") }
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	got, err := agent.SweepScratch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() || len(p.ops) != 0 || len(r.removed) != 0 || len(r.unmounted) != 0 {
		t.Fatalf("report=%#v ops=%v dm=%v umount=%v", got, p.ops, r.removed, r.unmounted)
	}
}
