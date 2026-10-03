package local

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

type recordingRunner struct {
	calls [][]string
	fail  string // 遇到该命令名时失败一次
	out   map[string]string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.fail != "" && name == r.fail {
		r.fail = ""
		return []byte("mkfs: device busy"), errors.New("exit status 1")
	}
	if r.out != nil {
		return []byte(r.out[name]), nil
	}
	return nil, nil
}

func blankAgent(t *testing.T) (*Agent, *fakePool, *recordingRunner) {
	t.Helper()
	pool := newFakePool()
	agent := New("server-a", pool)
	runner := &recordingRunner{}
	agent.runner = runner
	agent.resolveNodeFn = func(_ context.Context, dev string) (string, error) { return "/dev/zd16", nil }
	agent.waitForNodeFn = func(_ context.Context, path string) error { return nil }
	return agent, pool, runner
}

func TestCreateBlankImagePartitionsFormatsAndFinishesLikeAnImport(t *testing.T) {
	agent, pool, runner := blankAgent(t)
	const size = int64(100) << 30
	res, err := agent.CreateBlankImage(context.Background(), storage.BlankImageReq{Name: "games", SizeBytes: size, Filesystem: "ntfs"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID != "games" || res.Image.Size != size || res.Image.Purpose != domain.ImagePurposeData || res.Image.Origin != domain.ImageOriginBlank || res.Image.OSType != domain.OSTypeWindows {
		t.Fatalf("image = %#v", res.Image)
	}
	if res.Config.ID != "games_default" || res.Reduction.Name != "@0" || res.Reduction.ConfigID != "games_default" {
		t.Fatalf("config/reduction = %#v / %#v", res.Config, res.Reduction)
	}
	if len(pool.volumes) != 1 || pool.volumes[0].dataset != "tank/nd/games" || pool.volumes[0].sizeBytes != size {
		t.Fatalf("volumes = %#v", pool.volumes)
	}
	// 盘名即卷标，客户机资源管理器里看到的与控制台一致；mkfs 前必须 udevadm settle（原因见 CreateBlankImage）。
	want := [][]string{
		{"sgdisk", "-Z", "-n", "1:1MiB:0", "-t", "1:0700", "-c", "1:games", "/dev/zd16"},
		{"partprobe", "/dev/zd16"},
		{"udevadm", "settle", "--timeout=30"},
		{"mkfs.ntfs", "-Q", "-F", "-L", "games", "/dev/zd16p1"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
	// 收尾与导入相同：镜像 @0、克隆默认配置、配置 @0。
	joined := strings.Join(pool.ops, " | ")
	for _, op := range []string{"snapshot tank/nd/games@0", "clone tank/nd/games_default", "snapshot tank/nd/games_default@0"} {
		if !strings.Contains(joined, op) {
			t.Fatalf("ops = %v, missing %q", pool.ops, op)
		}
	}
	// 数据盘不烘焙启动脚本。
	if res.MountScriptInjected {
		t.Fatal("mount script injected into a data disk")
	}
}

func TestCreateBlankImageExt4IsALinuxDisk(t *testing.T) {
	agent, _, runner := blankAgent(t)
	res, err := agent.CreateBlankImage(context.Background(), storage.BlankImageReq{Name: "share", SizeBytes: 10 << 30, Filesystem: "ext4"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.OSType != domain.OSTypeLinux {
		t.Fatalf("os = %q", res.Image.OSType)
	}
	want := [][]string{
		{"sgdisk", "-Z", "-n", "1:1MiB:0", "-t", "1:8300", "-c", "1:share", "/dev/zd16"},
		{"partprobe", "/dev/zd16"},
		{"udevadm", "settle", "--timeout=30"},
		{"mkfs.ext4", "-F", "-q", "-L", "share", "/dev/zd16p1"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

// 填写的卷标优先于盘名，并同样裁剪。
func TestCreateBlankImageUsesTheTypedLabel(t *testing.T) {
	agent, _, runner := blankAgent(t)
	if _, err := agent.CreateBlankImage(context.Background(), storage.BlankImageReq{Name: "games", SizeBytes: 10 << 30, Filesystem: "ntfs", Label: " 游戏盘: "}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"sgdisk", "-Z", "-n", "1:1MiB:0", "-t", "1:0700", "-c", "1:游戏盘", "/dev/zd16"},
		{"partprobe", "/dev/zd16"},
		{"udevadm", "settle", "--timeout=30"},
		{"mkfs.ntfs", "-Q", "-F", "-L", "游戏盘", "/dev/zd16p1"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

// 卷标按文件系统上限裁剪并去掉非法字符；全被去掉时不设卷标，而不是给个坏卷标。
func TestVolumeLabelFitsTheFilesystem(t *testing.T) {
	cases := []struct{ name, fs, want string }{
		{"游戏盘", "ntfs", "游戏盘"},
		{"games:d?", "ntfs", "gamesd"},
		{"abcdefghij-abcdefghij-abcdefghij-xyz", "ntfs", "abcdefghij-abcdefghij-abcdefghij"},
		{"share-of-the-classroom-2026", "ext4", "share-of-the-cla"},
		{"资料共享盘二零二六", "ext4", "资料共享盘"}, // 16 字节只容 5 个汉字，不截半个字
		{"???", "ntfs", ""},
	}
	for _, c := range cases {
		if got := volumeLabel(c.name, c.fs); got != c.want {
			t.Errorf("volumeLabel(%q, %s) = %q, want %q", c.name, c.fs, got, c.want)
		}
	}
}

// 建卷后失败不能留下无人认领的半成品数据集。
func TestCreateBlankImageDestroysTheVolumeWhenFormattingFails(t *testing.T) {
	agent, pool, runner := blankAgent(t)
	runner.fail = "mkfs.ntfs"
	_, err := agent.CreateBlankImage(context.Background(), storage.BlankImageReq{Name: "games", SizeBytes: 10 << 30, Filesystem: "ntfs"})
	if err == nil {
		t.Fatal("format failure went unreported")
	}
	if !contains(pool.destroyed, "tank/nd/games") {
		t.Fatalf("volume left behind: destroyed = %v", pool.destroyed)
	}
}

func TestCreateBlankImageRejectsBadRequests(t *testing.T) {
	agent, pool, _ := blankAgent(t)
	for _, req := range []storage.BlankImageReq{
		{Name: "", SizeBytes: 10 << 30, Filesystem: "ntfs"},
		{Name: "x", SizeBytes: 0, Filesystem: "ntfs"},
		{Name: "x", SizeBytes: 10 << 30, Filesystem: "btrfs"},
	} {
		if _, err := agent.CreateBlankImage(context.Background(), req); err == nil {
			t.Fatalf("request %#v accepted", req)
		}
	}
	if len(pool.volumes) != 0 {
		t.Fatalf("volumes created despite refusals: %#v", pool.volumes)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestCreateBlankImageNamesItsTargetBeforeTouchingThePool(t *testing.T) {
	agent, pool, _ := blankAgent(t)
	var target string
	var volumesBefore = -1
	_, err := agent.CreateBlankImage(context.Background(), storage.BlankImageReq{
		Name: "games", SizeBytes: 1 << 30, Filesystem: "ntfs",
		OnTarget: func(id string) { target, volumesBefore = id, len(pool.volumes) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if target != "games" || volumesBefore != 0 {
		t.Fatalf("target = %q after %d volumes", target, volumesBefore)
	}
}
