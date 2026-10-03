package zfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

func TestCloneCommand(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.Clone(context.Background(), "tank/cfg@red", "tank/CLIENT-AABB"); err != nil {
		t.Fatal(err)
	}
	want := []string{"clone", "tank/cfg@red", "tank/CLIENT-AABB"}
	if !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, want)
	}
}

func TestCommandError(t *testing.T) {
	runner := &fakeRunner{err: errors.New("boom"), out: []byte("no zfs")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := client.Destroy(context.Background(), "tank/CLIENT-AABB")
	if !IsCommandError(err) {
		t.Fatalf("err = %v, want CommandError", err)
	}
}

func TestListDatasetsParsesNames(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank/CLIENT-AABB\n\ntank/CLIENT-AABB-DATA-1\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := client.ListDatasets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/CLIENT-AABB", "tank/CLIENT-AABB-DATA-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("datasets = %#v, want %#v", got, want)
	}
	wantArgs := []string{"list", "-H", "-o", "name"}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, wantArgs)
	}
}

func TestListDependentClonesParsesOrigins(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank/CLIENT-AABB\ttank/cfg-win@0\ntank/CLIENT-BBCC\ttank/other@0\ntank/CLIENT-AABB-DATA-1\ttank/cfg-win@daily\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := client.ListDependentClones(context.Background(), "tank/cfg-win")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/CLIENT-AABB", "tank/CLIENT-AABB-DATA-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clones = %#v, want %#v", got, want)
	}
	wantArgs := []string{"list", "-H", "-t", "volume", "-o", "name,origin"}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, wantArgs)
	}
}

func TestCreateVolumeCommand(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 100 字节按 16K 块向上取整为一个完整的 16384 字节块。
	if err := client.CreateVolume(context.Background(), "tank/win11", 100, 16384); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "-s", "-o", "volblocksize=16384", "-V", "16384", "tank/win11"}
	if !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, want)
	}
}

func TestCreateVolumeRejectsNonPositiveSize(t *testing.T) {
	client := New("tank", &fakeRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.CreateVolume(context.Background(), "tank/x", 0, 16384); err == nil {
		t.Fatal("expected error for non-positive size")
	}
}

// 池布局只由建池命令行的 vdev 语法决定，所以 argv 就是全部功能：镜像、条带各按布局拼接，
// 并统一带上 ashift=12 和 lz4。
func TestCreatePoolBuildsVdevGrammarPerLayout(t *testing.T) {
	cases := []struct {
		name string
		spec storage.PoolSpec
		want []string
	}{
		{"stripe", storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []string{"/dev/sdb", "/dev/sdc"}},
			[]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "/dev/sdb", "/dev/sdc"}},
		{"mirror-2", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{"/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde"}},
			[]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "mirror", "/dev/sdb", "/dev/sdc", "mirror", "/dev/sdd", "/dev/sde"}},
		{"mirror-3", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 3, Disks: []string{"/dev/sdb", "/dev/sdc", "/dev/sdd"}},
			[]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "mirror", "/dev/sdb", "/dev/sdc", "/dev/sdd"}},
		{"raidz2", storage.PoolSpec{Layout: domain.PoolLayoutRaidz2, GroupWidth: 6, Disks: []string{"/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf", "/dev/sdg"}},
			[]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "raidz2", "/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf", "/dev/sdg"}},
		{"raidz1-two-groups", storage.PoolSpec{Layout: domain.PoolLayoutRaidz1, GroupWidth: 3, Disks: []string{"a", "b", "c", "d", "e", "f"}},
			[]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "raidz1", "a", "b", "c", "raidz1", "d", "e", "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := client.CreatePool(context.Background(), "tank2", tc.spec); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runner.calls, []fakeCall{{name: "zpool", args: tc.want}}) {
				t.Fatalf("calls = %#v, want %#v", runner.calls, tc.want)
			}
		})
	}
}

// 宽度不能整除盘数时最后一组会悄悄变窄（镜像池里混一块无镜像的盘），此处也必须拒绝。
func TestCreatePoolRefusesRaggedGroups(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := client.CreatePool(context.Background(), "tank2", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{"a", "b", "c"}})
	if err == nil || len(runner.calls) != 0 {
		t.Fatalf("err = %v, calls = %#v", err, runner.calls)
	}
}

func TestCreateAndDestroyPoolCommands(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.CreatePool(context.Background(), "tank2", storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []string{"/dev/sdb", "/dev/sdc"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.DestroyPool(context.Background(), "tank2"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "/dev/sdb", "/dev/sdc"}},
		{name: "zpool", args: []string{"destroy", "tank2"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

// Capacity 取 ZFS 自己的 used + available，而非含 slop 预留和 raidz 校验的 `zpool list size`；
// 否则页面显示还剩 5%，写入却已 `out of space`。
func TestPoolStatusParsesZPoolList(t *testing.T) {
	runner := &sequenceRunner{outs: [][]byte{
		[]byte("tank\tONLINE\n"),
		[]byte("268435456\t805306368\n"),
		[]byte(""),
	}}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := client.PoolStatus(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "tank" || status.Capacity != 1073741824 || status.Used != 268435456 || status.Health != "ONLINE" {
		t.Fatalf("status = %#v", status)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"list", "-Hp", "-o", "name,health", "tank"}},
		{name: "zfs", args: []string{"list", "-Hp", "-o", "used,available", "tank"}},
		{name: "zpool", args: []string{"status", "-P", "tank"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

// 一次列表同时回答池有多满、各镜像实占多少；文件系统（池根、导入目录）不是卷，不进 map。
func TestSpaceUsageParsesPoolAndVolumes(t *testing.T) {
	runner := &fakeRunner{out: []byte("" +
		"tank\tfilesystem\t-\t39670943744\t167359901696\n" +
		"tank/imports\tfilesystem\t-\t30000000000\t167359901696\n" +
		"tank/win11\tvolume\t85899345920\t9376923648\t167359901696\n" +
		"tank/win11_default\tvolume\t85899345920\t8192\t167359901696\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	usage, err := client.SpaceUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.PoolUsed != 39670943744 || usage.PoolAvailable != 167359901696 {
		t.Fatalf("pool usage = %#v", usage)
	}
	wantVolumes := map[string]VolumeUsage{
		"tank/win11":         {Size: 85899345920, Used: 9376923648},
		"tank/win11_default": {Size: 85899345920, Used: 8192},
	}
	if !reflect.DeepEqual(usage.Volumes, wantVolumes) {
		t.Fatalf("volumes = %#v, want %#v", usage.Volumes, wantVolumes)
	}
	want := fakeCall{name: "zfs", args: []string{"list", "-Hp", "-r", "-t", "filesystem,volume", "-o", "name,type,volsize,used,available", "tank"}}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call = %#v, want %#v", runner.calls[0], want)
	}
}

// 刚建的卷可能在大小属性就绪前就被列出，坏行不能拖垮整个列表（和镜像页）。
func TestSpaceUsageSkipsMalformedRows(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank\tfilesystem\t-\t100\t900\ntank/x\tvolume\t-\t-\t-\ntank/y\tvolume\t50\t10\t900\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	usage, err := client.SpaceUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.PoolUsed != 100 || usage.PoolAvailable != 900 || len(usage.Volumes) != 1 || usage.Volumes["tank/y"] != (VolumeUsage{Size: 50, Used: 10}) {
		t.Fatalf("usage = %#v", usage)
	}
}

// 接收 zfs 流的 zvol 大小由流决定，导入后读回 volsize 是此类镜像获得逻辑大小的唯一途径。
func TestVolumeSizeReadsVolsize(t *testing.T) {
	runner := &fakeRunner{out: []byte("85899345920\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	size, err := client.VolumeSize(context.Background(), "tank/win11")
	if err != nil {
		t.Fatal(err)
	}
	if size != 85899345920 {
		t.Fatalf("size = %d", size)
	}
	want := fakeCall{name: "zfs", args: []string{"get", "-Hp", "-o", "value", "volsize", "tank/win11"}}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call = %#v, want %#v", runner.calls[0], want)
	}
}

func TestPoolDiskOperationsCallzpoolSubcommands(t *testing.T) {
	runner := &sequenceRunner{
		outs: [][]byte{
			nil,
		},
	}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.AddDisk(context.Background(), "tank", storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: []string{"/dev/sdb", "/dev/sdc"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveDisk(context.Background(), "tank", "/dev/sdc"); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceDisk(context.Background(), "tank", "/dev/sdc", "/dev/sdd"); err != nil {
		t.Fatal(err)
	}
	if err := client.AddReadCache(context.Background(), "tank", []string{"/dev/nvme0n1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveReadCache(context.Background(), "tank", "/dev/nvme0n1"); err != nil {
		t.Fatal(err)
	}
	if err := client.AddWriteCache(context.Background(), "tank", []string{"/dev/nvme1n1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveWriteCache(context.Background(), "tank", "/dev/nvme1n1"); err != nil {
		t.Fatal(err)
	}
	if err := client.FlushWriteCache(context.Background(), "tank"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"add", "tank", "/dev/sdb", "/dev/sdc"}},
		{name: "zpool", args: []string{"remove", "tank", "/dev/sdc"}},
		{name: "zpool", args: []string{"replace", "tank", "/dev/sdc", "/dev/sdd"}},
		{name: "zpool", args: []string{"add", "tank", "cache", "/dev/nvme0n1"}},
		{name: "zpool", args: []string{"remove", "tank", "/dev/nvme0n1"}},
		{name: "zpool", args: []string{"add", "tank", "log", "/dev/nvme1n1"}},
		{name: "zpool", args: []string{"remove", "tank", "/dev/nvme1n1"}},
		{name: "zpool", args: []string{"sync", "tank"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestPoolReadCacheOperationsCallzpoolSubcommands(t *testing.T) {
	runner := &sequenceRunner{
		outs: [][]byte{
			nil,
		},
	}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.AddReadCache(context.Background(), "tank", []string{"/dev/sdc", "/dev/sdd"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveReadCache(context.Background(), "tank", "/dev/sdc"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"add", "tank", "cache", "/dev/sdc", "/dev/sdd"}},
		{name: "zpool", args: []string{"remove", "tank", "/dev/sdc"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestPoolStatusParsesDisks(t *testing.T) {
	runner := &sequenceRunner{
		outs: [][]byte{
			[]byte("tank\tONLINE\n"),
			[]byte("268435456\t805306368\n"),
			[]byte(`
pool: tank
  scan: resilver in progress since Mon Jun 29 12:00:00 2026
        512M scanned, 128M issued, 12.5% done
  STATE: ONLINE
NAME        STATE     READ WRITE CKSUM
tank        ONLINE      -      -     -
/dev/sdb    ONLINE      -      -     -
/dev/sdc    DEGRADED    -      -     -
`),
		},
	}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := client.PoolStatus(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "tank" || status.Health != "ONLINE" || status.Operation != "resilver" || status.Progress != "12.5%" || status.Capacity != 1073741824 || status.Used != 268435456 {
		t.Fatalf("status = %#v", status)
	}
	if len(status.Disks) != 2 || status.Disks[0].Path != "/dev/sdb" || status.Disks[0].Role != domain.PoolDiskRoleData || status.Disks[0].Status != "ONLINE" {
		t.Fatalf("status disks = %#v", status.Disks)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"list", "-Hp", "-o", "name,health", "tank"}},
		{name: "zfs", args: []string{"list", "-Hp", "-o", "used,available", "tank"}},
		{name: "zpool", args: []string{"status", "-P", "tank"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestPoolStatusParsesReadCacheDisks(t *testing.T) {
	runner := &sequenceRunner{
		outs: [][]byte{
			[]byte("tank\tONLINE\n"),
			[]byte("268435456\t805306368\n"),
			[]byte(`
pool: tank
  scan: resilver in progress since Mon Jun 29 12:00:00 2026
        512M scanned, 128M issued, 12.5% done
  STATE: ONLINE
NAME        STATE     READ WRITE CKSUM
tank        ONLINE      -      -     -
/dev/sdb    ONLINE      -      -     -
cache
/dev/sdc    ONLINE      -      -     -
logs
/dev/nvme1n1 ONLINE     -      -     -
`),
		},
	}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := client.PoolStatus(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Disks) != 3 {
		t.Fatalf("status disks = %#v", status.Disks)
	}
	if status.Disks[0].Role != domain.PoolDiskRoleData || status.Disks[1].Role != domain.PoolDiskRoleReadCache || status.Disks[2].Role != domain.PoolDiskRoleWriteCache {
		t.Fatalf("status = %#v", status.Disks)
	}
}

// 测试机上采集的真实 `zpool status -P` 输出（文件 vdev，专门建好再销毁）。
// zpool 的输出才是布局的真相，库里只是缓存，所以解析器按原文校验。
const statusMirrorSpecialSpare = `  pool: zt1
 state: ONLINE
config:

	NAME            STATE     READ WRITE CKSUM
	zt1             ONLINE       0     0     0
	  mirror-0      ONLINE       0     0     0
	    /tmp/zt/f1  ONLINE       0     0     0
	    /tmp/zt/f2  ONLINE       0     0     0
	  mirror-1      ONLINE       0     0     0
	    /tmp/zt/f3  ONLINE       0     0     0
	    /tmp/zt/f4  ONLINE       0     0     0
	  mirror-2      ONLINE       0     0     0
	    /tmp/zt/f5  ONLINE       0     0     0
	    /tmp/zt/f6  ONLINE       0     0     0
	special	
	  mirror-3      ONLINE       0     0     0
	    /tmp/zt/f7  ONLINE       0     0     0
	    /tmp/zt/f8  ONLINE       0     0     0
	spares
	  /tmp/zt/f9    AVAIL   

errors: No known data errors
`

const statusMirrorDegraded = `  pool: zt1
 state: DEGRADED
status: One or more devices has been taken offline by the administrator.
	Sufficient replicas exist for the pool to continue functioning in a
	degraded state.
action: Online the device using 'zpool online' or replace the device with
	'zpool replace'.
config:

	NAME            STATE     READ WRITE CKSUM
	zt1             DEGRADED     0     0     0
	  mirror-0      DEGRADED     0     0     0
	    /tmp/zt/f1  ONLINE       0     0     0
	    /tmp/zt/f2  OFFLINE      0     0     0
	  mirror-1      ONLINE       0     0     0
	    /tmp/zt/f3  ONLINE       0     0     0
	    /tmp/zt/f4  ONLINE       0     0     0

errors: No known data errors
`

const statusRaidz2 = `  pool: zt2
 state: ONLINE
config:

	NAME            STATE     READ WRITE CKSUM
	zt2             ONLINE       0     0     0
	  raidz2-0      ONLINE       0     0     0
	    /tmp/zt/f1  ONLINE       0     0     0
	    /tmp/zt/f2  ONLINE       0     0     0
	    /tmp/zt/f3  ONLINE       0     0     0
	    /tmp/zt/f4  ONLINE       0     0     0
	    /tmp/zt/f5  ONLINE       0     0     0

errors: No known data errors
`

const statusStripeCacheLog = `  pool: zt3
 state: ONLINE
config:

	NAME          STATE     READ WRITE CKSUM
	zt3           ONLINE       0     0     0
	  /tmp/zt/f1  ONLINE       0     0     0
	  /tmp/zt/f2  ONLINE       0     0     0
	logs	
	  /tmp/zt/f4  ONLINE       0     0     0
	cache
	  /tmp/zt/f3  ONLINE       0     0     0

errors: No known data errors
`

// 一组镜像加一块裸盘（zpool 需 -f 才建）。数据 vdev 中有裸盘即无冗余可言，应读作条带。
const statusMixed = `  pool: zt4
 state: ONLINE
config:

	NAME            STATE     READ WRITE CKSUM
	zt4             ONLINE       0     0     0
	  mirror-0      ONLINE       0     0     0
	    /tmp/zt/f1  ONLINE       0     0     0
	    /tmp/zt/f2  ONLINE       0     0     0
	  /tmp/zt/f3    ONLINE       0     0     0

errors: No known data errors
`

// 组编号全池共享：这里的 log 镜像是 mirror-1，不能误判为第二个数据组。
const statusMirrorLogMirror = `  pool: zt5
 state: ONLINE
config:

	NAME            STATE     READ WRITE CKSUM
	zt5             ONLINE       0     0     0
	  mirror-0      ONLINE       0     0     0
	    /tmp/zt/f1  ONLINE       0     0     0
	    /tmp/zt/f2  ONLINE       0     0     0
	logs	
	  mirror-1      ONLINE       0     0     0
	    /tmp/zt/f3  ONLINE       0     0     0
	    /tmp/zt/f4  ONLINE       0     0     0

errors: No known data errors
`

func TestParsePoolDetailsGroupsAndLayout(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		pool       string
		layout     domain.PoolLayout
		width      int
		vdevs      []domain.PoolVdev
		dataDisks  []string
		roleOfDisk map[string]domain.PoolDiskRole
	}{
		{
			name: "mirror x3 + special mirror + spare", out: statusMirrorSpecialSpare, pool: "zt1",
			layout: domain.PoolLayoutMirror, width: 2,
			vdevs: []domain.PoolVdev{
				{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}, {Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}}},
				{Name: "mirror-1", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}, {Path: "/tmp/zt/f4", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}}},
				{Name: "mirror-2", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f5", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-2"}, {Path: "/tmp/zt/f6", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-2"}}},
				{Name: "mirror-3", Kind: "mirror", Role: domain.PoolDiskRoleSpecial, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f7", Role: domain.PoolDiskRoleSpecial, Status: "ONLINE", Vdev: "mirror-3"}, {Path: "/tmp/zt/f8", Role: domain.PoolDiskRoleSpecial, Status: "ONLINE", Vdev: "mirror-3"}}},
				{Name: "/tmp/zt/f9", Kind: "disk", Role: domain.PoolDiskRoleSpare, Status: "AVAIL", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f9", Role: domain.PoolDiskRoleSpare, Status: "AVAIL", Vdev: ""}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2", "/tmp/zt/f3", "/tmp/zt/f4", "/tmp/zt/f5", "/tmp/zt/f6"},
		},
		{
			name: "mirror degraded", out: statusMirrorDegraded, pool: "zt1",
			layout: domain.PoolLayoutMirror, width: 2,
			vdevs: []domain.PoolVdev{
				{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "DEGRADED", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}, {Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "OFFLINE", Vdev: "mirror-0"}}},
				{Name: "mirror-1", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}, {Path: "/tmp/zt/f4", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-1"}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2", "/tmp/zt/f3", "/tmp/zt/f4"},
		},
		{
			name: "raidz2", out: statusRaidz2, pool: "zt2",
			layout: domain.PoolLayoutRaidz2, width: 5,
			vdevs: []domain.PoolVdev{
				{Name: "raidz2-0", Kind: "raidz2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{
					{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"}, {Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"}, {Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"}, {Path: "/tmp/zt/f4", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"}, {Path: "/tmp/zt/f5", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "raidz2-0"}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2", "/tmp/zt/f3", "/tmp/zt/f4", "/tmp/zt/f5"},
		},
		{
			name: "stripe with cache and log", out: statusStripeCacheLog, pool: "zt3",
			layout: domain.PoolLayoutStripe, width: 1,
			vdevs: []domain.PoolVdev{
				{Name: "/tmp/zt/f1", Kind: "disk", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "/tmp/zt/f1"}}},
				{Name: "/tmp/zt/f2", Kind: "disk", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "/tmp/zt/f2"}}},
				{Name: "/tmp/zt/f4", Kind: "disk", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f4", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE", Vdev: "/tmp/zt/f4"}}},
				{Name: "/tmp/zt/f3", Kind: "disk", Role: domain.PoolDiskRoleReadCache, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleReadCache, Status: "ONLINE", Vdev: "/tmp/zt/f3"}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2"},
		},
		{
			name: "mixed mirror + bare disk reads as stripe", out: statusMixed, pool: "zt4",
			layout: domain.PoolLayoutStripe, width: 1,
			vdevs: []domain.PoolVdev{
				{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}, {Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}}},
				{Name: "/tmp/zt/f3", Kind: "disk", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "/tmp/zt/f3"}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2", "/tmp/zt/f3"},
		},
		{
			name: "mirror with a log mirror", out: statusMirrorLogMirror, pool: "zt5",
			layout: domain.PoolLayoutMirror, width: 2,
			vdevs: []domain.PoolVdev{
				{Name: "mirror-0", Kind: "mirror", Role: domain.PoolDiskRoleData, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f1", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}, {Path: "/tmp/zt/f2", Role: domain.PoolDiskRoleData, Status: "ONLINE", Vdev: "mirror-0"}}},
				{Name: "mirror-1", Kind: "mirror", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE", Disks: []domain.PoolDiskStatus{{Path: "/tmp/zt/f3", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE", Vdev: "mirror-1"}, {Path: "/tmp/zt/f4", Role: domain.PoolDiskRoleWriteCache, Status: "ONLINE", Vdev: "mirror-1"}}},
			},
			dataDisks: []string{"/tmp/zt/f1", "/tmp/zt/f2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := parsePoolDetails(tc.out, tc.pool)
			if d.Layout != tc.layout || d.GroupWidth != tc.width {
				t.Fatalf("layout = %q/%d, want %q/%d", d.Layout, d.GroupWidth, tc.layout, tc.width)
			}
			if !reflect.DeepEqual(d.Vdevs, tc.vdevs) {
				t.Fatalf("vdevs =\n%#v\nwant\n%#v", d.Vdevs, tc.vdevs)
			}
			// 现有调用方读的扁平磁盘列表是各组成员按序展开，每块盘带所属组。
			var flat []domain.PoolDiskStatus
			for _, v := range tc.vdevs {
				flat = append(flat, v.Disks...)
			}
			if !reflect.DeepEqual(d.Disks, flat) {
				t.Fatalf("disks =\n%#v\nwant\n%#v", d.Disks, flat)
			}
			var data []string
			for _, disk := range d.Disks {
				if disk.Role == domain.PoolDiskRoleData {
					data = append(data, disk.Path)
				}
			}
			if !reflect.DeepEqual(data, tc.dataDisks) {
				t.Fatalf("data disks = %v, want %v", data, tc.dataDisks)
			}
		})
	}
}

// 替换进行中，组内会嵌一个 `replacing-N` 伪 vdev；其成员归外层组，组类型不变。
func TestParsePoolDetailsSeesThroughReplacing(t *testing.T) {
	out := `  pool: t
 state: ONLINE
config:

	NAME              STATE     READ WRITE CKSUM
	t                 ONLINE       0     0     0
	  mirror-0        ONLINE       0     0     0
	    /dev/sda      ONLINE       0     0     0
	    replacing-1   ONLINE       0     0     0
	      /dev/sdb    ONLINE       0     0     0
	      /dev/sdc    ONLINE       0     0     0  (resilvering)

errors: No known data errors
`
	d := parsePoolDetails(out, "t")
	if d.Layout != domain.PoolLayoutMirror || len(d.Vdevs) != 1 || d.Vdevs[0].Kind != "mirror" || len(d.Vdevs[0].Disks) != 3 {
		t.Fatalf("details = %#v", d)
	}
	if d.Vdevs[0].Disks[2].Path != "/dev/sdc" || d.Vdevs[0].Disks[2].Vdev != "mirror-0" {
		t.Fatalf("nested member = %#v", d.Vdevs[0].Disks[2])
	}
}

// 镜像或 raidz 池加盘是按建池语法加整组；attach 是给现有盘再挂一块（裸盘变镜像，两路变三路）。
func TestAddDiskAndAttachBuildVdevGrammar(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.AddDisk(context.Background(), "tank", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{"/dev/sde", "/dev/sdf", "/dev/sdg", "/dev/sdh"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.AddDisk(context.Background(), "tank", storage.PoolSpec{Layout: domain.PoolLayoutRaidz2, GroupWidth: 4, Disks: []string{"a", "b", "c", "d"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.AttachDisk(context.Background(), "tank", "/dev/sda", "/dev/sdz"); err != nil {
		t.Fatal(err)
	}
	if err := client.DetachDisk(context.Background(), "tank", "/dev/sdz"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"add", "tank", "mirror", "/dev/sde", "/dev/sdf", "mirror", "/dev/sdg", "/dev/sdh"}},
		{name: "zpool", args: []string{"add", "tank", "raidz2", "a", "b", "c", "d"}},
		{name: "zpool", args: []string{"attach", "tank", "/dev/sda", "/dev/sdz"}},
		{name: "zpool", args: []string{"detach", "tank", "/dev/sdz"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
	if err := client.AddDisk(context.Background(), "tank", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{"a"}}); err == nil {
		t.Fatal("ragged mirror group accepted")
	}
}

// special vdev 存元数据，丢了整池就丢，所以只以镜像添加；热备盘按 spare 加并开启 autoreplace。
func TestSpecialAndSpareCommands(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.AddSpecial(context.Background(), "tank", []string{"/dev/nvme0n1", "/dev/nvme1n1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveSpecial(context.Background(), "tank", "mirror-3"); err != nil {
		t.Fatal(err)
	}
	if err := client.AddSpare(context.Background(), "tank", []string{"/dev/sdz"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveSpare(context.Background(), "tank", "/dev/sdz"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{
		{name: "zpool", args: []string{"add", "tank", "special", "mirror", "/dev/nvme0n1", "/dev/nvme1n1"}},
		{name: "zpool", args: []string{"remove", "tank", "mirror-3"}},
		{name: "zpool", args: []string{"add", "tank", "spare", "/dev/sdz"}},
		{name: "zpool", args: []string{"set", "autoreplace=on", "tank"}},
		{name: "zpool", args: []string{"remove", "tank", "/dev/sdz"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
	if err := client.AddSpecial(context.Background(), "tank", []string{"/dev/nvme0n1"}); err == nil {
		t.Fatal("unmirrored special accepted")
	}
}

// raidz 加盘需用户态和内核都为 OpenZFS 2.3 且池特性开启。用户态为 2.2 时 `zpool get feature@raidz_expansion`
// 无输出且失败，所以先查用户态版本再问池；每种拒绝都说明缺哪一半。
func TestRaidzExpansionDetection(t *testing.T) {
	cases := []struct {
		name  string
		outs  [][]byte
		errAt int
		ok    bool
		note  string
		calls int
	}{
		{"userland too old", [][]byte{[]byte("zfs-2.2.2-0ubuntu9.4\nzfs-kmod-2.3.4-1ubuntu2\n")}, 0, false, "2.2.2", 1},
		{"feature enabled", [][]byte{[]byte("zfs-2.3.1-1\nzfs-kmod-2.3.1-1\n"), []byte("enabled\n")}, 0, true, "", 2},
		{"feature active", [][]byte{[]byte("zfs-2.3.1-1\nzfs-kmod-2.3.1-1\n"), []byte("active\n")}, 0, true, "", 2},
		{"feature disabled", [][]byte{[]byte("zfs-2.3.1-1\nzfs-kmod-2.3.1-1\n"), []byte("disabled\n")}, 0, false, "raidz_expansion", 2},
		{"feature unknown to zpool", [][]byte{[]byte("zfs-2.3.1-1\nzfs-kmod-2.3.1-1\n"), []byte("")}, 2, false, "raidz_expansion", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &sequenceRunner{outs: tc.outs}
			if tc.errAt > 0 {
				runner = &sequenceRunner{outs: tc.outs, errAt: tc.errAt, err: errors.New("bad property list: invalid property 'feature@raidz_expansion'")}
			}
			client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ok, note := client.RaidzExpansion(context.Background(), "tank")
			if ok != tc.ok || (tc.note != "" && !strings.Contains(note, tc.note)) || (tc.ok && note != "") {
				t.Fatalf("ok=%v note=%q, want ok=%v note containing %q", ok, note, tc.ok, tc.note)
			}
			if len(runner.calls) != tc.calls {
				t.Fatalf("calls = %#v, want %d", runner.calls, tc.calls)
			}
			if runner.calls[0].name != "zpool" || runner.calls[0].args[0] != "version" {
				t.Fatalf("first call = %#v, want zpool version", runner.calls[0])
			}
			if tc.calls == 2 && !reflect.DeepEqual(runner.calls[1].args, []string{"get", "-Hp", "-o", "value", "feature@raidz_expansion", "tank"}) {
				t.Fatalf("second call = %#v", runner.calls[1])
			}
		})
	}
}

func TestPoolWriteCacheFlushCallszpoolSync(t *testing.T) {
	runner := &fakeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.FlushWriteCache(context.Background(), "tank"); err != nil {
		t.Fatalf("flush = %v", err)
	}
	want := fakeCall{name: "zpool", args: []string{"sync", "tank"}}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call = %#v, want %#v", runner.calls[0], want)
	}
}

func TestPoolStatusParsesWriteCacheDisks(t *testing.T) {
	runner := &sequenceRunner{
		outs: [][]byte{
			[]byte("tank\tONLINE\n"),
			[]byte("268435456\t805306368\n"),
			[]byte(`
pool: tank
  scan: resilver in progress since Mon Jun 29 12:00:00 2026
        512M scanned, 128M issued, 12.5% done
  STATE: ONLINE
NAME        STATE     READ WRITE CKSUM
tank        ONLINE      -      -     -
/dev/sdb    ONLINE      -      -     -
logs
/dev/nvme0n1 ONLINE      -      -     -
/dev/nvme0n2 ONLINE      -      -     -
`),
		},
	}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := client.PoolStatus(context.Background(), "tank")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Disks) != 3 {
		t.Fatalf("status disks = %#v", status.Disks)
	}
	if status.Disks[0].Role != domain.PoolDiskRoleData || status.Disks[1].Role != domain.PoolDiskRoleWriteCache || status.Disks[2].Role != domain.PoolDiskRoleWriteCache {
		t.Fatalf("status = %#v", status.Disks)
	}
}

func TestBackupCommandsFullAndIncremental(t *testing.T) {
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.Backup(context.Background(), "tank/cfg", "backup/ndiskless/cfg", "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.calls[0], fakeCall{name: "zfs", args: []string{"create", "-p", "backup/ndiskless"}}) {
		t.Fatalf("create call = %#v", runner.calls[0])
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-w", "tank/cfg@snap-1"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"recv", "-F", "backup/ndiskless/cfg"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}

	runner = &pipeRunner{}
	client = New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.Backup(context.Background(), "tank/cfg", "backup/ndiskless/cfg", "snap-2", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-w", "-i", "tank/cfg@snap-1", "tank/cfg@snap-2"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
}

func TestReceiveFileUsesZFSReceive(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.zfs")
	if err := os.WriteFile(sourcePath, []byte("stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"receive", "-F", "tank/win11"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q", got)
	}
}

func TestReceiveFileReportsProgress(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.zfs")
	if err := os.WriteFile(sourcePath, bytes.Repeat([]byte("a"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var got []int
	onProgress := func(percent int) { got = append(got, percent) }
	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", onProgress); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("onProgress was never called")
	}
	if got[len(got)-1] != 100 {
		t.Fatalf("last reported progress = %d, want 100", got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("progress not strictly increasing: %v", got)
		}
	}
}

func TestReceiveFileAcceptsGzip(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.zfs.gz")
	f, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("stream")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"receive", "-F", "tank/win11"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q", got)
	}
}

func TestReceiveFileAcceptsLegacyGzipExtension(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.gzip")
	f, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("stream")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"receive", "-F", "tank/win11"}}) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q", got)
	}
}

// 文件名以 .gzip 结尾不代表已压缩：改过名的原始流应正常导入，内容真坏由 zfs receive 去报。
func TestReceiveFilePassesThroughStreamNamedGzip(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.gzip")
	if err := os.WriteFile(sourcePath, []byte("stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q, want the bytes verbatim", got)
	}
}

type fakeRunner struct {
	calls []fakeCall
	out   []byte
	err   error
}

type fakeCall struct {
	name string
	args []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
	return r.out, r.err
}

type pipeRunner struct {
	fakeRunner
	mu          sync.Mutex
	streamCalls []fakeCall
	streamData  [][]byte
}

func (r *pipeRunner) RunWithIO(_ context.Context, name string, stdin io.Reader, _ io.Writer, args ...string) error {
	var data []byte
	if stdin != nil {
		var err error
		data, err = io.ReadAll(stdin)
		if err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamCalls = append(r.streamCalls, fakeCall{name: name, args: append([]string{}, args...)})
	r.streamData = append(r.streamData, data)
	return nil
}

func (r *pipeRunner) hasStreamCall(want fakeCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.streamCalls {
		if reflect.DeepEqual(call, want) {
			return true
		}
	}
	return false
}

type sequenceRunner struct {
	calls []fakeCall
	outs  [][]byte
	err   error
	errAt int // 大于 0 时只有第 errAt 次调用返回 err
}

func (r *sequenceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
	err := r.err
	if r.errAt > 0 && len(r.calls) != r.errAt {
		err = nil
	}
	if len(r.outs) == 0 {
		return nil, err
	}
	if idx := len(r.calls) - 1; idx < len(r.outs) {
		return r.outs[idx], err
	}
	return r.outs[len(r.outs)-1], err
}

// 合并和超管保存所用的命令。上层都用假池驱动，argv 本身无处校验：`rename` 两个操作数顺序反了、
// `promote` 变成空操作，上层都看不出来。
func TestMergeAndSaveCommands(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(*Client) error
		want []string
	}{
		{
			name: "rename 先源后目标",
			run:  func(c *Client) error { return c.Rename(ctx, "tank/cfg", "tank/cfg_before_super") },
			want: []string{"rename", "tank/cfg", "tank/cfg_before_super"},
		},
		{
			name: "promote 只带被提升的克隆",
			run:  func(c *Client) error { return c.Promote(ctx, "tank/cfg") },
			want: []string{"promote", "tank/cfg"},
		},
		{
			name: "snapshot 用 @ 连接",
			run:  func(c *Client) error { return c.SnapshotVolume(ctx, "tank/cfg", "v1") },
			want: []string{"snapshot", "tank/cfg@v1"},
		},
		{
			// -r 会删整棵树；一个还原点只是一个快照。
			name: "destroy 快照不带 -r",
			run:  func(c *Client) error { return c.DestroySnapshot(ctx, "tank/cfg", "v1") },
			want: []string{"destroy", "tank/cfg@v1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := tc.run(client); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runner.calls[0].args, tc.want) {
				t.Fatalf("args = %#v, want %#v", runner.calls[0].args, tc.want)
			}
		})
	}
}

// ListSnapshots 供一致性检查逐条比对还原点；若悄悄返回数据集而非快照，所有还原点都会被报缺失。
func TestListSnapshotsAsksForSnapshotsOnly(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank/img@0\n\ntank/img_default@0\ntank/img_default@v1\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := client.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/img@0", "tank/img_default@0", "tank/img_default@v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshots = %#v, want %#v", got, want)
	}
	wantArgs := []string{"list", "-H", "-t", "snapshot", "-o", "name"}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, wantArgs)
	}
}

// ListSnapshotClones 回答单个还原点能否删，ListDependentClones 回答整个配置能否删；
// 混用会错拒或错放删除。
func TestListSnapshotClonesMatchesOneSnapshotNotTheWholeDataset(t *testing.T) {
	runner := &fakeRunner{out: []byte(
		"tank/CLIENT-AABB\ttank/cfg@v1\n" +
			"tank/cfg_fork\ttank/cfg@v1\n" +
			"tank/CLIENT-BBCC\ttank/cfg@v2\n" + // 同一数据集，不同快照
			"tank/other\t-\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := client.ListSnapshotClones(context.Background(), "tank/cfg@v1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/CLIENT-AABB", "tank/cfg_fork"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clones = %#v, want %#v", got, want)
	}
	wantArgs := []string{"list", "-H", "-t", "volume", "-o", "name,origin"}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.calls[0].args, wantArgs)
	}
}

// 池不存在报 "no such pool" 而非 "dataset does not exist"；把「已不存在」当成功的删除两者都得认，
// 否则拔了盘的池留下的记录在产品里删不掉。
func TestIsNotExistRecognisesAMissingPool(t *testing.T) {
	poolGone := storage.CommandError{
		Name: "zpool", Args: []string{"destroy", "tank2"},
		Output: "cannot open 'tank2': no such pool", Err: errors.New("exit status 1"),
	}
	if !IsNotExist(poolGone) {
		t.Fatal("a missing pool must read as not-exist")
	}
	datasetGone := storage.CommandError{
		Name: "zfs", Output: "cannot open 'tank/x': dataset does not exist", Err: errors.New("exit status 1"),
	}
	if !IsNotExist(datasetGone) {
		t.Fatal("a missing dataset must still read as not-exist")
	}
	busy := storage.CommandError{
		Name: "zfs", Output: "cannot destroy 'tank/x': dataset is busy", Err: errors.New("exit status 1"),
	}
	if IsNotExist(busy) {
		t.Fatal("a busy dataset is not a missing one")
	}
}

// exportRunner 模拟导出时的 zfs：RunWithIO 把流字节写进调用方的 stdout，即 SendSnapshot 转交给文件或下载的内容。
type exportRunner struct {
	fakeRunner
	streamCalls []fakeCall
	stream      []byte
	streamErr   error
}

func (r *exportRunner) RunWithIO(_ context.Context, name string, _ io.Reader, stdout io.Writer, args ...string) error {
	r.streamCalls = append(r.streamCalls, fakeCall{name: name, args: append([]string{}, args...)})
	if r.streamErr != nil {
		return r.streamErr
	}
	_, err := stdout.Write(r.stream)
	return err
}

func TestSendSnapshotStreamsRawSendIntoWriter(t *testing.T) {
	runner := &exportRunner{stream: []byte("zfs-stream-bytes")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var buf bytes.Buffer
	if err := client.SendSnapshot(context.Background(), "tank/nd/win11@ndexport-1", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "zfs-stream-bytes" {
		t.Fatalf("stream = %q", buf.String())
	}
	want := fakeCall{name: "zfs", args: []string{"send", "-w", "tank/nd/win11@ndexport-1"}}
	if len(runner.streamCalls) != 1 || !reflect.DeepEqual(runner.streamCalls[0], want) {
		t.Fatalf("stream calls = %#v", runner.streamCalls)
	}
}

func TestSendSizeParsesDryRunEstimate(t *testing.T) {
	runner := &fakeRunner{out: []byte("full\ttank/nd/win11@ndexport-1\t42949672960\nsize\t42949672960\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	size, err := client.SendSize(context.Background(), "tank/nd/win11@ndexport-1")
	if err != nil {
		t.Fatal(err)
	}
	if size != 42949672960 {
		t.Fatalf("size = %d", size)
	}
	want := fakeCall{name: "zfs", args: []string{"send", "-nP", "-w", "tank/nd/win11@ndexport-1"}}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestReferencedReadsDatasetProperty(t *testing.T) {
	runner := &fakeRunner{out: []byte("10737418240\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	size, err := client.Referenced(context.Background(), "tank/nd/win11")
	if err != nil {
		t.Fatal(err)
	}
	if size != 10737418240 {
		t.Fatalf("referenced = %d", size)
	}
	want := fakeCall{name: "zfs", args: []string{"get", "-Hp", "-o", "value", "referenced", "tank/nd/win11"}}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

// 保留 .zfs 名的 gzip 流（手工压缩的导出或改名的 .zfs.gz）也必须识别出来；按扩展名猜会把压缩字节交给
// zfs receive，报出与真实原因无关的流错误。
func TestReceiveFileDetectsGzipByContentNotName(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "win10.zfs")
	f, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("stream")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q, want the decompressed stream", got)
	}
}

// 反过来，名为 .gz 的未压缩流不能经 gzip 读取，否则好好的导入会死在 "invalid header"。
func TestReceiveFileDoesNotDecompressPlainStreamNamedGz(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "image.zfs.gz")
	if err := os.WriteFile(sourcePath, []byte("stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &pipeRunner{}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.ReceiveFile(context.Background(), sourcePath, "tank/win11", nil); err != nil {
		t.Fatal(err)
	}
	if got := runner.streamData[0]; string(got) != "stream" {
		t.Fatalf("stream data = %q, want the bytes verbatim", got)
	}
}

// 备份池从 zpool 推导而不是读配置或目录库：每台机器最多两个池，非数据池即备份池；备机的目录库可能落后。
func TestListPoolNamesAsksZpool(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank\nbackup\n")}
	client := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := client.ListPoolNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "tank" || got[1] != "backup" {
		t.Fatalf("pools = %#v", got)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "zpool" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if strings.Join(runner.calls[0].args, " ") != "list -H -o name" {
		t.Fatalf("args = %#v", runner.calls[0].args)
	}
}

// SetPool 让池名可在运行期确定：服务先无池启动，运维在界面建池命名后立即生效，无需命令行参数或重启。
func TestSetPoolReboundsDatasetPaths(t *testing.T) {
	c := New("tank", &fakeRunner{}, nil)
	if got := c.Dataset("winmin"); got != "tank/nd/winmin" {
		t.Fatalf("初始 Dataset = %q, want tank/nd/winmin", got)
	}
	c.SetPool("mypool")
	if got := c.PoolName(); got != "mypool" {
		t.Fatalf("SetPool 后 PoolName = %q, want mypool", got)
	}
	if got := c.Dataset("winmin"); got != "mypool/nd/winmin" {
		t.Fatalf("SetPool 后 Dataset = %q, want mypool/nd/winmin", got)
	}
	if got := c.Snapshot("winmin", "0"); got != "mypool/nd/winmin@0" {
		t.Fatalf("SetPool 后 Snapshot = %q, want mypool/nd/winmin@0", got)
	}
}

// 池名读写须并发安全：后台建池任务改池名的同时，开机路径可能在读。
func TestSetPoolConcurrentSafe(t *testing.T) {
	c := New("tank", &fakeRunner{}, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			c.SetPool("p")
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = c.Dataset("x")
	}
	<-done
}

// 只有 scan: 行的「resilver in progress」和 remove: 行的进行中才算进行中；已完成的记录、status 说明、
// REMOVED 设备行都不能让页面一直显示「重建中/移除中」。
func TestPoolDetailsReportsOnlyOperationsInProgress(t *testing.T) {
	cases := []struct {
		name, out, op, progress string
	}{
		{"重建进行中", `  pool: tank
 state: DEGRADED
  scan: resilver in progress since Sat Oct  3 06:00:00 2026
	1.23G scanned at 100M/s, 500M issued at 50M/s, 10G total
	500M resilvered, 5.00% done, 00:03:10 to go
config:

	NAME          STATE     READ WRITE CKSUM
	tank          DEGRADED     0     0     0
	  mirror-0    DEGRADED     0     0     0
	    /dev/sdb  ONLINE       0     0     0
	    /dev/sdc  ONLINE       0     0     0  (resilvering)
`, "resilver", "5.00%"},
		{"移除进行中", `  pool: tank
 state: ONLINE
remove: Evacuation of /dev/sdc in progress since Sat Oct  3 06:00:00 2026
	2.11G copied out of 10.0G at 100M/s, 21.10% done, 0h1m to go
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  /dev/sdb  ONLINE       0     0     0
	  /dev/sdc  ONLINE       0     0     0
`, "remove", "21.10%"},
		{"重建已完成", `  pool: tank
 state: ONLINE
  scan: resilvered 1.50G in 00:01:02 with 0 errors on Sat Oct  3 06:00:00 2026
config:

	NAME          STATE     READ WRITE CKSUM
	tank          ONLINE       0     0     0
	  mirror-0    ONLINE       0     0     0
	    /dev/sdb  ONLINE       0     0     0
	    /dev/sdc  ONLINE       0     0     0
`, "", ""},
		{"移除已完成", `  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:00:01 with 0 errors on Sat Oct  3 05:00:00 2026
remove: Removal of vdev 1 copied 1.50G in 0h0m, completed on Sat Oct  3 06:00:00 2026
	6.50K memory used for removed device mappings
config:

	NAME          STATE     READ WRITE CKSUM
	tank          ONLINE       0     0     0
	  /dev/sdb    ONLINE       0     0     0
`, "", ""},
		{"设备被管理员移除", `  pool: tank
 state: DEGRADED
status: One or more devices has been removed by the administrator.
	Sufficient replicas exist for the pool to continue functioning in a
	degraded state.
action: Online the device using zpool online' or replace the device with
	'zpool replace'.
config:

	NAME          STATE     READ WRITE CKSUM
	tank          DEGRADED     0     0     0
	  mirror-0    DEGRADED     0     0     0
	    /dev/sdb  ONLINE       0     0     0
	    /dev/sdc  REMOVED      0     0     0
`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePoolDetails(c.out, "tank")
			if got.Operation != c.op || got.Progress != c.progress {
				t.Fatalf("operation=%q progress=%q，期望 %q %q", got.Operation, got.Progress, c.op, c.progress)
			}
		})
	}
}
