package local

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/qemu"
)

// 外部磁盘镜像（vmdk/vhdx/qcow2）由 qemu-img 直接转换进新建的 zvol。转换本身归 qemu 包测；
// 这里测编排：探测大小、按该大小建卷、解析真实设备节点、转换写入。

type fakeQemuRunner struct {
	info        string
	infoErr     error
	progress    string
	convertErr  error
	convertArgs []string
}

func (f *fakeQemuRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte(f.info), f.infoErr
}

func (f *fakeQemuRunner) RunWithProgress(_ context.Context, stdout io.Writer, name string, args ...string) error {
	f.convertArgs = append([]string{name}, args...)
	if f.progress != "" {
		_, _ = io.WriteString(stdout, f.progress)
	}
	return f.convertErr
}

// diskImageAgent 构造一个 qemu-img 与设备节点解析为假件、导入路径本身为真的 Agent。
func diskImageAgent(t *testing.T, runner *fakeQemuRunner) (*Agent, *fakePool) {
	t.Helper()
	pool := newFakePool()
	agent := New("server-a", pool)
	agent.qemu = qemu.New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent.resolveNodeFn = func(_ context.Context, dev string) (string, error) { return "/dev/zd16", nil }
	agent.injectMountScriptFn = func(context.Context, string, []byte) error { return nil }
	return agent, pool
}

func diskImageFixture(t *testing.T, name string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("not really a disk image"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestImportDiskImageSizesTheVolumeFromWhatQemuReports(t *testing.T) {
	const virtualSize = 34359738368 // 32 GiB
	runner := &fakeQemuRunner{info: `{"format":"vmdk","virtual-size":34359738368}`, progress: "(50.00/100%)\n"}
	agent, pool := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.vmdk")

	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 卷大小必须取虚拟大小而非文件大小：6 GB 的压缩 vmdk 里装的是 32 GiB 的盘。
	if len(pool.volumes) != 1 {
		t.Fatalf("volumes = %#v", pool.volumes)
	}
	vol := pool.volumes[0]
	if vol.dataset != "tank/nd/win11" || vol.sizeBytes != virtualSize {
		t.Fatalf("volume = %#v", vol)
	}
	if vol.volblock != defaultVolblockBytes {
		t.Fatalf("volblock = %d, want %d", vol.volblock, defaultVolblockBytes)
	}
	if result.Image.Size != virtualSize {
		t.Fatalf("recorded size = %d, want %d", result.Image.Size, virtualSize)
	}
}

// qemu-img 写真实的 /dev/zdN 而非 /dev/zvol 软链：开机风暴下 udev 滞后，
// 过时软链可能指向已属于别台机器的盘。
func TestImportDiskImageConvertsIntoTheResolvedDeviceNode(t *testing.T) {
	runner := &fakeQemuRunner{info: `{"format":"vmdk","virtual-size":1073741824}`}
	agent, _ := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.vmdk")

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.convertArgs, " ")
	if !strings.Contains(args, "/dev/zd16") {
		t.Fatalf("convert args = %q, want the resolved node", args)
	}
	if strings.Contains(args, "/dev/zvol/") {
		t.Fatalf("convert wrote through the udev symlink: %q", args)
	}
	// 把探测到的格式显式传给 qemu-img，不让它再猜；带 vmdk 尾部的 raw 文件就是猜错的典型。
	if !strings.Contains(args, "-f vmdk") || !strings.Contains(args, "-O raw") {
		t.Fatalf("convert args = %q", args)
	}
}

func TestImportDiskImageReportsConversionProgress(t *testing.T) {
	runner := &fakeQemuRunner{info: `{"format":"qcow2","virtual-size":1073741824}`, progress: "(10.00/100%)\n(60.00/100%)\n"}
	agent, _ := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.qcow2")

	var messages []string
	_, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
		OnProgress: func(percent int, message string) { messages = append(messages, message) },
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(messages, "|")
	for _, want := range []string{"探测镜像", "创建卷", "转换写入 10%", "转换写入 60%"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("progress = %q, want it to mention %q", joined, want)
		}
	}
}

// 转换中途失败会留下半个镜像的 zvol，不能让它作为看似可用的数据集留下。
func TestImportDiskImageCleansUpAfterAFailedConversion(t *testing.T) {
	runner := &fakeQemuRunner{info: `{"format":"vmdk","virtual-size":1073741824}`, convertErr: errors.New("qemu-img: write error")}
	agent, pool := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.vmdk")

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	}); err == nil {
		t.Fatal("expected the conversion failure to surface")
	}
	for _, d := range pool.datasets {
		if d == "tank/nd/win11" {
			t.Fatalf("the half-written volume survived: %#v", pool.datasets)
		}
	}
}

// 新服务器首次运行常缺 qemu-img，报错要告诉操作者装什么，而不是报 exec 错误。
func TestImportDiskImageSaysWhatToInstallWhenQemuIsMissing(t *testing.T) {
	runner := &fakeQemuRunner{infoErr: errors.New("exec: \"qemu-img\": executable file not found in $PATH")}
	agent, _ := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.vmdk")

	_, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if qemu.Available() {
		t.Skip("qemu-img is installed here, so the missing-binary branch cannot be reached")
	}
	if !strings.Contains(err.Error(), "qemu-utils") {
		t.Fatalf("err = %v, want it to name the package to install", err)
	}
}

// ZFS 流只走 receive，不走 qemu-img，两个入口不能串。
func TestImportOfAZFSStreamNeverCallsQemu(t *testing.T) {
	runner := &fakeQemuRunner{info: `{"format":"raw","virtual-size":1}`}
	agent, _ := diskImageAgent(t, runner)
	root, source := diskImageFixture(t, "win11.zfs")

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	}); err != nil {
		t.Fatal(err)
	}
	if runner.convertArgs != nil {
		t.Fatalf("a zfs stream was sent through qemu-img: %#v", runner.convertArgs)
	}
}

// 两次同名导入并发时，第二次建卷撞上第一次刚建的卷：失败的回滚只能删自己建的，
// 不能把对方正在写的卷删掉。
func TestImportDiskImageLeavesAConcurrentImportsVolumeAlone(t *testing.T) {
	agent, pool := diskImageAgent(t, &fakeQemuRunner{info: `{"format":"vmdk","virtual-size":1073741824}`})
	root, source := diskImageFixture(t, "win11.vmdk")
	pool.beforeOp = func(op string) {
		if op == "create tank/nd/win11" {
			pool.datasets = append(pool.datasets, "tank/nd/win11") // 另一次导入抢先建好
		}
	}

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: source, ImportDir: root, OSType: domain.OSTypeWindows,
	}); err == nil {
		t.Fatal("want the create to fail")
	}
	for _, d := range pool.destroyAttempts {
		if d == "tank/nd/win11" {
			t.Fatalf("destroyed the other import's volume: %v", pool.destroyAttempts)
		}
	}
}
