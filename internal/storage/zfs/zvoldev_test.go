package zfs

import (
	"errors"
	"reflect"
	"testing"
)

func TestZdPartReMatchesWholeDiskAndPartitions(t *testing.T) {
	// 数据集名在整盘节点上，分区须折回整盘；短 minor 不能匹配长 minor 的前缀。
	for _, tc := range []struct{ in, want string }{
		{"/dev/zd2800", "/dev/zd2800"},
		{"/dev/zd2800p3", "/dev/zd2800"},
		{"/dev/zd0", "/dev/zd0"},
		{"/dev/sda1", ""},
		{"/dev/zvol/tank/CLIENT-AABBCCDDEEFF", ""},
		{"tmpfs", ""},
	} {
		m := zdPartRe.FindStringSubmatch(tc.in)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != tc.want {
			t.Fatalf("zdPartRe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseZvolMountsFoldsPartitionsToDatasets(t *testing.T) {
	// 分区挂载归到其整盘节点的数据集；非 zd 来源和内核不认的节点跳过。
	mounts := []byte(`tmpfs /run tmpfs rw 0 0
/dev/zd2800p3 /tmp/ndiskless-vol-x ntfs3 rw,force 0 0
/dev/zd64 /mnt/whole ext4 ro 0 0
/dev/sda1 /boot ext4 rw 0 0
/dev/zd96p1 /tmp/ndiskless-vol-y vfat ro 0 0
malformed-line
`)
	nameOf := func(node string) (string, error) {
		switch node {
		case "/dev/zd2800":
			return "tank/CLIENT-AABBCCDDEEFF", nil
		case "/dev/zd64":
			return "tank/SCLIENT-001122334455", nil
		case "/dev/zd96":
			// 例如在读 /proc 与 ioctl 之间被销毁。
			return "", errors.New("no such device")
		}
		t.Fatalf("unexpected nameOf(%q)", node)
		return "", nil
	}

	got := parseZvolMounts(mounts, nameOf)

	want := []ZvolMount{
		{Device: "/dev/zd2800p3", Mountpoint: "/tmp/ndiskless-vol-x", Dataset: "tank/CLIENT-AABBCCDDEEFF"},
		{Device: "/dev/zd64", Mountpoint: "/mnt/whole", Dataset: "tank/SCLIENT-001122334455"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseZvolMounts = %#v, want %#v", got, want)
	}
}

func TestParseZvolMountsEmptyAndGarbage(t *testing.T) {
	nameOf := func(string) (string, error) {
		t.Fatal("nameOf must not be called")
		return "", nil
	}
	if got := parseZvolMounts(nil, nameOf); got != nil {
		t.Fatalf("nil input: %#v", got)
	}
	if got := parseZvolMounts([]byte("proc /proc proc rw 0 0\n"), nameOf); got != nil {
		t.Fatalf("no zvol lines: %#v", got)
	}
}
