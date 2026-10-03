package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

var errNoGrubConfig = errors.New("根分区和单独的 /boot 分区里都没有 grub/grub.cfg，无法自动加上 iSCSI 开机参数")

var (
	grubLinuxLineRe  = regexp.MustCompile(`(?m)^([ \t]*linux(?:efi|16)?[ \t]+\S.*?)[ \t]*$`)
	grubDefaultCmdRe = regexp.MustCompile(`(?m)^GRUB_CMDLINE_LINUX="([^"]*)"[ \t]*$`)
)

// adaptLinuxRoot 让已挂载的 Linux 根能从 iSCSI 启动并挂载数据盘，幂等。
func adaptLinuxRoot(root string, script []byte) error {
	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	cfg, err := os.ReadFile(at(storage.LinuxGrubCfgPath))
	if err != nil {
		return errNoGrubConfig
	}
	if err := os.WriteFile(at(storage.LinuxGrubCfgPath), []byte(addKernelParam(string(cfg))), 0o644); err != nil {
		return err
	}
	if err := addGrubDefaultParam(at(storage.LinuxGrubDefaultPath)); err != nil {
		return err
	}
	files := []struct {
		rel  string
		body []byte
		mode os.FileMode
	}{
		{storage.LinuxISCSIInitramfsPath, []byte(storage.LinuxISCSIInitramfsBody), 0o644},
		{storage.LinuxMountScriptPath, script, 0o755},
		{storage.LinuxMountUnitPath, []byte(storage.LinuxMountUnitBody), 0o644},
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(at(f.rel)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(at(f.rel), f.body, f.mode); err != nil {
			return err
		}
		if err := os.Chmod(at(f.rel), f.mode); err != nil {
			return err
		}
	}
	if err := dropStaticAddresses(root); err != nil {
		return err
	}
	wants := at(storage.LinuxMountUnitWantsPath)
	if err := os.MkdirAll(filepath.Dir(wants), 0o755); err != nil {
		return err
	}
	_ = os.Remove(wants)
	return os.Symlink("/"+storage.LinuxMountUnitPath, wants)
}

func addKernelParam(cfg string) string {
	return grubLinuxLineRe.ReplaceAllStringFunc(cfg, func(line string) string {
		line = strings.TrimRight(line, " \t")
		if slices.Contains(strings.Fields(line), storage.LinuxKernelParam) {
			return line
		}
		return line + " " + storage.LinuxKernelParam
	})
}

func addGrubDefaultParam(path string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(b)
	if m := grubDefaultCmdRe.FindStringSubmatch(content); m != nil {
		if slices.Contains(strings.Fields(m[1]), storage.LinuxKernelParam) {
			return nil
		}
		value := strings.TrimSpace(m[1] + " " + storage.LinuxKernelParam)
		content = grubDefaultCmdRe.ReplaceAllLiteralString(content, `GRUB_CMDLINE_LINUX="`+value+`"`)
	} else {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += `GRUB_CMDLINE_LINUX="` + storage.LinuxKernelParam + "\"\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// bakeLinux 对刚收下的 Linux 镜像做适配（见 adaptLinuxRoot）。
func (a *Agent) bakeLinux(ctx context.Context, dev string, script []byte) error {
	if a.adaptLinuxFn != nil {
		return a.adaptLinuxFn(ctx, dev, script)
	}
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	return withLinuxRoot(ctx, r, dev, importPartitionWait, func(root string) error {
		return adaptLinuxRoot(root, script)
	})
}

var errNoLinuxRoot = errors.New("no Linux root filesystem found")

// withLinuxRoot 以读写方式挂载设备上的 Linux 根（含 /etc/os-release 的分区）并对其执行 fn。
func withLinuxRoot(ctx context.Context, r runner, dev string, wait time.Duration, fn func(root string) error) error {
	// 既无分区表也无文件系统的卷不必等根分区；刚收下的卷设备节点可能滞后，先等节点出现。
	if waitDeviceNode(ctx, r, dev, wait) && detectPartitionStyle(ctx, r, dev) == "" && wholeVolumeFS(ctx, r, dev) == "" {
		return fmt.Errorf("%w: %s has no partition table or filesystem", errNoLinuxRoot, dev)
	}
	start := time.Now()
	parts := waitPartitions(ctx, r, dev, func(p []inspectPartition) bool { return hasLinuxFS(p) || hasLVM(p) }, wait)
	if len(parts) == 0 {
		return fmt.Errorf("%w: no ext4/xfs or LVM partition appeared on %s within %s", errNoLinuxRoot, dev, wait)
	}
	first := time.Now()
	for {
		parts = settledPartitions(ctx, r, dev, parts)
		found, err := withRootIn(ctx, r, parts, fn)
		// 找到了，或是重试也改变不了的失败（不支持的卷）。
		if found || err != nil {
			return err
		}
		// 晚识别的分区几秒内就会出现；本来就没有的根（btrfs、加密）永远等不到。
		if time.Since(first) > rootRetryBudget || time.Since(start) > wait || ctx.Err() != nil {
			return fmt.Errorf("%w on %s", errNoLinuxRoot, dev)
		}
		// 根分区可能还没被识别类型，再看一次。
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		if next, err := listInspectPartitions(ctx, r, dev); err == nil {
			parts = next
		}
	}
}

// withRootIn 在 parts 中找到 Linux 根时对其执行 fn，返回是否找到。
func withRootIn(ctx context.Context, r runner, parts []inspectPartition, fn func(root string) error) (bool, error) {
	lvs, releaseLVs, err := mapLVM(ctx, r, parts)
	defer releaseLVs()
	if err != nil {
		return false, fmt.Errorf("%w: %v", errNoLinuxRoot, err)
	}
	parts = append(parts, lvs...)
	for i, p := range parts {
		if !linuxFS(p.FSType) {
			continue
		}
		dir, err := os.MkdirTemp("", "ndiskless-vol-*")
		if err != nil {
			return false, err
		}
		// 先只读探测，只对根分区改为可写（只读挂载仍会回放脏日志，那是镜像自身的数据）。
		if _, err := r.Run(ctx, "mount", "-o", "ro", p.Path, dir); err != nil {
			discardFailedMount(ctx, r, dir)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "etc", "os-release")); err != nil {
			releaseMount(r, dir)
			continue
		}
		if _, err := r.Run(ctx, "mount", "-o", "remount,rw", dir); err != nil {
			releaseMount(r, dir)
			return true, fmt.Errorf("remount the root read-write: %w", err)
		}
		return true, func() error {
			defer releaseMount(r, dir)
			if boot := mountSeparateBoot(ctx, r, dir, parts, i); boot != "" {
				defer func() {
					cctx, cancel := cleanupContext()
					defer cancel()
					detach(cctx, r, boot)
				}()
			}
			return fn(dir)
		}()
	}
	return false, nil
}

// settledPartitions 等 udev 探测完新卷：分区是逐个识别的，/boot 出现而 LVM 成员未出现时找根会落空。
// 连续两次列表一致即视为稳定。
func settledPartitions(ctx context.Context, r runner, dev string, parts []inspectPartition) []inspectPartition {
	for range 10 {
		select {
		case <-ctx.Done():
			return parts
		case <-time.After(settleInterval):
		}
		next, err := listInspectPartitions(ctx, r, dev)
		if err != nil {
			return parts
		}
		if slices.Equal(next, parts) {
			return parts
		}
		parts = next
	}
	return parts
}

var settleInterval = 500 * time.Millisecond

// rootRetryBudget 限定首轮没找到根时重找的时长。
var rootRetryBudget = 20 * time.Second

// mountSeparateBoot 在根里没有 grub.cfg 时（LVM 安装的 /boot 是独立分区），把含 grub/grub.cfg 的分区
// 挂到根的 /boot 上并返回挂载点，否则返回 ""。
func mountSeparateBoot(ctx context.Context, r runner, root string, parts []inspectPartition, rootIdx int) string {
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(storage.LinuxGrubCfgPath))); err == nil {
		return ""
	}
	boot := filepath.Join(root, "boot")
	if err := os.MkdirAll(boot, 0o755); err != nil {
		return ""
	}
	for i, p := range parts {
		if i == rootIdx || !linuxFS(p.FSType) {
			continue
		}
		// 逐个只读探测其它分区，只有 /boot 改为可写。
		if _, err := r.Run(ctx, "mount", "-o", "ro", p.Path, boot); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(boot, "grub", "grub.cfg")); err == nil {
			if _, err := r.Run(ctx, "mount", "-o", "remount,rw", boot); err == nil {
				return boot
			}
		}
		cctx, cancel := cleanupContext()
		detach(cctx, r, boot)
		cancel()
	}
	return ""
}

func linuxFS(fsType string) bool {
	switch strings.ToLower(fsType) {
	case "ext2", "ext3", "ext4", "xfs":
		return true
	}
	return false
}

func hasLinuxFS(parts []inspectPartition) bool {
	return slices.ContainsFunc(parts, func(p inspectPartition) bool { return linuxFS(p.FSType) })
}

// waitDeviceNode 报告设备节点是否在 budget 内出现。
func waitDeviceNode(ctx context.Context, r runner, dev string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if _, err := r.Run(ctx, "test", "-e", dev); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func wholeVolumeFS(ctx context.Context, r runner, dev string) string {
	out, err := r.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ndDHCPNetplan 替代被 dropStaticAddresses 挪走的静态配置。
const ndDHCPNetplan = `# ndiskless: 客户机地址由服务器 DHCP 按 MAC 固定分配，镜像里不写静态 IP
network:
  version: 2
  ethernets:
    nd-en:
      match:
        name: "en*"
      dhcp4: true
    nd-eth:
      match:
        name: "eth*"
      dhcp4: true
`

// dropStaticAddresses 挪走写死地址的网络配置。所有客户机共用一个镜像，写死的地址会被所有机器同时占用，
// 若恰是服务器地址还会切断自己的 iSCSI 盘；地址改由服务器 DHCP 按 MAC 固定分配。
func dropStaticAddresses(root string) error {
	moved := false
	for _, pattern := range []string{"etc/netplan/*.yaml", "etc/netplan/*.yml"} {
		files, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil || !netplanHasStaticAddress(string(b)) {
				continue
			}
			if err := os.Rename(f, f+".nd-bak"); err != nil {
				return err
			}
			moved = true
		}
	}
	if moved {
		if err := os.WriteFile(filepath.Join(root, "etc", "netplan", "01-ndiskless-dhcp.yaml"), []byte(ndDHCPNetplan), 0o600); err != nil {
			return err
		}
	}
	profiles, _ := filepath.Glob(filepath.Join(root, "etc", "NetworkManager", "system-connections", "*.nmconnection"))
	for _, f := range profiles {
		b, err := os.ReadFile(f)
		if err != nil || !nmProfileIsManual(string(b)) {
			continue
		}
		if err := os.Rename(f, f+".nd-bak"); err != nil {
			return err
		}
	}
	return nil
}

// netplanHasStaticAddress 判断是否有不属于 nameservers: 下的 addresses: 键。
// 靠缩进判断即可，为一个键不值得引入 YAML 解析依赖。
func netplanHasStaticAddress(src string) bool {
	type key struct {
		indent int
		name   string
	}
	var stack []key
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		name, _, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		indent := len(line) - len(trimmed)
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if strings.Trim(name, `"' `) == "addresses" && (len(stack) == 0 || stack[len(stack)-1].name != "nameservers") {
			return true
		}
		stack = append(stack, key{indent, strings.Trim(name, `"' `)})
	}
	return false
}

// nmProfileIsManual 判断 NetworkManager keyfile 的 [ipv4] 是否为 manual。
func nmProfileIsManual(src string) bool {
	section := ""
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section == "[ipv4]" && strings.ReplaceAll(line, " ", "") == "method=manual" {
			return true
		}
	}
	return false
}
