package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

const ubuntuGrubCfg = `menuentry 'Ubuntu' {
	linux	/boot/vmlinuz-6.8.0-142-generic root=UUID=6dcd ro  quiet splash
	initrd	/boot/initrd.img-6.8.0-142-generic
}
submenu 'Advanced' {
	menuentry 'Ubuntu, recovery' {
		linux	/boot/vmlinuz-6.8.0-142-generic root=UUID=6dcd ro recovery nomodeset
	}
}
`

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ubuntuRoot(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"etc/os-release":        "NAME=Ubuntu\n",
		"etc/default/grub":      "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nGRUB_CMDLINE_LINUX=\"\"\n",
		"boot/grub/grub.cfg":    ubuntuGrubCfg,
		"etc/iscsi/iscsid.conf": "",
	})
	return root
}

func TestAdaptLinuxRootMakesTheInitramfsLogInAndMountsDataDisks(t *testing.T) {
	root := ubuntuRoot(t)
	if err := adaptLinuxRoot(root, []byte("#!/bin/sh\necho hi\n")); err != nil {
		t.Fatal(err)
	}
	cfg := readFile(t, root, "boot/grub/grub.cfg")
	if strings.Count(cfg, " iscsi_auto") != 2 || !strings.Contains(cfg, "ro  quiet splash iscsi_auto\n") {
		t.Fatalf("grub.cfg:\n%s", cfg)
	}
	if !strings.Contains(readFile(t, root, "etc/default/grub"), "GRUB_CMDLINE_LINUX=\"iscsi_auto\"\n") {
		t.Fatalf("default/grub:\n%s", readFile(t, root, "etc/default/grub"))
	}
	if readFile(t, root, "etc/iscsi/iscsi.initramfs") != "ISCSI_AUTO=true\n" {
		t.Fatal("iscsi.initramfs")
	}
	if readFile(t, root, storage.LinuxMountScriptPath) != "#!/bin/sh\necho hi\n" {
		t.Fatal("script not written")
	}
	if info, _ := os.Stat(filepath.Join(root, storage.LinuxMountScriptPath)); info.Mode()&0o111 == 0 {
		t.Fatal("script not executable")
	}
	if readFile(t, root, storage.LinuxMountUnitPath) != storage.LinuxMountUnitBody {
		t.Fatal("unit not written")
	}
	link, err := os.Readlink(filepath.Join(root, storage.LinuxMountUnitWantsPath))
	if err != nil || link != "/"+storage.LinuxMountUnitPath {
		t.Fatalf("unit not enabled: %q %v", link, err)
	}
}

func TestAdaptLinuxRootTwiceChangesNothing(t *testing.T) {
	root := ubuntuRoot(t)
	for i := 0; i < 2; i++ {
		if err := adaptLinuxRoot(root, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(readFile(t, root, "boot/grub/grub.cfg"), "iscsi_auto"); n != 2 {
		t.Fatalf("iscsi_auto appears %d times", n)
	}
	if !strings.Contains(readFile(t, root, "etc/default/grub"), "GRUB_CMDLINE_LINUX=\"iscsi_auto\"\n") {
		t.Fatal("default/grub changed on the second run")
	}
}

func TestAdaptLinuxRootKeepsExistingKernelParameters(t *testing.T) {
	root := ubuntuRoot(t)
	writeTree(t, root, map[string]string{"etc/default/grub": "GRUB_CMDLINE_LINUX=\"console=ttyS0\"\n"})
	if err := adaptLinuxRoot(root, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "etc/default/grub"); got != "GRUB_CMDLINE_LINUX=\"console=ttyS0 iscsi_auto\"\n" {
		t.Fatalf("default/grub = %q", got)
	}
	writeTree(t, root, map[string]string{"etc/default/grub": "GRUB_DEFAULT=0\n"})
	if err := adaptLinuxRoot(root, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "etc/default/grub"); got != "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"iscsi_auto\"\n" {
		t.Fatalf("default/grub = %q", got)
	}
}

func TestAdaptLinuxRootRefusesARootWithoutGrubConfig(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"etc/os-release": "NAME=Ubuntu\n", "boot/.keep": ""})
	if err := adaptLinuxRoot(root, []byte("x")); err == nil {
		t.Fatal("adapted a root whose /boot is elsewhere")
	}
	if _, err := os.Stat(filepath.Join(root, storage.LinuxMountScriptPath)); err == nil {
		t.Fatal("wrote files into a root it refused")
	}
}

// 没有分区表也没有文件系统的卷找不到根分区，应立即放弃，等满分区等待时间会让导入慢 3 分钟。
func TestWithLinuxRootGivesUpAtOnceOnABlankVolume(t *testing.T) {
	r := &fakeRunner{lsblkOutput: `{"blockdevices":[{"path":"/dev/zvol/tank/nd/probe","fstype":null}]}`}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := withLinuxRoot(ctx, r, "/dev/zvol/tank/nd/probe", 3*time.Minute, func(string) error { return nil })
	if !errors.Is(err, errNoLinuxRoot) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("waited %s for a volume that has nothing on it", took)
	}
}

// 镜像所有客户机共用，网卡上写死的地址会让每台抢同一个 IP；地址由 DHCP 保留按 MAC 发。
const staticNetplan = `network:
  version: 2
  ethernets:
    ens33:
      addresses:
      - "192.168.10.5/24"
      nameservers:
        addresses:
        - 114.114.114.114
      routes:
      - to: "default"
        via: "192.168.10.2"
`

const dhcpNetplan = `network:
  version: 2
  ethernets:
    ens33:
      dhcp4: true
      nameservers:
        addresses: [114.114.114.114]
`

func TestAdaptLinuxRootTurnsAStaticAddressIntoDHCP(t *testing.T) {
	root := ubuntuRoot(t)
	writeTree(t, root, map[string]string{
		"etc/netplan/50-cloud-init.yaml":                           staticNetplan,
		"etc/NetworkManager/system-connections/Wired.nmconnection": "[connection]\nid=Wired\n\n[ipv4]\naddress1=192.168.10.5/24\nmethod=manual\n",
		"etc/NetworkManager/system-connections/Auto.nmconnection":  "[connection]\nid=Auto\n\n[ipv4]\nmethod=auto\n",
	})
	for i := 0; i < 2; i++ { // 幂等
		if err := adaptLinuxRoot(root, []byte("#!/bin/sh\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc/netplan/50-cloud-init.yaml")); !os.IsNotExist(err) {
		t.Fatal("static netplan still active")
	}
	if readFile(t, root, "etc/netplan/50-cloud-init.yaml.nd-bak") != staticNetplan {
		t.Fatal("original netplan not kept as .nd-bak")
	}
	dhcp := readFile(t, root, "etc/netplan/01-ndiskless-dhcp.yaml")
	if !strings.Contains(dhcp, `name: "en*"`) || !strings.Contains(dhcp, "dhcp4: true") {
		t.Fatalf("dhcp netplan:\n%s", dhcp)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/NetworkManager/system-connections/Wired.nmconnection.nd-bak")); err != nil {
		t.Fatal("static NetworkManager profile not set aside")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/NetworkManager/system-connections/Auto.nmconnection")); err != nil {
		t.Fatal("DHCP NetworkManager profile was moved")
	}
}

func TestAdaptLinuxRootLeavesADHCPImageAlone(t *testing.T) {
	root := ubuntuRoot(t)
	writeTree(t, root, map[string]string{"etc/netplan/50-cloud-init.yaml": dhcpNetplan})
	if err := adaptLinuxRoot(root, []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, root, "etc/netplan/50-cloud-init.yaml") != dhcpNetplan {
		t.Fatal("DHCP netplan was changed")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/netplan/01-ndiskless-dhcp.yaml")); !os.IsNotExist(err) {
		t.Fatal("wrote a DHCP netplan into an image that already uses DHCP")
	}
}

func TestNetplanStaticAddressDetection(t *testing.T) {
	for src, want := range map[string]bool{
		staticNetplan: true,
		dhcpNetplan:   false,
		"network:\n  ethernets:\n    eth0:\n      addresses: [10.0.0.9/24]\n": true,
		"network:\n  ethernets:\n    eth0:\n      dhcp4: yes\n":               false,
	} {
		if got := netplanHasStaticAddress(src); got != want {
			t.Errorf("netplanHasStaticAddress(%q) = %v, want %v", src, got, want)
		}
	}
}
