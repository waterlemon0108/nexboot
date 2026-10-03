package local

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// blankFilesystems 记录每种可选文件系统对应的客户机系统、sgdisk 用的 GPT 类型码和 mkfs 命令。
var blankFilesystems = map[string]struct {
	os      domain.OSType
	gptType string
	mkfs    func(label, part string) []string
}{
	"ntfs": {domain.OSTypeWindows, "0700", func(label, part string) []string {
		args := []string{"mkfs.ntfs", "-Q", "-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
		return append(args, part)
	}},
	"ext4": {domain.OSTypeLinux, "8300", func(label, part string) []string {
		args := []string{"mkfs.ext4", "-F", "-q"}
		if label != "" {
			args = append(args, "-L", label)
		}
		return append(args, part)
	}},
}

const blankPartitionWait = 15 * time.Second

// CreateBlankImage 新建空白数据盘（内容由超管机写入而非导入）：稀疏卷 + GPT 分区表 + 一个格式化分区，
// 之后与导入走同样的收尾。分区表不能省：客户机脚本按「最大的普通数据分区」找数据盘，
// 而 Windows 会把没有分区表的卷显示为未初始化磁盘。
func (a *Agent) CreateBlankImage(ctx context.Context, req storage.BlankImageReq) (storage.ImportImageResult, error) {
	name := strings.TrimSpace(req.Name)
	fs, ok := blankFilesystems[strings.ToLower(strings.TrimSpace(req.Filesystem))]
	if name == "" || req.SizeBytes <= 0 || !ok {
		return storage.ImportImageResult{}, fmt.Errorf("name, a positive size and a filesystem of ntfs or ext4 are required")
	}
	// 优先用填写的卷标，否则用盘名，都裁到文件系统允许的长度。
	labelSource := strings.TrimSpace(req.Label)
	if labelSource == "" {
		labelSource = name
	}
	label := volumeLabel(labelSource, strings.ToLower(strings.TrimSpace(req.Filesystem)))
	report := func(percent int, message string) {
		if req.OnProgress != nil {
			req.OnProgress(percent, message)
		}
	}
	report(pgPrepare, "准备")
	datasets, err := a.existingDatasets(ctx)
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	imageID := storage.ImageName(name, func(id string) bool { return datasets[a.zfs.Dataset(id)] })
	imageDataset := a.zfs.Dataset(imageID)
	configID := storage.DefaultConfigName(imageID)
	if datasets[imageDataset] || datasets[a.zfs.Dataset(configID)] {
		return storage.ImportImageResult{}, fmt.Errorf("target dataset already exists")
	}
	if req.OnTarget != nil {
		req.OnTarget(imageID)
	}
	report(pgCreateVol, "创建卷")
	if err := a.zfs.CreateVolume(ctx, imageDataset, req.SizeBytes, defaultVolblockBytes); err != nil {
		return storage.ImportImageResult{}, err
	}
	fail := func(err error) (storage.ImportImageResult, error) {
		a.destroyDetached(imageDataset)
		return storage.ImportImageResult{}, err
	}
	node, err := a.resolveNode(ctx, a.zfs.VolumePath(imageDataset))
	if err != nil {
		return fail(err)
	}
	report(pgCopyStart, "分区")
	sgdisk := []string{"-Z", "-n", "1:1MiB:0", "-t", "1:" + fs.gptType}
	if label != "" {
		sgdisk = append(sgdisk, "-c", "1:"+label)
	}
	if err := a.runOS(ctx, "sgdisk", append(sgdisk, node)...); err != nil {
		return fail(err)
	}
	if err := a.runOS(ctx, "partprobe", node); err != nil {
		return fail(err)
	}
	// partprobe 返回时 udev 事件仍在排队，会删掉再重建分区节点，mkfs 打开时节点可能已消失。
	// 不 settle 时 20 次失败 9 次，settle 后 0 次。
	if err := a.runOS(ctx, "udevadm", "settle", "--timeout=30"); err != nil {
		return fail(err)
	}
	part := partitionNode(node, 1)
	if err := a.waitForNode(ctx, part); err != nil {
		return fail(fmt.Errorf("partition %s did not appear: %w", part, err))
	}
	report(copyPercent(50), "格式化 "+strings.ToUpper(req.Filesystem))
	if err := a.runOS(ctx, fs.mkfs(label, part)[0], fs.mkfs(label, part)[1:]...); err != nil {
		return fail(err)
	}
	return a.finishImage(ctx, imageID, configID, report, domain.Image{
		Name: name, OSType: fs.os, Size: req.SizeBytes, Purpose: domain.ImagePurposeData, Origin: domain.ImageOriginBlank,
	}, false)
}

// runOS 经 runner 执行宿主机命令，失败包装成 storage.CommandError，供 OperatorMessage 翻译。
func (a *Agent) runOS(ctx context.Context, name string, args ...string) error {
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		return storage.CommandError{Name: name, Args: args, Output: string(out), Err: err}
	}
	return nil
}

// partitionNode 返回块设备第 n 个分区的内核名：以数字结尾的设备名要加 "p"，
// 如 /dev/zd16 -> /dev/zd16p1，/dev/sdb -> /dev/sdb1。
func partitionNode(dev string, n int) string {
	if dev != "" && dev[len(dev)-1] >= '0' && dev[len(dev)-1] <= '9' {
		return fmt.Sprintf("%sp%d", dev, n)
	}
	return fmt.Sprintf("%s%d", dev, n)
}

// waitForNode 等待内核异步创建的设备节点出现。
func (a *Agent) waitForNode(ctx context.Context, path string) error {
	if a.waitForNodeFn != nil {
		return a.waitForNodeFn(ctx, path)
	}
	deadline := time.Now().Add(blankPartitionWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// volumeLabel 把卷标裁到文件系统上限（NTFS 32 字符、ext4 16 字节）并去掉 Windows 不接受的字符；
// 按 rune 截断，中文不会被截成半个字。
func volumeLabel(name, fs string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	label := strings.TrimSpace(b.String())
	switch fs {
	case "ext4":
		for len(label) > 16 {
			_, size := utf8.DecodeLastRuneInString(label)
			label = label[:len(label)-size]
		}
	default:
		if runes := []rune(label); len(runes) > 32 {
			label = string(runes[:32])
		}
	}
	return strings.TrimSpace(label)
}
