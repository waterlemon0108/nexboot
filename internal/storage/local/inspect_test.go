package local

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func itemLevel(t *testing.T, items []storage.InspectItem, name string) domain.HealthLevel {
	t.Helper()
	for _, item := range items {
		if item.Name == name {
			return item.Level
		}
	}
	t.Fatalf("item %q missing in %#v", name, items)
	return ""
}

const nicINF = `[Version]
Signature="$Windows NT$"
Class=Net
ClassGuid={4d36e972-e325-11ce-bfc1-08002be10318}
Provider=%Intel%
DriverVer=06/01/2024,12.19.2.60

[Manufacturer]
%Intel%=Intel,NTamd64

[Intel.NTamd64]
%Dev1% = Install, PCI\VEN_8086&DEV_15B8

[Strings]
Intel="Intel"
Dev1="Intel Ethernet"
`

func TestInspectWindowsHealthyTree(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"Windows/System32/drivers/msiscsi.sys":                                "bin",
		"Windows/System32/DriverStore/FileRepository/e1d.inf_amd64_x/e1d.inf": nicINF,
		"Boot/BCD": "bcd",
	})
	items := inspectMounts([]mountedFS{{Root: root, FSType: "ntfs"}}, domain.OSTypeWindows)

	if got := itemLevel(t, items, "windows_partition"); got != domain.HealthOK {
		t.Fatalf("windows_partition = %s", got)
	}
	if got := itemLevel(t, items, "msiscsi"); got != domain.HealthOK {
		t.Fatalf("msiscsi = %s", got)
	}
	if got := itemLevel(t, items, "bcd"); got != domain.HealthOK {
		t.Fatalf("bcd = %s", got)
	}
	if got := itemLevel(t, items, "driverstore_nic"); got != domain.HealthOK {
		t.Fatalf("driverstore_nic = %s", got)
	}
	if got := itemLevel(t, items, "boot_registry"); got != domain.HealthUnknown {
		t.Fatalf("boot_registry = %s", got)
	}
	// boot_registry 未知时整体结论保持 unknown，不能判 ok。
	if level := aggregateLevel(items); level != domain.HealthUnknown {
		t.Fatalf("aggregate = %s", level)
	}
}

func TestInspectWindowsMissingMsiscsiBlocks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"Windows/System32/kernel32.dll": "bin",
		"Boot/BCD":                      "bcd",
	})
	items := inspectMounts([]mountedFS{{Root: root, FSType: "ntfs"}}, domain.OSTypeWindows)

	if got := itemLevel(t, items, "msiscsi"); got != domain.HealthBlock {
		t.Fatalf("msiscsi = %s", got)
	}
	if got := itemLevel(t, items, "driverstore_nic"); got != domain.HealthWarn {
		t.Fatalf("driverstore_nic = %s", got)
	}
	if level := aggregateLevel(items); level != domain.HealthBlock {
		t.Fatalf("aggregate = %s", level)
	}
}

func TestInspectWindowsCaseInsensitiveLookup(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"WINDOWS/system32/DRIVERS/MSISCSI.SYS": "bin",
	})
	items := inspectMounts([]mountedFS{{Root: root, FSType: "ntfs"}}, domain.OSTypeWindows)
	if got := itemLevel(t, items, "msiscsi"); got != domain.HealthOK {
		t.Fatalf("msiscsi = %s", got)
	}
}

func TestInspectWindowsNoSystemPartition(t *testing.T) {
	items := inspectMounts(nil, domain.OSTypeWindows)
	if got := itemLevel(t, items, "windows_partition"); got != domain.HealthBlock {
		t.Fatalf("windows_partition = %s", got)
	}
	if level := aggregateLevel(items); level != domain.HealthBlock {
		t.Fatalf("aggregate = %s", level)
	}
}

func TestInspectLinuxOpenISCSI(t *testing.T) {
	withISCSI := t.TempDir()
	writeTree(t, withISCSI, map[string]string{
		"etc/fstab":           "/",
		"usr/sbin/iscsistart": "bin",
	})
	items := inspectMounts([]mountedFS{{Root: withISCSI, FSType: "ext4"}}, domain.OSTypeLinux)
	if got := itemLevel(t, items, "open_iscsi"); got != domain.HealthOK {
		t.Fatalf("open_iscsi = %s", got)
	}

	withoutISCSI := t.TempDir()
	writeTree(t, withoutISCSI, map[string]string{"etc/fstab": "/"})
	items = inspectMounts([]mountedFS{{Root: withoutISCSI, FSType: "ext4"}}, domain.OSTypeLinux)
	if got := itemLevel(t, items, "open_iscsi"); got != domain.HealthBlock {
		t.Fatalf("open_iscsi = %s", got)
	}
	if level := aggregateLevel(items); level != domain.HealthBlock {
		t.Fatalf("aggregate = %s", level)
	}
}

func TestInspectImageClonesMountsAndCleansUp(t *testing.T) {
	zfsRunner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", zfsRunner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	runner := &fakeRunner{lsblkOutput: `{"blockdevices":[{"path":"/dev/zvol/tank/nd/x","fstype":"","children":[{"path":"/dev/zvol/tank/nd/x-part1","fstype":"ntfs"}]}]}`}
	agent.runner = runner

	report, err := agent.InspectImage(context.Background(), storage.InspectImageReq{
		ImageID: "win11", ConfigID: "win11_default", SnapshotName: "0", OSType: domain.OSTypeWindows,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 挂载目录为空：检查跑完一无所获，判 block，且临时克隆照样清理。
	if report.Level != domain.HealthBlock {
		t.Fatalf("level = %s", report.Level)
	}

	if len(zfsRunner.calls) != 2 || zfsRunner.calls[0].args[0] != "clone" || zfsRunner.calls[1].args[0] != "destroy" {
		t.Fatalf("zfs calls = %#v", zfsRunner.calls)
	}
	if zfsRunner.calls[0].args[1] != "tank/nd/win11_default@0" || !strings.HasPrefix(zfsRunner.calls[0].args[2], "tank/run/INSPECT-WIN11-") {
		t.Fatalf("clone args = %#v", zfsRunner.calls[0].args)
	}
	if zfsRunner.calls[1].args[len(zfsRunner.calls[1].args)-1] != zfsRunner.calls[0].args[2] {
		t.Fatalf("destroy should target the temporary clone: %#v", zfsRunner.calls)
	}

	var names []string
	for _, call := range runner.calls {
		names = append(names, call.name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mount") || !strings.Contains(joined, "umount") {
		t.Fatalf("runner calls = %v", names)
	}
}

func TestInspectImageDestroysCloneOnFailure(t *testing.T) {
	zfsRunner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", zfsRunner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	// lsblk 始终无有效输出：分区轮询放弃（这里由测试截止时间限定），体检失败，克隆须被删除。
	runner := &fakeRunner{errAt: 1, err: os.ErrPermission}
	agent.runner = runner

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := agent.InspectImage(ctx, storage.InspectImageReq{
		ImageID: "win11", ConfigID: "win11_default", SnapshotName: "0", OSType: domain.OSTypeWindows,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(zfsRunner.calls) != 2 || zfsRunner.calls[1].args[0] != "destroy" {
		t.Fatalf("temporary clone must be destroyed on failure: %#v", zfsRunner.calls)
	}
}

func TestDetectBootModes(t *testing.T) {
	esp := t.TempDir()
	writeTree(t, esp, map[string]string{"EFI/Boot/bootx64.efi": "efi"})
	rootfs := t.TempDir()
	writeTree(t, rootfs, map[string]string{"etc/fstab": "/"})
	const biosGrub = "21686148-6449-6e6f-744e-656564454649"

	for _, tc := range []struct {
		name      string
		style     string
		partTypes []string
		mounts    []mountedFS
		want      string
	}{
		{"GPT 带 BIOS boot 分区、无 ESP", "gpt", []string{biosGrub, "0fc63daf-8483-4772-8e79-3d69d8477de4"}, []mountedFS{{Root: rootfs}}, "bios"},
		{"GPT 只有 ESP", "gpt", []string{"c12a7328-f81f-11d2-ba4b-00a0c93ec93b"}, []mountedFS{{Root: esp}, {Root: rootfs}}, "uefi"},
		{"MBR 带 ESP", "mbr", []string{"0xef", "0x83"}, []mountedFS{{Root: esp}, {Root: rootfs}}, "bios,uefi"},
		{"MBR 无 ESP", "mbr", nil, []mountedFS{{Root: rootfs}}, "bios"},
		{"GPT 两样都没有：判不出", "gpt", nil, []mountedFS{{Root: rootfs}}, ""},
		{"分区表不认识：判不出", "", nil, []mountedFS{{Root: rootfs}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(detectBootModes(tc.style, tc.partTypes, tc.mounts), ","); got != tc.want {
				t.Fatalf("boot modes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInspectImageReportsBootModes(t *testing.T) {
	zfsRunner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", zfsRunner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = &fakeRunner{lsblkOutput: `{"blockdevices":[{"path":"/dev/zvol/tank/nd/x","fstype":"","parttype":null,"children":[` +
		`{"path":"/dev/zvol/tank/nd/x-part1","fstype":null,"parttype":"21686148-6449-6e6f-744e-656564454649"},` +
		`{"path":"/dev/zvol/tank/nd/x-part2","fstype":"ext4","parttype":"0fc63daf-8483-4772-8e79-3d69d8477de4"}]}]}`}
	report, err := agent.InspectImage(context.Background(), storage.InspectImageReq{
		ImageID: "ubuntu", ConfigID: "ubuntu_default", SnapshotName: "0", OSType: domain.OSTypeLinux,
	})
	if err != nil {
		t.Fatal(err)
	}
	// fakeRunner 的 blkid 不报分区表类型，只能靠 BIOS boot 分区判出来。
	if got := strings.Join(report.BootModes, ","); got != "bios" {
		t.Fatalf("boot modes = %q", got)
	}
}

// 真机 util-linux 2.39 不带 NAME 列时输出扁平列表，没有 children。
func TestPartitionTypesReadsFlatLsblkOutput(t *testing.T) {
	r := &fakeRunner{lsblkOutput: `{"blockdevices":[{"path":"/dev/zd64","fstype":null,"parttype":null},` +
		`{"path":"/dev/zd64p1","fstype":null,"parttype":"21686148-6449-6E6F-744E-656564454649"},` +
		`{"path":"/dev/zd64p2","fstype":"ext4","parttype":"0fc63daf-8483-4772-8e79-3d69d8477de4"}]}`}
	got := partitionTypes(context.Background(), r, "/dev/zvol/tank/nd/ubuntu")
	if !slices.Contains(got, biosBootPartType) {
		t.Fatalf("types = %v", got)
	}
}

func TestInspectLinuxWarnsWhenTheInitramfsWillNotLogIn(t *testing.T) {
	bare := t.TempDir()
	writeTree(t, bare, map[string]string{
		"etc/fstab": "/", "usr/sbin/iscsistart": "bin",
		"boot/grub/grub.cfg": "linux /boot/vmlinuz root=UUID=x ro\n",
	})
	items := inspectMounts([]mountedFS{{Root: bare, FSType: "ext4"}}, domain.OSTypeLinux)
	if got := itemLevel(t, items, "iscsi_boot"); got != domain.HealthWarn {
		t.Fatalf("iscsi_boot = %s", got)
	}

	adapted := t.TempDir()
	writeTree(t, adapted, map[string]string{
		"etc/fstab": "/", "usr/sbin/iscsistart": "bin",
		"boot/grub/grub.cfg": "linux /boot/vmlinuz root=UUID=x ro iscsi_auto\n",
	})
	items = inspectMounts([]mountedFS{{Root: adapted, FSType: "ext4"}}, domain.OSTypeLinux)
	if got := itemLevel(t, items, "iscsi_boot"); got != domain.HealthOK {
		t.Fatalf("iscsi_boot = %s", got)
	}
}

// 开机时 iPXE 只报启动网卡的 PCI 厂商号:设备号；体检把镜像里网卡驱动覆盖的同一级编号存下来，
// 才能比出「这台的网卡镜像里没驱动」。
func TestInspectWindowsListsTheNICsItsDriversCover(t *testing.T) {
	root := t.TempDir()
	realtek := strings.Replace(strings.Replace(nicINF, `PCI\VEN_8086&DEV_15B8`, `PCI\VEN_10EC&DEV_8125&SUBSYS_012310EC&REV_05`, 1), "Intel", "Realtek", -1)
	writeTree(t, root, map[string]string{
		"Windows/System32/drivers/msiscsi.sys":                                  "bin",
		"Windows/System32/DriverStore/FileRepository/e1d.inf_amd64_x/e1d.inf":   nicINF,
		"Windows/System32/DriverStore/FileRepository/e1d2.inf_amd64_y/e1d.inf":  nicINF,
		"Windows/System32/DriverStore/FileRepository/rt.inf_amd64_z/rt.inf":     realtek,
		"Windows/System32/DriverStore/FileRepository/disp.inf_amd64_w/disp.inf": strings.Replace(nicINF, "Class=Net", "Class=Display", 1),
	})
	_, ids := inspectMountsReport([]mountedFS{{Root: root, FSType: "ntfs"}}, domain.OSTypeWindows)
	if strings.Join(ids, ",") != "10EC:8125,8086:15B8" {
		t.Fatalf("nic pci ids = %v", ids)
	}
}
