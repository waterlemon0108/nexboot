package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

// lvmRunner 伪造 lsblk/blkid/lvs/pvs/dmsetup，mount 时按设备展开文件树
// （按后缀匹配，因为 mapper 名带随机部分）。
type lvmRunner struct {
	calls       [][]string
	lsblk       string
	lsblkSeq    []string // 依次先于 lsblkOutput 返回，模拟晚出现的分区
	lvs         string
	pvs         string
	trees       map[string]map[string]string
	tables      map[string]string
	removed     []string
	onUmount    func(dir string)
	removeFails int
}

func (r *lvmRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "lsblk":
		if len(r.lsblkSeq) > 0 {
			out := r.lsblkSeq[0]
			r.lsblkSeq = r.lsblkSeq[1:]
			return []byte(out), nil
		}
		return []byte(r.lsblk), nil
	case "blkid":
		if strings.HasPrefix(args[len(args)-1], "/dev/mapper/") {
			return []byte("ext4\n"), nil
		}
		return []byte("gpt\n"), nil
	case "lvs":
		return []byte(r.lvs), nil
	case "pvs":
		return []byte(r.pvs), nil
	case "dmsetup":
		switch args[0] {
		case "create":
			b, err := os.ReadFile(args[len(args)-1])
			if err != nil {
				return nil, err
			}
			if r.tables == nil {
				r.tables = map[string]string{}
			}
			r.tables[args[len(args)-2]] = string(b)
		case "remove":
			if r.removeFails > 0 && !strings.Contains(strings.Join(args, " "), "--deferred") {
				r.removeFails--
				return []byte("Device or resource busy"), errors.New("exit status 1")
			}
			r.removed = append(r.removed, strings.Join(args[1:], " "))
		}
		return nil, nil
	case "mount":
		part, dir := args[len(args)-2], args[len(args)-1]
		for dev, tree := range r.trees {
			if !strings.HasSuffix(part, dev) {
				continue
			}
			for rel, content := range tree {
				path := filepath.Join(dir, filepath.FromSlash(rel))
				_ = os.MkdirAll(filepath.Dir(path), 0o755)
				_ = os.WriteFile(path, []byte(content), 0o644)
			}
		}
		return nil, nil
	case "umount":
		dir := args[len(args)-1]
		if r.onUmount != nil {
			r.onUmount(dir)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil, nil
	}
	return nil, nil
}

func (r *lvmRunner) ran(name string) bool {
	for _, c := range r.calls {
		if c[0] == name {
			return true
		}
	}
	return false
}

const lvmLvs = "  ubuntu-vg|ubuntu-lv|0|20971520|linear|/dev/zd8p3:0-2559|8192\n" +
	"  ubuntu-vg|ubuntu-lv|20971520|8192|linear|/dev/zd8p3:3000-3000|8192\n"
const lvmPvs = "  /dev/zd8p3|2048\n"

// 只读出布局、自己建映射：不激活卷组、不改元数据，服务器自己的同名卷组也碰不到。
func TestMapLVMBuildsALinearTableWithoutTouchingMetadata(t *testing.T) {
	r := &lvmRunner{lvs: lvmLvs, pvs: lvmPvs}
	parts, release, err := mapLVM(context.Background(), r, []inspectPartition{{Path: "/dev/zd8p3", FSType: "LVM2_member"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || !strings.HasPrefix(parts[0].Path, "/dev/mapper/nd-") || parts[0].FSType != "ext4" {
		t.Fatalf("parts = %#v", parts)
	}
	name := strings.TrimPrefix(parts[0].Path, "/dev/mapper/")
	want := "0 20971520 linear /dev/zd8p3 2048\n20971520 8192 linear /dev/zd8p3 24578048\n"
	if r.tables[name] != want {
		t.Fatalf("table =\n%s\nwant\n%s", r.tables[name], want)
	}
	for _, c := range r.calls {
		if c[0] == "vgchange" || c[0] == "vgrename" || c[0] == "lvchange" {
			t.Fatalf("touched LVM metadata: %v", c)
		}
		if c[0] == "lvs" && !strings.Contains(strings.Join(c, " "), `a|^/dev/zd8p3$|`) {
			t.Fatalf("lvs not confined to this disk: %v", c)
		}
	}
	release()
	if len(r.removed) != 1 || r.removed[0] != "--retry "+name {
		t.Fatalf("removed = %v", r.removed)
	}
}

// lvs 和告警共用一个输出（CombinedOutput）：一行 WARNING 不能让整盘读不出来。
func TestMapLVMSkipsWarningLines(t *testing.T) {
	r := &lvmRunner{lvs: "  WARNING: lvmetad is not running.\n" + lvmLvs, pvs: "  WARNING: PV header is old.\n" + lvmPvs}
	parts, release, err := mapLVM(context.Background(), r, []inspectPartition{{Path: "/dev/zd8p3", FSType: "LVM2_member"}})
	defer release()
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts = %v err = %v", parts, err)
	}
}

// 映射仍被占用（懒卸载后还有人用）时 remove 会失败，须改为延迟删除，由内核在最后一个使用者离开时删，克隆盘才能销毁。
func TestMapLVMDefersARemoveThatIsBusy(t *testing.T) {
	r := &lvmRunner{lvs: lvmLvs, pvs: lvmPvs, removeFails: 1}
	parts, release, err := mapLVM(context.Background(), r, []inspectPartition{{Path: "/dev/zd8p3", FSType: "LVM2_member"}})
	if err != nil {
		t.Fatal(err)
	}
	release()
	name := strings.TrimPrefix(parts[0].Path, "/dev/mapper/")
	if len(r.removed) != 1 || r.removed[0] != "--deferred "+name {
		t.Fatalf("removed = %v", r.removed)
	}
}

func TestMapLVMRefusesThinVolumesByName(t *testing.T) {
	r := &lvmRunner{lvs: "  vg|pool|0|2048|thin-pool|/dev/zd8p3:0-0|8192\n", pvs: lvmPvs}
	_, release, err := mapLVM(context.Background(), r, []inspectPartition{{Path: "/dev/zd8p3", FSType: "LVM2_member"}})
	release()
	if err == nil || !strings.Contains(err.Error(), "thin-pool") || r.ran("dmsetup") {
		t.Fatalf("err = %v, dmsetup ran = %v", err, r.ran("dmsetup"))
	}
}

func TestMapLVMDoesNothingWithoutLVM(t *testing.T) {
	r := &lvmRunner{}
	parts, release, err := mapLVM(context.Background(), r, []inspectPartition{{Path: "/dev/zd8p2", FSType: "ext4"}})
	release()
	if err != nil || len(parts) != 0 || len(r.calls) != 0 {
		t.Fatalf("parts=%v err=%v calls=%v", parts, err, r.calls)
	}
}

const lvmDiskLsblk = `{"blockdevices":[{"path":"/dev/zd8","fstype":null,"children":[
	{"path":"/dev/zd8p1","fstype":null},
	{"path":"/dev/zd8p2","fstype":"ext4"},
	{"path":"/dev/zd8p3","fstype":"LVM2_member"},
	{"path":"/dev/zd8p4","fstype":"xfs"}]}]}`

// udev 逐个探测分区：/boot 先出现、PV 还没出现时就开始找，根就找不到。
const lvmDiskBootOnlyLsblk = `{"blockdevices":[{"path":"/dev/zd8","fstype":null,"children":[
	{"path":"/dev/zd8p1","fstype":null},
	{"path":"/dev/zd8p2","fstype":"ext4"}]}]}`

// Ubuntu Server 选 LVM 时，根在逻辑卷里，/boot 是单独的 ext4 分区。
func TestWithLinuxRootFindsARootOnLVMAndItsSeparateBoot(t *testing.T) {
	bootSurvived := false
	r := &lvmRunner{lsblk: lvmDiskLsblk, lsblkSeq: []string{lvmDiskBootOnlyLsblk, lvmDiskBootOnlyLsblk}, lvs: lvmLvs, pvs: lvmPvs, trees: map[string]map[string]string{
		"/dev/zd8p2": {"grub/grub.cfg": "linux /vmlinuz root=/dev/mapper/ubuntu--vg-ubuntu--lv ro\n"},
		"-ubuntu-lv": {"etc/os-release": "NAME=Ubuntu\n", "boot/.keep": ""},
	}}
	r.onUmount = func(dir string) {
		if _, err := os.Stat(filepath.Join(dir, "etc", "os-release")); err == nil {
			_, err := os.Stat(filepath.Join(dir, "boot"))
			bootSurvived = err == nil
		}
	}
	err := withLinuxRoot(context.Background(), r, "/dev/zd8", importPartitionWait, func(root string) error {
		if _, err := os.Stat(filepath.Join(root, "boot", "grub", "grub.cfg")); err != nil {
			t.Fatalf("separate /boot not mounted under the root: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bootSurvived {
		t.Fatal("unmounting /boot removed the image's /boot directory")
	}
	if len(r.removed) != 1 {
		t.Fatalf("mapping not removed: %v", r.removed)
	}
	// 探测 /boot 时只读挂载，数据分区绝不写入。
	for _, c := range r.calls {
		if c[0] == "mount" && c[len(c)-2] == "/dev/zd8p4" && strings.Contains(strings.Join(c, " "), "rw") {
			t.Fatalf("data partition mounted read-write while probing: %v", c)
		}
	}
}

func TestInspectFindsGrubOnASeparateBootPartition(t *testing.T) {
	root, boot := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string]string{"etc/os-release": "NAME=Ubuntu\n", "usr/sbin/iscsistart": ""})
	writeTree(t, boot, map[string]string{"grub/grub.cfg": "linux /vmlinuz ro iscsi_auto\n"})
	items := inspectLinux([]mountedFS{{Root: boot, FSType: "ext4"}, {Root: root, FSType: "ext4"}})
	for _, it := range items {
		if it.Name == "iscsi_boot" && it.Level != domain.HealthOK {
			t.Fatalf("iscsi_boot = %#v", it)
		}
		if it.Level == domain.HealthBlock {
			t.Fatalf("blocked: %#v", it)
		}
	}
}

func TestEnsureZvolLVMFilter(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "lvm.conf")
	orig := "config {\n}\ndevices {\n\tdir = \"/dev\"\n\t# global_filter = [ \"a|.*|\" ]\n}\n"
	if err := os.WriteFile(conf, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureZvolLVMFilter(conf)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), "devices {\n\t# ndiskless") || !strings.Contains(string(b), "\tglobal_filter = [ \"r|^/dev/zd.*|\" ]\n") {
		t.Fatalf("lvm.conf:\n%s", b)
	}
	if changed, err := EnsureZvolLVMFilter(conf); err != nil || changed {
		t.Fatalf("second run changed=%v err=%v", changed, err)
	}

	custom := filepath.Join(dir, "custom.conf")
	_ = os.WriteFile(custom, []byte("devices {\n\tglobal_filter = [ \"r|/dev/sdz|\" ]\n}\n"), 0o644)
	if changed, err := EnsureZvolLVMFilter(custom); changed || !errors.Is(err, errLVMFilterCustomized) {
		t.Fatalf("customized: changed=%v err=%v", changed, err)
	}
	if changed, err := EnsureZvolLVMFilter(filepath.Join(dir, "missing.conf")); changed || err != nil {
		t.Fatalf("missing: changed=%v err=%v", changed, err)
	}
}

// 根不在（btrfs、加密）时多试一会儿就放弃，不能把导入预算（3 分钟）全等完。
func TestWithLinuxRootGivesUpSoonWithoutARoot(t *testing.T) {
	defer func(b time.Duration) { rootRetryBudget = b }(rootRetryBudget)
	rootRetryBudget = 2 * time.Second
	r := &lvmRunner{lsblk: lvmDiskBootOnlyLsblk, trees: map[string]map[string]string{"/dev/zd8p2": {"grub/grub.cfg": "x"}}}
	start := time.Now()
	err := withLinuxRoot(context.Background(), r, "/dev/zd8", importPartitionWait, func(string) error { return nil })
	if !errors.Is(err, errNoLinuxRoot) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > rootRetryBudget+5*time.Second {
		t.Fatalf("took %s to give up", took)
	}
}

// 不支持的卷（thin、跨盘）重试也不会变：直接报。
func TestWithLinuxRootReportsAnUnsupportedVolumeAtOnce(t *testing.T) {
	r := &lvmRunner{lsblk: lvmDiskLsblk, lvs: "  vg|pool|0|2048|thin-pool|/dev/zd8p3:0-0|8192\n", pvs: lvmPvs}
	start := time.Now()
	err := withLinuxRoot(context.Background(), r, "/dev/zd8", importPartitionWait, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "thin-pool") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("retried a deterministic failure for %s", took)
	}
}
