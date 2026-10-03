package local

// 离线卷会话：在服务端挂载克隆的分区、执行离线操作并保证挂载被释放。
// 脚本注入、适配结果回读、镜像检查都只经 withWindowsPartition 和 withEachPartition 两个入口。
// 不变量：挂载不得活过会话。泄漏的挂载会占住整个 zvol，之后 `zfs destroy` 一律
// "dataset is busy"，客户机卡死直到人工卸载；所以释放用 defer、带重试和 lazy detach，
// 崩溃遗留的挂载由 releaseStaleMounts 在销毁克隆前兜底。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/storage/zfs"
)

type mountMode int

const (
	volRO mountMode = iota
	volRW
)

// errNoWindowsPartition 表示设备上没有带 \Windows\System32 的 NTFS 分区。
// 注入路径视为失败；适配结果轮询视为「还没有可读的」。
var errNoWindowsPartition = errors.New("no Windows partition found")

// withWindowsPartition 挂载带 \Windows\System32 的那个 NTFS 分区（跳过恢复分区、
// 系统保留分区）并对其执行 fn；无论 fn 出错还是 panic，挂载都会释放。
func withWindowsPartition(ctx context.Context, r runner, dev string, mode mountMode, wait time.Duration, fn func(mnt string) error) error {
	parts := waitPartitions(ctx, r, dev, hasNTFS, wait)
	if len(parts) == 0 {
		// 常见原因是刚收下的镜像分区节点还没出现，报错要能和「镜像里没有 Windows」区分开。
		return fmt.Errorf("%w: no NTFS partition appeared on %s within %s", errNoWindowsPartition, dev, wait)
	}
	mounted := 0
	for _, p := range parts {
		mnt, ok := mountNTFS(ctx, r, p.Path, mode)
		if !ok {
			continue
		}
		mounted++
		if _, err := os.Stat(filepath.Join(mnt, "Windows", "System32")); err != nil {
			unmount(ctx, r, mnt)
			continue
		}
		return func() error {
			defer releaseMount(r, mnt)
			return fn(mnt)
		}()
	}
	if mounted == 0 {
		return fmt.Errorf("%w: %s has %d NTFS partition(s) but none could be mounted", errNoWindowsPartition, dev, len(parts))
	}
	return fmt.Errorf(`%w: %s has %d mountable NTFS partition(s) but none carries \Windows\System32`, errNoWindowsPartition, dev, mounted)
}

// withEachPartition 只读挂载设备上所有可读分区并一次性交给 fn，镜像检查需要跨分区视图。
// 挂不上的分区跳过，fn 可能收到空集；只有设备本身列不出来才报错。挂载在 fn 返回后全部释放。
func withEachPartition(ctx context.Context, r runner, dev string, fn func(mounts []mountedFS) error) error {
	parts := waitPartitions(ctx, r, dev, anyFilesystem, clonePartitionWait)
	if parts == nil {
		// 轮询超时：区分「设备可列出但无文件系统」（继续检查，结果为未知）和「设备列不出」（检查失败）。
		var err error
		if parts, err = listInspectPartitions(ctx, r, dev); err != nil {
			return err
		}
	}
	lvs, releaseLVs, err := mapLVM(ctx, r, parts)
	defer releaseLVs() // defer 后进先出，保证在下面的卸载之后执行
	if err != nil {
		slog.Warn("logical volumes not mapped; inspecting the plain partitions only", "device", dev, "error", err)
	}
	parts = append(parts, lvs...)
	var mounts []mountedFS
	defer func() {
		for _, m := range mounts {
			releaseMount(r, m.Root)
		}
	}()
	for _, p := range parts {
		if m, ok := mountReadOnly(ctx, r, p); ok {
			mounts = append(mounts, m)
		}
	}
	return fn(mounts)
}

// releaseMount 用独立的清理 context 释放挂载，调用方 context 已取消也不能跳过释放。
func releaseMount(r runner, mnt string) {
	cctx, cancel := cleanupContext()
	defer cancel()
	unmount(cctx, r, mnt)
}

// partprobeAfter 是强制重扫分区表前等内核自行扫描的时间；总等待时长由调用方按场景决定。
const partprobeAfter = 600 * time.Millisecond

const (
	// clonePartitionWait 用于开机：客户机在等，克隆的分区节点通常不到一秒就出现。
	clonePartitionWait = 10 * time.Second
	// probePartitionWait 用于探查早已存在的数据集；恢复目录时要逐个镜像探查，等太久会拖慢启动。
	probePartitionWait = 2 * time.Second
	// importPartitionWait 用于导入：新收下的 zvol 分区表可能出现得很慢，放弃就烘焙不了，
	// 之后每次开机都要付注入的代价。
	importPartitionWait = 3 * time.Minute
)

// waitNTFSPartitions 等到出现 NTFS 分区再返回，这是 Windows 克隆就绪的信号。
func waitNTFSPartitions(ctx context.Context, r runner, dev string) []inspectPartition {
	return waitPartitions(ctx, r, dev, hasNTFS, clonePartitionWait)
}

// waitPartitions 轮询到分区列表满足 ready 为止，超时或取消返回 nil。
// 只用 lsblk 轮询，不反复 partprobe / `udevadm settle`：后者等的是全局 udev 队列，
// 开机风暴下 100 台并发时每台光 settle 就要约 8 秒。zvol 出现时内核会自行扫描分区表，
// /dev/zdNpM 由 devtmpfs 提供；只保留一次 partprobe 兜底，重复执行只会放大 uevent。
func waitPartitions(ctx context.Context, r runner, dev string, ready func([]inspectPartition) bool, budget time.Duration) []inspectPartition {
	start := time.Now()
	probed := false
	for {
		parts, err := listInspectPartitions(ctx, r, dev)
		if err == nil && ready(parts) {
			return parts
		}
		if time.Since(start) > budget {
			return nil
		}
		if !probed && time.Since(start) >= partprobeAfter {
			probed = true
			_, _ = r.Run(ctx, "partprobe", dev)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func hasNTFS(parts []inspectPartition) bool {
	for _, p := range parts {
		if strings.HasPrefix(strings.ToLower(p.FSType), "ntfs") {
			return true
		}
	}
	return false
}

func anyFilesystem(parts []inspectPartition) bool {
	return len(parts) > 0
}

type inspectPartition struct {
	Path   string
	FSType string
}

type mountedFS struct {
	Root   string
	FSType string
}

func listInspectPartitions(ctx context.Context, r runner, dev string) ([]inspectPartition, error) {
	out, err := r.Run(ctx, "lsblk", "-J", "-o", "PATH,FSTYPE", dev)
	if err != nil {
		return nil, fmt.Errorf("list partitions failed: %w", err)
	}
	var payload struct {
		Blockdevices []struct {
			Path     string `json:"path"`
			FSType   string `json:"fstype"`
			Children []struct {
				Path   string `json:"path"`
				FSType string `json:"fstype"`
			} `json:"children"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("invalid lsblk output: %w", err)
	}
	var parts []inspectPartition
	for _, d := range payload.Blockdevices {
		if len(d.Children) == 0 && strings.TrimSpace(d.FSType) != "" {
			// 没有分区表、文件系统直接写在卷上。
			parts = append(parts, inspectPartition{Path: d.Path, FSType: strings.TrimSpace(d.FSType)})
		}
		for _, c := range d.Children {
			if strings.TrimSpace(c.FSType) != "" {
				parts = append(parts, inspectPartition{Path: c.Path, FSType: strings.TrimSpace(c.FSType)})
			}
		}
	}
	return parts, nil
}

// mountNTFS 用 ntfs3 的 `force` 挂载：镜像的 $LogFile 非空，普通挂载会被拒绝，
// `force` 保留日志让 Windows 下次开机正常回放。不能用 `ntfsfix -d`，清空 $LogFile
// 会损坏元数据并让 Windows 进入 WinRE 自动修复。启动卷只允许一次读写挂载，
// 校验读取要走临时快照+克隆，不能重新挂载。
func mountNTFS(ctx context.Context, r runner, part string, mode mountMode) (string, bool) {
	dir, err := os.MkdirTemp("", "ndiskless-vol-*")
	if err != nil {
		return "", false
	}
	opts := "ro,force"
	if mode == volRW {
		opts = "rw,force"
	}
	if _, err := r.Run(ctx, "mount", "-t", "ntfs3", "-o", opts, part, dir); err != nil {
		discardFailedMount(ctx, r, dir)
		return "", false
	}
	return dir, true
}

// mountReadOnly 只读挂载一个分区。NTFS 优先 ntfs3，失败退回 ntfs-3g；不支持的文件系统跳过。
func mountReadOnly(ctx context.Context, r runner, p inspectPartition) (mountedFS, bool) {
	dir, err := os.MkdirTemp("", "ndiskless-vol-*")
	if err != nil {
		return mountedFS{}, false
	}
	fsType := strings.ToLower(p.FSType)
	var mountErr error
	switch {
	case strings.HasPrefix(fsType, "ntfs"):
		if _, mountErr = r.Run(ctx, "mount", "-t", "ntfs3", "-o", "ro", p.Path, dir); mountErr != nil {
			_, mountErr = r.Run(ctx, "ntfs-3g", "-o", "ro", p.Path, dir)
		}
	case fsType == "ext4" || fsType == "ext3" || fsType == "ext2" || fsType == "vfat" || fsType == "xfs":
		_, mountErr = r.Run(ctx, "mount", "-o", "ro", p.Path, dir)
	default:
		mountErr = fmt.Errorf("unsupported filesystem %q", p.FSType)
	}
	if mountErr != nil {
		_ = os.Remove(dir)
		return mountedFS{}, false
	}
	return mountedFS{Root: dir, FSType: fsType}, true
}

// discardFailedMount 在 mount 报错后收尾。mount(8) 若在系统调用成功后被取消杀掉，
// 会报错但挂载仍在，留下就会永久占住 zvol，所以删目录前先尝试卸载一次。
func discardFailedMount(ctx context.Context, r runner, dir string) {
	_, _ = r.Run(ctx, "umount", dir)
	_ = os.Remove(dir)
}

// unmount 释放临时挂载并删除目录。
func unmount(ctx context.Context, r runner, mnt string) {
	if detach(ctx, r, mnt) {
		_ = os.Remove(mnt)
	}
}

// detach 卸载但不删挂载点，用于属于下层文件系统的目录（如镜像自己的 /boot）。
// 短暂失败（回写、udev 重探）片刻即消，故短重试；仍失败就 lazy detach，此时文件已写完关闭。
// lazy detach 用独立 context：调用方超时正是 umount 最可能被中途杀掉的情形，不能连它一起取消。
func detach(ctx context.Context, r runner, mnt string) bool {
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if out, err = r.Run(ctx, "umount", mnt); err == nil {
			return true
		}
		// 调用方已取消或已是最后一次，就不再等待。
		if attempt == 2 || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	slog.Warn("umount failed; detaching lazily", "mount", mnt, "error", err, "output", strings.TrimSpace(string(out)))
	dctx, cancel := cleanupContext()
	defer cancel()
	if out, err = r.Run(dctx, "umount", "-l", mnt); err != nil {
		slog.Error("lazy umount failed; mount leaked", "mount", mnt, "error", err, "output", strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// releaseStaleMounts 在销毁克隆前强制卸载这些数据集的遗留挂载（进程被杀或 lazy 卸载也失败时留下），
// 让卡死在下次开机自愈。只碰调用方本就要销毁的数据集，不会打扰别的客户机正在用的挂载。
func (a *Agent) releaseStaleMounts(ctx context.Context, datasets []string) {
	if len(datasets) == 0 {
		return
	}
	doomed := make(map[string]bool, len(datasets))
	for _, dataset := range datasets {
		doomed[dataset] = true
	}
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	for _, m := range a.zvolMounts() {
		if !doomed[m.Dataset] {
			continue
		}
		slog.Warn("releasing leftover mount blocking clone destroy",
			"mount", m.Mountpoint, "device", m.Device, "dataset", m.Dataset)
		unmount(ctx, r, m.Mountpoint)
	}
}

func (a *Agent) zvolMounts() []zfs.ZvolMount {
	if a.zvolMountsFn != nil {
		return a.zvolMountsFn()
	}
	return zfs.ZvolMounts()
}
