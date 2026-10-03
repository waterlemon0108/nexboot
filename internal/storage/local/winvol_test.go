package local

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage/zfs"
)

// umountRunner 记录 umount 调用，并让普通 umount 失败指定次数，以观察重试和 lazy 兜底。
// ctxLive 记录每次调用拿到的 context 是否可用：调用方 context 已失效时 lazy detach 仍须可用。
type umountRunner struct {
	calls     [][]string
	ctxLive   []bool
	failPlain int
	failLazy  bool
}

func (r *umountRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	r.ctxLive = append(r.ctxLive, ctx.Err() == nil)
	if name != "umount" {
		return nil, nil
	}
	if len(args) > 0 && args[0] == "-l" {
		if r.failLazy {
			return []byte("target is busy"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if r.failPlain > 0 {
		r.failPlain--
		return []byte("target is busy"), errors.New("exit status 1")
	}
	return nil, nil
}

func TestUnmountRetriesThenDetachesLazily(t *testing.T) {
	// umount 持续失败时必须重试后 lazy detach，不能静默放弃让 zvol 永久被占。
	r := &umountRunner{failPlain: 3}
	dir := t.TempDir()

	unmount(context.Background(), r, dir)

	want := [][]string{
		{"umount", dir},
		{"umount", dir},
		{"umount", dir},
		{"umount", "-l", dir},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("mount dir not removed after lazy detach: %v", err)
	}
}

func TestUnmountStopsAtFirstSuccess(t *testing.T) {
	r := &umountRunner{}
	dir := t.TempDir()

	unmount(context.Background(), r, dir)

	if want := [][]string{{"umount", dir}}; !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
}

func TestUnmountKeepsDirWhenLazyDetachFails(t *testing.T) {
	// 什么都没释放就必须保留目录，删掉只会掩盖泄漏。
	r := &umountRunner{failPlain: 3, failLazy: true}
	dir := t.TempDir()

	unmount(context.Background(), r, dir)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("mount dir removed despite failed detach: %v", err)
	}
}

func TestDiscardFailedMountStillTriesUmount(t *testing.T) {
	// mount(8) 报错时内核可能已完成挂载，仍须尝试卸载。
	r := &umountRunner{}
	dir := t.TempDir()

	discardFailedMount(context.Background(), r, dir)

	if want := [][]string{{"umount", dir}}; !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir not removed: %v", err)
	}
}

func TestReleaseStaleMountsOnlyTouchesDoomedDatasets(t *testing.T) {
	r := &umountRunner{}
	agent := New("server-a", zfs.New("tank", &fakeRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = r
	agent.zvolMountsFn = func() []zfs.ZvolMount {
		return []zfs.ZvolMount{
			// 本客户机遗留的注入挂载，必须释放。
			{Device: "/dev/zd2800p3", Mountpoint: "/tmp/ndiskless-vol-1", Dataset: "tank/run/CLIENT-AABBCCDDEEFF"},
			// 另一台客户机正在注入，不能动。
			{Device: "/dev/zd64p3", Mountpoint: "/tmp/ndiskless-vol-2", Dataset: "tank/run/CLIENT-000000000001"},
		}
	}

	agent.releaseStaleMounts(context.Background(), []string{"tank/run/CLIENT-AABBCCDDEEFF"})

	want := [][]string{{"umount", "/tmp/ndiskless-vol-1"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
}

func TestReleaseStaleMountsNoopWithoutDatasets(t *testing.T) {
	r := &umountRunner{}
	agent := New("server-a", zfs.New("tank", &fakeRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = r
	agent.zvolMountsFn = func() []zfs.ZvolMount {
		t.Fatal("mounts scanned with nothing doomed")
		return nil
	}

	agent.releaseStaleMounts(context.Background(), nil)

	if len(r.calls) != 0 {
		t.Fatalf("calls = %#v", r.calls)
	}
}

func TestUnmountDetachesEvenWhenCallerContextIsDone(t *testing.T) {
	// 调用方 context 已失效时，lazy detach 不能继承它，否则无法防止永久卡死。
	r := &umountRunner{failPlain: 3}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	unmount(ctx, r, dir)

	// 只尝试一次普通 umount，随即 lazy detach。
	want := [][]string{{"umount", dir}, {"umount", "-l", dir}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
	if r.ctxLive[0] {
		t.Fatal("first attempt should have used the caller's dead context")
	}
	if !r.ctxLive[1] {
		t.Fatal("lazy detach ran on the caller's dead context; the mount would leak")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("mount dir not removed after detach: %v", err)
	}
}

// partitionRunner 从第 readyAt 次 lsblk 起才返回 NTFS 分区，用于观察轮询的调用模式。
type partitionRunner struct {
	calls     [][]string
	lsblkHits int
	readyAt   int
}

func (r *partitionRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name != "lsblk" {
		return nil, nil
	}
	r.lsblkHits++
	if r.lsblkHits >= r.readyAt {
		return []byte(`{"blockdevices":[{"path":"/dev/zd0","fstype":"","children":[{"path":"/dev/zd0p3","fstype":"ntfs"}]}]}`), nil
	}
	return []byte(`{"blockdevices":[{"path":"/dev/zd0","fstype":""}]}`), nil
}

func (r *partitionRunner) count(name string) int {
	n := 0
	for _, c := range r.calls {
		if c[0] == name {
			n++
		}
	}
	return n
}

func TestWaitNTFSPartitionsReadyCostsOneCall(t *testing.T) {
	// 分区已就绪时只调一次 lsblk，不 partprobe 也不 settle。
	r := &partitionRunner{readyAt: 1}

	parts := waitNTFSPartitions(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X")

	if len(parts) != 1 || parts[0].FSType != "ntfs" {
		t.Fatalf("parts = %#v", parts)
	}
	if want := [][]string{{"lsblk", "-J", "-o", "PATH,FSTYPE", "/dev/zvol/tank/run/CLIENT-X"}}; !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %#v, want %#v", r.calls, want)
	}
}

func TestWaitNTFSPartitionsProbesOnceAndNeverSettles(t *testing.T) {
	// 开机风暴下轮询要便宜：最多一次 partprobe，绝不 `udevadm settle`（全局屏障）。
	r := &partitionRunner{readyAt: 4}

	parts := waitNTFSPartitions(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X")

	if len(parts) != 1 {
		t.Fatalf("parts = %#v", parts)
	}
	if got := r.count("partprobe"); got != 1 {
		t.Fatalf("partprobe called %d times, want exactly 1", got)
	}
	if got := r.count("udevadm"); got != 0 {
		t.Fatalf("udevadm called %d times, want 0 (global barrier)", got)
	}
}

func TestWaitNTFSPartitionsGivesUpOnCancel(t *testing.T) {
	// 开机被取消后不能继续轮询到超时。
	r := &partitionRunner{readyAt: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if parts := waitNTFSPartitions(ctx, r, "/dev/zvol/tank/run/CLIENT-X"); parts != nil {
		t.Fatalf("parts = %#v, want nil", parts)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("kept polling for %v after cancellation", d)
	}
}

// sessionRunner 模拟卷会话用到的命令：lsblk 列分区，mount 为指定分区生成 \Windows\System32，
// umount 再把它清掉。
type sessionRunner struct {
	calls     [][]string
	lsblk     string
	winParts  map[string]bool // 含 \Windows\System32 的分区
	failMount map[string]bool // 每次挂载都失败的分区
}

func (r *sessionRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "lsblk":
		return []byte(r.lsblk), nil
	case "mount", "ntfs-3g":
		part, dir := args[len(args)-2], args[len(args)-1]
		if r.failMount[part] {
			return []byte("mount failed"), errors.New("exit status 32")
		}
		if r.winParts[part] {
			if err := os.MkdirAll(filepath.Join(dir, "Windows", "System32"), 0o755); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "umount":
		// 卸载后挂载点变回空目录。
		dir := args[len(args)-1]
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil, nil
	}
	return nil, nil
}

func (r *sessionRunner) count(name string) int {
	n := 0
	for _, c := range r.calls {
		if c[0] == name {
			n++
		}
	}
	return n
}

const twoNTFSLsblk = `{"blockdevices":[{"path":"/dev/zd0","fstype":"","children":[
	{"path":"/dev/zd0p4","fstype":"ntfs"},
	{"path":"/dev/zd0p3","fstype":"ntfs"}]}]}`

func TestWithWindowsPartitionSelectsSystem32AndReleases(t *testing.T) {
	// 排在前面、没有 \Windows 的恢复分区要跳过并释放；fn 只在真正的 Windows 分区上执行；全部卸载。
	r := &sessionRunner{lsblk: twoNTFSLsblk, winParts: map[string]bool{"/dev/zd0p3": true}}

	ran := 0
	err := withWindowsPartition(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X", volRW, clonePartitionWait, func(mnt string) error {
		ran++
		if _, err := os.Stat(filepath.Join(mnt, "Windows", "System32")); err != nil {
			t.Fatalf("fn saw no System32 at %s: %v", mnt, err)
		}
		return nil
	})
	if err != nil || ran != 1 {
		t.Fatalf("err = %v, ran = %d", err, ran)
	}
	// 每个候选挂一次、每个挂载释放一次，且以 rw 挂载。
	if got := r.count("mount"); got != 2 {
		t.Fatalf("mount called %d times, want 2", got)
	}
	if got := r.count("umount"); got != 2 {
		t.Fatalf("umount called %d times, want 2", got)
	}
	for _, c := range r.calls {
		if c[0] == "mount" && c[4] != "rw,force" {
			t.Fatalf("rw session mounted with %q", c[4])
		}
		if c[0] == "udevadm" {
			t.Fatal("udevadm must never run in a session")
		}
	}
}

func TestWithWindowsPartitionReleasesWhenFnFails(t *testing.T) {
	r := &sessionRunner{lsblk: twoNTFSLsblk, winParts: map[string]bool{"/dev/zd0p3": true}}

	boom := errors.New("payload failed")
	err := withWindowsPartition(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X", volRW, clonePartitionWait, func(string) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want payload error", err)
	}
	if got := r.count("umount"); got != 2 {
		t.Fatalf("umount called %d times after fn error, want 2", got)
	}
}

func TestWithWindowsPartitionReleasesOnPanic(t *testing.T) {
	// fn panic 时也要释放挂载，然后继续向上抛出 panic。
	r := &sessionRunner{lsblk: twoNTFSLsblk, winParts: map[string]bool{"/dev/zd0p3": true}}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = withWindowsPartition(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X", volRW, clonePartitionWait, func(string) error {
			panic("payload exploded")
		})
	}()
	if recovered == nil {
		t.Fatal("panic did not propagate")
	}
	if got := r.count("umount"); got != 2 {
		t.Fatalf("umount called %d times after panic, want 2", got)
	}
}

func TestWithWindowsPartitionNotFoundSentinel(t *testing.T) {
	// 只有 vfat ESP，轮询不会就绪；context 已取消则立即放弃并返回哨兵错误。
	r := &sessionRunner{lsblk: `{"blockdevices":[{"path":"/dev/zd0","fstype":"","children":[{"path":"/dev/zd0p1","fstype":"vfat"}]}]}`}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := withWindowsPartition(ctx, r, "/dev/zvol/tank/run/CLIENT-X", volRW, clonePartitionWait, func(string) error {
		t.Fatal("fn must not run")
		return nil
	})
	if !errors.Is(err, errNoWindowsPartition) {
		t.Fatalf("err = %v, want errNoWindowsPartition", err)
	}
	if !strings.Contains(err.Error(), "no Windows partition found") {
		t.Fatalf("error text changed: %v", err)
	}
}

func TestWithEachPartitionSurveysAndReleasesAll(t *testing.T) {
	// vfat 挂不上（跳过）、ntfs 挂上、swap 不支持：fn 只拿到可挂载的集合，所有挂载都释放。
	r := &sessionRunner{
		lsblk: `{"blockdevices":[{"path":"/dev/zd0","fstype":"","children":[
			{"path":"/dev/zd0p1","fstype":"vfat"},
			{"path":"/dev/zd0p3","fstype":"ntfs"},
			{"path":"/dev/zd0p5","fstype":"swap"}]}]}`,
		failMount: map[string]bool{"/dev/zd0p1": true},
	}

	var seen []string
	err := withEachPartition(context.Background(), r, "/dev/zvol/tank/run/CLIENT-X", func(mounts []mountedFS) error {
		for _, m := range mounts {
			seen = append(seen, m.FSType)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"ntfs"}) {
		t.Fatalf("surveyed fstypes = %v", seen)
	}
	if got := r.count("umount"); got != 1 {
		t.Fatalf("umount called %d times, want 1", got)
	}
	if got := r.count("udevadm"); got != 0 {
		t.Fatalf("udevadm called %d times, want 0", got)
	}
}

// 「分区没出现」「挂不上」「不是 Windows 卷」三种失败要在报错里区分开，排查方向各不相同；
// 同时仍保持 errNoWindowsPartition 哨兵。
func TestWithWindowsPartitionSaysWhichStepFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *sessionRunner
		want   string
	}{
		{
			name:   "分区始终没出现",
			runner: &sessionRunner{lsblk: `{"blockdevices":[{"path":"/dev/zd0","fstype":""}]}`},
			want:   "no NTFS partition appeared",
		},
		{
			name: "分区在但挂不上",
			runner: &sessionRunner{lsblk: twoNTFSLsblk,
				failMount: map[string]bool{"/dev/zd0p3": true, "/dev/zd0p4": true}},
			want: "none could be mounted",
		},
		{
			name:   "挂上了但不是 Windows 卷",
			runner: &sessionRunner{lsblk: twoNTFSLsblk},
			want:   "none carries",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := withWindowsPartition(context.Background(), tc.runner, "/dev/zvol/tank/nd/x", volRW, 50*time.Millisecond, func(string) error {
				t.Fatal("fn must not run")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if !errors.Is(err, errNoWindowsPartition) {
				t.Fatalf("err = %v, want it to stay the sentinel callers match on", err)
			}
		})
	}
}

// 等待时长由调用方指定并被遵守；导入的预算必须大于开机的，导入放弃就烘焙不了。
func TestWaitPartitionsHonorsTheBudgetItIsGiven(t *testing.T) {
	r := &partitionRunner{readyAt: 1 << 30} // 永不就绪
	start := time.Now()
	if parts := waitPartitions(context.Background(), r, "/dev/zvol/tank/nd/x", hasNTFS, 200*time.Millisecond); parts != nil {
		t.Fatalf("parts = %#v, want nil on timeout", parts)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("waited %s; the budget was ignored", elapsed)
	}
	if importPartitionWait <= clonePartitionWait {
		t.Fatalf("import budget %s must exceed the boot budget %s", importPartitionWait, clonePartitionWait)
	}
}
