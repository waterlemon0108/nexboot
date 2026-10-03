package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
)

// 仅在真实的 Ubuntu Server LVM 布局盘上运行（根在 LV 上含 /etc/os-release，/boot 为独立 ext4 分区）：
// ND_LVM_DEVICE=/dev/zdN go test -run TestRealLVMDisk ./internal/storage/local
func TestRealLVMDisk(t *testing.T) {
	dev := os.Getenv("ND_LVM_DEVICE")
	if dev == "" {
		t.Skip("ND_LVM_DEVICE not set")
	}
	ctx := context.Background()
	r := execRunner{}
	if err := withLinuxRoot(ctx, r, dev, importPartitionWait, func(root string) error {
		return adaptLinuxRoot(root, []byte("#!/bin/sh\n"))
	}); err != nil {
		t.Fatalf("adapt: %v", err)
	}
	var items []string
	if err := withEachPartition(ctx, r, dev, func(mounts []mountedFS) error {
		for _, it := range inspectLinux(mounts) {
			items = append(items, it.Name+"="+string(it.Level))
			if it.Name == "iscsi_boot" && it.Level != domain.HealthOK {
				t.Errorf("iscsi_boot after adapt: %+v", it)
			}
			if it.Level == domain.HealthBlock {
				t.Errorf("blocked: %+v", it)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	t.Logf("inspect: %s", strings.Join(items, " "))
	out, _ := exec.Command("dmsetup", "ls").CombinedOutput()
	if strings.Contains(string(out), "nd-") {
		t.Errorf("mappings left behind:\n%s", out)
	}
	mounts, _ := os.ReadFile("/proc/mounts")
	if strings.Contains(string(mounts), "/dev/mapper/nd-") || strings.Contains(string(mounts), filepath.Base(dev)) {
		t.Errorf("mounts left behind")
	}
}
