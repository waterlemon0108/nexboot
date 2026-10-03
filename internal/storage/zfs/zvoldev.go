package zfs

// zvol 设备节点模块：devtmpfs 建的 /dev/zdN 节点、它们与数据集名的对应、何时出现、
// 何时释放独占、哪些被挂载。
// 正确性不依赖 udev 维护的 /dev/zvol 符号链接：开机风暴时 udev 事件队列落后数秒，链接可能缺失，
// 甚至仍指向已被 ZFS 回收给别的客户机克隆的 minor（即错盘）。内核的 BLKZNAME ioctl 没有这种滞后。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// blkZName 是 ZFS 的 BLKZNAME ioctl（_IOR(0x12, 125, char[256])），直接从内核取 /dev/zdN 背后的数据集名，
// udev 的 zvol_id 也用它建 /dev/zvol 树。
const blkZName = 0x8100127d

// ZvolName 读 zvol 节点对应的数据集名，是本模块的核心原语。
// 打开是非独占且短暂的；与 zfs destroy 竞争时调用失败，调用方一律当作「不是这个数据集」。
func ZvolName(node string) (string, error) {
	f, err := os.OpenFile(node, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf [256]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkZName, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	n := bytes.IndexByte(buf[:], 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), nil
}

// zdNodeRe 匹配整盘 zvol 节点，排除 zd0p1 这类分区。
var zdNodeRe = regexp.MustCompile(`^zd\d+$`)

// IsZvolNode 判断块设备名是否为池分配的 zvol（镜像、配置、客户机克隆）。它们以 /dev/zdN 出现，
// lsblk 也报 TYPE=disk，向操作者列出可用磁盘时必须区分开。
func IsZvolNode(name string) bool { return zdNodeRe.MatchString(name) }

// zdPartRe 把 zvol 分区节点折回整盘节点：数据集名在 /dev/zdN 上，不在 /dev/zdNpM 上。
var zdPartRe = regexp.MustCompile(`^(/dev/zd\d+)(?:p\d+)?$`)

// 两个等待预算有意分开：解析等的是 zvol 创建（风暴时 minor 注册会排队），
// 释放等的是内核对独占的短暂延迟拆除。
const (
	resolveTimeout = 15 * time.Second
	resolvePoll    = 20 * time.Millisecond
	releaseTimeout = 5 * time.Second
	releasePoll    = 10 * time.Millisecond
)

// ResolveNode 把 /dev/zvol/<pool>/<dataset> 映射为真实的 /dev/zdN：逐个询问 zd 设备的数据集名，
// 并等过新建 zvol 尚无节点的窗口。非 zvol 路径只等路径出现后原样返回。
// `zfs clone` 返回时节点还不一定可用，这里的 ENOENT 是时间窗口，不是错误。
func ResolveNode(ctx context.Context, dev string) (string, error) {
	dataset, ok := strings.CutPrefix(dev, "/dev/zvol/")
	if !ok {
		return pollNode(ctx, func() (string, bool) {
			_, err := os.Stat(dev)
			return dev, err == nil
		}, func() error { return fmt.Errorf("device %s did not appear within 15s", dev) })
	}
	return pollNode(ctx, func() (string, bool) {
		return findNode(dataset)
	}, func() error { return fmt.Errorf("zvol %s has no block device after 15s", dataset) })
}

// pollNode 在共享的解析预算内每 resolvePoll 重试一次 probe，两次之间响应 ctx 取消。
func pollNode(ctx context.Context, probe func() (string, bool), timeoutErr func() error) (string, error) {
	deadline := time.Now().Add(resolveTimeout)
	for {
		if node, found := probe(); found {
			return node, nil
		}
		if time.Now().After(deadline) {
			return "", timeoutErr()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(resolvePoll):
		}
	}
}

func findNode(dataset string) (string, bool) {
	nodes, _ := filepath.Glob("/dev/zd*")
	for _, node := range nodes {
		if !zdNodeRe.MatchString(filepath.Base(node)) {
			continue
		}
		if name, err := ZvolName(node); err == nil && name == dataset {
			return node, true
		}
	}
	return "", false
}

// WaitReleased 阻塞到块设备的独占声明消失。configfs rmdir 在内核延迟释放完成前就返回，
// 此时 zfs destroy 会报 "dataset is busy"。按 open(2)，块设备的 O_EXCL 打开恰在无人声明时成功。
func WaitReleased(dev string) error {
	deadline := time.Now().Add(releaseTimeout)
	for {
		f, err := os.OpenFile(dev, os.O_RDONLY|os.O_EXCL, 0)
		if err == nil {
			f.Close()
			return nil
		}
		if os.IsNotExist(err) {
			return nil // 设备已不存在
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s still claimed after teardown: %w", dev, err)
		}
		time.Sleep(releasePoll)
	}
}

// ZvolMount 是一个已挂载的 zvol 分区，及内核报告的其整盘节点所属数据集。
type ZvolMount struct {
	Device     string
	Mountpoint string
	Dataset    string
}

// ZvolMounts 列出所有已挂载的 zvol 分区及其数据集。
func ZvolMounts() []ZvolMount {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	return parseZvolMounts(data, ZvolName)
}

// parseZvolMounts 是 ZvolMounts 的纯函数部分，拆出来以便用假 nameOf 测行解析和分区折叠。
// 读取的两个字段是内核生成的设备路径和 mkdtemp 路径，不会出现 /proc 对空白的八进制转义。
func parseZvolMounts(data []byte, nameOf func(node string) (string, error)) []ZvolMount {
	var out []ZvolMount
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		node := zdPartRe.FindStringSubmatch(fields[0])
		if node == nil {
			continue
		}
		dataset, err := nameOf(node[1])
		if err != nil {
			continue
		}
		out = append(out, ZvolMount{Device: fields[0], Mountpoint: fields[1], Dataset: dataset})
	}
	return out
}
