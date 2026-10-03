package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Linux 镜像导入时写入的内容（根文件系统内路径）。initramfs 的 open-iscsi 只在被要求时登录：
// 内核参数 iscsi_auto（镜像现有 initrd 会读）或 /etc/iscsi/iscsi.initramfs 里的 ISCSI_AUTO
// （下次 update-initramfs 生效）。两者都写，再改 GRUB 默认值，update-grub 后参数仍在。
const (
	LinuxKernelParam        = "iscsi_auto"
	LinuxGrubCfgPath        = "boot/grub/grub.cfg"
	LinuxGrubDefaultPath    = "etc/default/grub"
	LinuxISCSIInitramfsPath = "etc/iscsi/iscsi.initramfs"
	LinuxISCSIInitramfsBody = "ISCSI_AUTO=true\n"

	LinuxMountScriptPath    = "usr/local/sbin/nd-datadisks"
	LinuxMountUnitPath      = "etc/systemd/system/nd-datadisks.service"
	LinuxMountUnitWantsPath = "etc/systemd/system/multi-user.target.wants/nd-datadisks.service"
	LinuxMountUnitBody      = "[Unit]\nDescription=ndiskless data disks\nWants=network-online.target\n" +
		"After=network-online.target systemd-udev-settle.service\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh /" + LinuxMountScriptPath + "\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
)

// LinuxLayoutID 之于 Linux 烘焙，相当于 MountScriptLayoutID 之于 Windows。
func LinuxLayoutID() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		LinuxKernelParam, LinuxGrubCfgPath, LinuxGrubDefaultPath, LinuxISCSIInitramfsPath, LinuxISCSIInitramfsBody,
		LinuxMountScriptPath, LinuxMountUnitPath, LinuxMountUnitWantsPath, LinuxMountUnitBody,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
