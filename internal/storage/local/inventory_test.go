package local

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// 一致性检查拿 Inventory 对比库里每一行，「须先删除 X」的提示由两个依赖列表生成；
// 这里直接测真实实现中去池前缀、拼快照路径的部分。

func TestInventoryReportsPoolRelativeNames(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/img_default", "tank/run/CLIENT-AABBCCDDEEFF"}
	z.snaps = map[string]bool{"tank/nd/img@0": true, "tank/nd/img_default@0": true}
	agent := New("server-a", z)

	inv, err := agent.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 必须是池相对名，与库里 ID 一致；否则 "tank/nd/img" 对不上 "img"，所有镜像都会报缺失。
	if !reflect.DeepEqual(inv.Datasets, []string{"img", "img_default", "CLIENT-AABBCCDDEEFF"}) {
		t.Fatalf("datasets = %#v", inv.Datasets)
	}
	if !reflect.DeepEqual(inv.Snapshots, []string{"img@0", "img_default@0"}) {
		t.Fatalf("snapshots = %#v", inv.Snapshots)
	}
}

// 别的池的数据集不属于本节点清单，报上来会被一致性检查当成孤儿。
func TestInventorySkipsWhatIsNotInThisPool(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "other/img", "tank"}
	agent := New("server-a", z)

	inv, err := agent.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Datasets, []string{"img"}) {
		t.Fatalf("datasets = %#v", inv.Datasets)
	}
}

func TestInventorySurfacesAPoolItCannotRead(t *testing.T) {
	agent := New("server-a", &failingListPool{fakePool: newFakePool(), err: errors.New("zfs: pool is faulted")})
	if _, err := agent.Inventory(context.Background()); err == nil {
		t.Fatal("expected the read failure to surface, not an empty inventory")
	}
}

type failingListPool struct {
	*fakePool
	err error
}

func (p *failingListPool) ListDatasets(context.Context) ([]string, error) { return nil, p.err }

func TestConfigDependentsFindsEveryCloneOfTheConfig(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/cfg", "tank/nd/cfg_fork", "tank/run/CLIENT-AABBCCDDEEFF", "tank/nd/unrelated"}
	z.snaps = map[string]bool{"tank/nd/cfg@0": true, "tank/nd/cfg@v1": true, "tank/nd/img@0": true}
	z.origin = map[string]string{
		"tank/nd/cfg":                  "tank/nd/img@0",
		"tank/nd/cfg_fork":             "tank/nd/cfg@0",  // 从某个还原点派生的配置
		"tank/run/CLIENT-AABBCCDDEEFF": "tank/nd/cfg@v1", // 从另一个还原点开机的客户机
		"tank/nd/unrelated":            "tank/nd/img@0",
	}
	agent := New("server-a", z)

	got, err := agent.ConfigDependents(context.Background(), "cfg")
	if err != nil {
		t.Fatal(err)
	}
	// 配置任一快照上的依赖都要列出：删配置会把它们全删掉。
	if !reflect.DeepEqual(got, []string{"tank/nd/cfg_fork", "tank/run/CLIENT-AABBCCDDEEFF"}) {
		t.Fatalf("dependents = %#v", got)
	}
}

// 还原点是单个快照：只有挂在它上面的依赖才阻止删除，按整个配置回答会误拒正常删除。
func TestReductionDependentsAnswersForOneSnapshot(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/cfg", "tank/nd/cfg_fork", "tank/run/CLIENT-AABBCCDDEEFF"}
	z.snaps = map[string]bool{"tank/nd/cfg@0": true, "tank/nd/cfg@v1": true}
	z.origin = map[string]string{
		"tank/nd/cfg_fork":             "tank/nd/cfg@0",
		"tank/run/CLIENT-AABBCCDDEEFF": "tank/nd/cfg@v1",
	}
	agent := New("server-a", z)

	for _, tc := range []struct {
		name string
		want []string
	}{
		{"@0", []string{"tank/nd/cfg_fork"}},
		{"v1", []string{"tank/run/CLIENT-AABBCCDDEEFF"}}, // 带不带 "@" 都行
		{"@v2", nil},
	} {
		got, err := agent.ReductionDependents(context.Background(), "cfg", tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s dependents = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestDependentLookupsRejectMissingArguments(t *testing.T) {
	agent := New("server-a", newFakePool())
	ctx := context.Background()
	if _, err := agent.ConfigDependents(ctx, "  "); err == nil {
		t.Fatal("empty config should be refused, not looked up as \"\"")
	}
	if _, err := agent.ReductionDependents(ctx, "cfg", " @ "); err == nil {
		t.Fatal("empty reduction should be refused")
	}
}

// SuperStart 与普通开机只差一个标志；标志错了，运维的持久盘就会走临时克隆的路径。
func TestSuperStartAsksForThePersistentClone(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/cfg"}
	z.snaps = map[string]bool{"tank/nd/cfg@0": true}
	agent := New("server-a", z)

	info, err := agent.SuperStart(context.Background(), storage.ClientReq{
		MAC:    "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{ConfigID: "cfg", SnapshotName: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.System.VolPath != "/dev/zvol/tank/run/SCLIENT-AABBCCDDEEFF" {
		t.Fatalf("volpath = %q, want the SCLIENT clone", info.System.VolPath)
	}
}

// ActiveClientMACs 来自在线 iSCSI 会话。没有 exporter 时必须报错，
// 返回空会被回收器理解为「所有机器都关了」。
func TestActiveClientMACsRefusesToGuessWithoutAnExporter(t *testing.T) {
	agent := New("server-a", newFakePool())
	if _, err := agent.ActiveClientMACs(context.Background()); !errors.Is(err, storage.ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented rather than an empty list", err)
	}
}

func TestActiveClientMACsReportsWhatTheExporterSees(t *testing.T) {
	exporter := &fakeExporter{sessions: []string{"AABBCCDDEEFF", "001122334455"}}
	agent := New("server-a", newFakePool(), exporter)

	got, err := agent.ActiveClientMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"AABBCCDDEEFF", "001122334455"}) {
		t.Fatalf("macs = %#v", got)
	}
}

// 从池恢复目录需要 CoW 谱系而不只是名单：哪个是根（镜像），哪个克隆自谁的快照（配置或派生配置）。
func TestInventoryReportsTheCloneLineage(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/img_default", "tank/nd/img_fork", "tank/run/CLIENT-AABBCCDDEEFF"}
	z.snaps = map[string]bool{"tank/nd/img@0": true, "tank/nd/img_default@0": true, "tank/nd/img_default@v1": true}
	z.origin = map[string]string{
		"tank/nd/img_default":          "tank/nd/img@0",
		"tank/nd/img_fork":             "tank/nd/img_default@v1",
		"tank/run/CLIENT-AABBCCDDEEFF": "tank/nd/img_default@0",
	}
	agent := New("server-a", z)

	inv, err := agent.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 两侧都是池相对名，与清单其余部分一致。
	want := map[string]string{
		"img_default":         "img@0",
		"img_fork":            "img_default@v1",
		"CLIENT-AABBCCDDEEFF": "img_default@0",
	}
	for dataset, origin := range want {
		if inv.Origins[dataset] != origin {
			t.Fatalf("origin of %s = %q, want %q (all: %#v)", dataset, inv.Origins[dataset], origin, inv.Origins)
		}
	}
	// 根数据集不出现在 origin 表里，而不是对应空串。
	if _, ok := inv.Origins["img"]; ok {
		t.Fatalf("the image reports an origin: %#v", inv.Origins)
	}
}

// 收养池中恢复的镜像需要系统类型，库只接受 windows 或 linux；猜错会下发错误启动脚本
// （清网关只对 Windows）。看分区表即可判断，无需挂载：NTFS 即 Windows。
func TestDetectOSTypeReadsThePartitionTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lsblk  string
		want   domain.OSType
		wantOK bool
	}{
		{
			name:   "Windows 盘",
			lsblk:  `{"blockdevices":[{"path":"/dev/zd0","fstype":null,"children":[{"path":"/dev/zd0p1","fstype":"vfat"},{"path":"/dev/zd0p3","fstype":"ntfs"}]}]}`,
			want:   domain.OSTypeWindows,
			wantOK: true,
		},
		{
			name:   "Linux 盘",
			lsblk:  `{"blockdevices":[{"path":"/dev/zd0","fstype":null,"children":[{"path":"/dev/zd0p1","fstype":"vfat"},{"path":"/dev/zd0p2","fstype":"ext4"}]}]}`,
			want:   domain.OSTypeLinux,
			wantOK: true,
		},
		{
			// 认不出就如实报告，不随便选一个。
			name:   "认不出来",
			lsblk:  `{"blockdevices":[{"path":"/dev/zd0","fstype":null}]}`,
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := New("server-a", newFakePool())
			agent.runner = &fakeRunner{lsblkOutput: tc.lsblk}
			agent.resolveNodeFn = func(context.Context, string) (string, error) { return "/dev/zd0", nil }

			got, ok := agent.DetectOSType(context.Background(), "img")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("os = %q, want %q", got, tc.want)
			}
		})
	}
}
