package storage

import "strings"

// SameDisk 判断两个设备名是否指同一块盘。ZFS 拿到整盘会自行分区并以第一个分区报告成员
// （/dev/sda -> /dev/sda1、nvme0n1 -> nvme0n1p1、by-id -> ...-part1），而操作者和 zpool 命令用整盘名。
// 只认这一种后缀：/dev/sda11 不等于 /dev/sda1。
func SameDisk(a, b string) bool {
	if a == b {
		return true
	}
	return isFirstPartitionOf(a, b) || isFirstPartitionOf(b, a)
}

func isFirstPartitionOf(part, whole string) bool {
	rest, ok := strings.CutPrefix(part, whole)
	if !ok || whole == "" {
		return false
	}
	endsInDigit := whole[len(whole)-1] >= '0' && whole[len(whole)-1] <= '9'
	switch rest {
	case "1": // sda -> sda1；sda1 不是 sda11 的整盘
		return !endsInDigit
	case "p1": // nvme0n1 -> nvme0n1p1, mmcblk0 -> mmcblk0p1
		return endsInDigit
	case "-part1": // /dev/disk/by-id/... -> ...-part1
		return true
	}
	return false
}
