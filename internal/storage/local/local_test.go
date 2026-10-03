package local

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

func TestCreateClientLUNClonesSystemVolume(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	info, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC: "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{
			ConfigID:     "cfg-default",
			SnapshotName: "red-0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Server != "server-a" {
		t.Fatalf("server = %q", info.Server)
	}
	if info.System.VolPath != "/dev/zvol/tank/run/CLIENT-AABBCCDDEEFF" {
		t.Fatalf("volpath = %q", info.System.VolPath)
	}
	want := [][]string{
		{"list", "-H", "-o", "name"}, // 开机只列一次数据集
		{"clone", "tank/nd/cfg-default@red-0", "tank/run/CLIENT-AABBCCDDEEFF"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestCreateClientLUNTearsDownPreviousSessionFirst(t *testing.T) {
	runner := &fakeRunner{listOutput: "tank/run/CLIENT-AABBCCDDEEFF\ntank/run/CLIENT-AABBCCDDEEFF-DATA-1\ntank/run/SCLIENT-AABBCCDDEEFF\n"}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	_, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC: "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{
			ConfigID:     "cfg-default",
			SnapshotName: "red-0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 一次批量拆除整个 target 和上次的 LUN backstore：lun0（普通与残留超管克隆共用）和 lun1。
	if exporter.tornTarget != iscsi.TargetForMAC("aa:bb:cc:dd:ee:ff") {
		t.Fatalf("torn target = %q", exporter.tornTarget)
	}
	if !reflect.DeepEqual(exporter.tornLUNs, []int{0, 1}) {
		t.Fatalf("torn luns = %#v", exporter.tornLUNs)
	}
	// 普通开机也会丢弃残留的超管持久克隆。
	want := [][]string{
		{"list", "-H", "-o", "name"},
		{"destroy", "-r", "tank/run/CLIENT-AABBCCDDEEFF"},
		{"destroy", "-r", "tank/run/CLIENT-AABBCCDDEEFF-DATA-1"},
		{"destroy", "-r", "tank/run/SCLIENT-AABBCCDDEEFF"},
		{"clone", "tank/nd/cfg-default@red-0", "tank/run/CLIENT-AABBCCDDEEFF"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	if len(exporter.reqs) != 1 || exporter.reqs[0].LUN != 0 {
		t.Fatalf("export reqs = %#v", exporter.reqs)
	}
}

func TestCleanupClientClonesDestroysClientDataset(t *testing.T) {
	// 经 ZFS 接口断言销毁了哪些数据集，而不是命令行参数（那是 zfs 包的测试范围）。
	zfsFake := poolWith("tank/run/CLIENT-AABBCCDDEEFF", "tank/run/CLIENT-AABBCCDDEEFF-DATA-1", "tank/run/CLIENT-OTHER")
	exporter := &fakeExporter{}
	agent := New("server-a", zfsFake, exporter)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	if err := agent.CleanupClientClones(context.Background(), "aa:bb:cc:dd:ee:ff", nil); err != nil {
		t.Fatal(err)
	}
	if exporter.deletedTarget != iscsi.TargetForMAC("aa:bb:cc:dd:ee:ff") {
		t.Fatalf("deleted target = %q", exporter.deletedTarget)
	}
	if !reflect.DeepEqual(exporter.deletedLUNs, []int{0, 1}) {
		t.Fatalf("deleted luns = %#v", exporter.deletedLUNs)
	}
	if !reflect.DeepEqual(zfsFake.destroyed, []string{"tank/run/CLIENT-AABBCCDDEEFF", "tank/run/CLIENT-AABBCCDDEEFF-DATA-1"}) {
		t.Fatalf("destroyed = %#v", zfsFake.destroyed)
	}
	// 无关客户机的克隆保留。
	if !reflect.DeepEqual(zfsFake.datasets, []string{"tank/run/CLIENT-OTHER"}) {
		t.Fatalf("datasets = %#v", zfsFake.datasets)
	}
}

func TestCleanupClientClonesHonorsKeepList(t *testing.T) {
	zfsFake := poolWith("tank/run/CLIENT-AABBCCDDEEFF")
	agent := New("server-a", zfsFake)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	if err := agent.CleanupClientClones(context.Background(), "aa:bb:cc:dd:ee:ff", []string{"CLIENT-AABBCCDDEEFF"}); err != nil {
		t.Fatal(err)
	}
	if len(zfsFake.destroyAttempts) != 0 {
		t.Fatalf("kept dataset destroyed: %#v", zfsFake.destroyAttempts)
	}
}

func TestCreateClientLUNExportsWhenExporterInjected(t *testing.T) {
	runner := &fakeRunner{}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	info, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC: "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{
			ConfigID:     "cfg-default",
			SnapshotName: "red-0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exporter.reqs[0].VolPath != "/dev/zvol/tank/run/CLIENT-AABBCCDDEEFF" {
		t.Fatalf("export volpath = %q", exporter.reqs[0].VolPath)
	}
	if info.Target != "target-a" || info.System.Target != "target-a" {
		t.Fatalf("info = %#v", info)
	}
}

func TestCreateClientLUNExportsDataDisksInSameTarget(t *testing.T) {
	runner := &fakeRunner{}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	info, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC: "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{
			ConfigID:     "cfg-system",
			SnapshotName: "red-system",
		},
		DataDisks: []storage.ClientSource{
			{ConfigID: "cfg-data-1", SnapshotName: "red-data-1", MountTarget: "D", LUN: 1},
			{ConfigID: "cfg-data-2", SnapshotName: "red-data-2", MountTarget: "E", LUN: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Target != "target-a" || len(info.DataDisks) != 2 {
		t.Fatalf("info = %#v", info)
	}
	for i, lun := range []storage.LUN{info.System, info.DataDisks[0], info.DataDisks[1]} {
		if lun.Target != "target-a" || lun.LUN != i {
			t.Fatalf("lun %d = %#v", i, lun)
		}
	}
	if info.DataDisks[0].MountTarget != "D" || info.DataDisks[1].MountTarget != "E" {
		t.Fatalf("data disks = %#v", info.DataDisks)
	}
	wantClones := [][]string{
		{"list", "-H", "-o", "name"},
		{"clone", "tank/nd/cfg-system@red-system", "tank/run/CLIENT-AABBCCDDEEFF"},
		{"clone", "tank/nd/cfg-data-1@red-data-1", "tank/run/CLIENT-AABBCCDDEEFF-DATA-1"},
		{"clone", "tank/nd/cfg-data-2@red-data-2", "tank/run/CLIENT-AABBCCDDEEFF-DATA-2"},
	}
	if !reflect.DeepEqual(runner.args(), wantClones) {
		t.Fatalf("args = %#v, want %#v", runner.args(), wantClones)
	}
	wantLUNs := []int{0, 1, 2}
	if len(exporter.reqs) != len(wantLUNs) {
		t.Fatalf("export reqs = %#v", exporter.reqs)
	}
	for i, req := range exporter.reqs {
		if req.LUN != wantLUNs[i] {
			t.Fatalf("export reqs = %#v", exporter.reqs)
		}
	}
}

func TestSuperStartClonesPersistentVolumeOnce(t *testing.T) {
	runner := &fakeRunner{}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	info, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC:   "aa:bb:cc:dd:ee:ff",
		Super: true,
		System: storage.ClientSource{
			ConfigID:     "cfg-system",
			SnapshotName: "red-latest",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.System.VolPath != "/dev/zvol/tank/run/SCLIENT-AABBCCDDEEFF" || info.System.Target != "target-a" {
		t.Fatalf("info = %#v", info)
	}
	want := [][]string{
		{"list", "-H", "-o", "name"}, // 一次列表同时用于拆除和克隆复用
		{"clone", "tank/nd/cfg-system@red-latest", "tank/run/SCLIENT-AABBCCDDEEFF"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	if len(exporter.reqs) != 1 || exporter.reqs[0].LUN != 0 {
		t.Fatalf("export reqs = %#v", exporter.reqs)
	}
}

func TestSuperStartReusesPersistentVolume(t *testing.T) {
	runner := &fakeRunner{
		listOutput:   "tank/run/SCLIENT-AABBCCDDEEFF\n",
		originOutput: "tank/run/SCLIENT-AABBCCDDEEFF\ttank/nd/cfg-system@red-old\n",
	}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	_, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC:   "aa:bb:cc:dd:ee:ff",
		Super: true,
		System: storage.ClientSource{
			ConfigID:     "cfg-system",
			SnapshotName: "red-latest",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 复用前核对来源（同一配置、换了还原点也算本机的盘），不重新克隆。
	want := [][]string{
		{"list", "-H", "-o", "name"},
		{"list", "-H", "-t", "volume", "-o", "name,origin"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	// 复用的持久卷的旧 backstore（lun0）随 target 拆除释放，注入脚本才能在
	// ExportBlocks 重新占用前挂载它。
	if !reflect.DeepEqual(exporter.tornLUNs, []int{0}) {
		t.Fatalf("torn luns = %#v", exporter.tornLUNs)
	}
	if len(exporter.reqs) != 1 || exporter.reqs[0].VolPath != "/dev/zvol/tank/run/SCLIENT-AABBCCDDEEFF" {
		t.Fatalf("export reqs = %#v", exporter.reqs)
	}
}

func TestSuperStopDiscardDestroysPersistentClones(t *testing.T) {
	runner := &fakeRunner{listOutput: "tank/run/SCLIENT-AABBCCDDEEFF\ntank/run/SCLIENT-AABBCCDDEEFF-DATA-1\ntank/run/CLIENT-AABBCCDDEEFF\n"}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	if _, err := agent.SuperStop(context.Background(), storage.SuperStopReq{MAC: "aa:bb:cc:dd:ee:ff"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"list", "-H", "-o", "name"}, // 枚举待删 backstore
		{"list", "-H", "-o", "name"},
		{"destroy", "-r", "tank/run/SCLIENT-AABBCCDDEEFF"},
		{"destroy", "-r", "tank/run/SCLIENT-AABBCCDDEEFF-DATA-1"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	// 占住 zvol 的 backstore 要先于数据集删除：lun0（系统盘）和 lun1。
	if !reflect.DeepEqual(exporter.deletedLUNs, []int{0, 1}) {
		t.Fatalf("deleted luns = %#v", exporter.deletedLUNs)
	}
}

func TestSuperStopSavePromotesAndSnapshotsConfig(t *testing.T) {
	z := seedSuperPool()
	z.datasets = append(z.datasets, "tank/run/SCLIENT-AABBCCDDEEFF-DATA-1")
	exporter := &fakeExporter{}
	agent := New("server-a", z, exporter)

	res, err := agent.SuperStop(context.Background(), superStopReq())
	if err != nil {
		t.Fatal(err)
	}
	if reduction := res.System; reduction.ID != "cfg_v1" || reduction.Name != "@v1" || reduction.ConfigID != "cfg" {
		t.Fatalf("reduction = %#v", reduction)
	}
	if exporter.deletedTarget != "iqn.2026-06.local.ndiskless:client-aabbccddeeff" {
		t.Fatalf("deleted target = %q", exporter.deletedTarget)
	}
	// promote/改名之前系统盘和数据盘的 backstore 都要删掉，否则会残留指向改名后卷的存储对象。
	if !reflect.DeepEqual(exporter.deletedLUNs, []int{0, 1}) {
		t.Fatalf("deleted luns = %#v", exporter.deletedLUNs)
	}
	want := []string{
		"promote tank/run/SCLIENT-AABBCCDDEEFF",
		"rename tank/nd/cfg",                   // 被编辑的配置挪开
		"rename tank/run/SCLIENT-AABBCCDDEEFF", // 机器的克隆顶替其位置
		"snapshot tank/nd/cfg@v1",
		"destroy tank/nd/cfg_before_super",
		"destroy tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
	}
	if !reflect.DeepEqual(z.ops, want) {
		t.Fatalf("ops = %#v, want %#v", z.ops, want)
	}
}

func TestImportImageCreatesInitialSnapshotAndDefaultConfig(t *testing.T) {
	runner := &fakeRunner{recordIO: true, volsizeOutput: "85899345920\n"}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	root, sourcePath := importImageFixture(t, "win11.zfs")

	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.ID != "win11" || result.Config.ID != "win11_default" || result.Reduction.Name != "@0" {
		t.Fatalf("result = %#v", result)
	}
	// 流自己决定 zvol 大小，必须回读逻辑大小，否则镜像 Size 为 0，页面显示 "0.0 GiB"。
	if result.Image.Size != 85899345920 {
		t.Fatalf("image size = %d, want volsize read back", result.Image.Size)
	}
	want := [][]string{
		{"list", "-H", "-o", "name"},
		{"receive", "-F", "tank/nd/win11"},
		// 收到的流可能带导出时的 ndexport- 临时快照，导入在 receive 后立即查找它。
		{"list", "-H", "-t", "snapshot", "-o", "name"},
		{"get", "-Hp", "-o", "value", "volsize", "tank/nd/win11"},
		{"snapshot", "tank/nd/win11@0"},
		{"clone", "tank/nd/win11@0", "tank/nd/win11_default"},
		{"snapshot", "tank/nd/win11_default@0"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestImportImageAcceptsLegacyGzipFile(t *testing.T) {
	runner := &fakeRunner{recordIO: true}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	root, sourcePath := importImageFixture(t, "win11.gzip")
	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.args()[1], []string{"receive", "-F", "tank/nd/win11"}) {
		t.Fatalf("args = %#v", runner.args())
	}
}

func TestImportImageRollsBackOnCloneFailure(t *testing.T) {
	runner := &fakeRunner{recordIO: true, errAt: 6, err: errors.New("clone failed")}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	root, sourcePath := importImageFixture(t, "win11.zfs")

	_, err := agent.ImportImage(context.Background(), storage.ImportImageReq{Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows"})
	if err == nil {
		t.Fatal("import succeeded")
	}
	want := [][]string{
		{"list", "-H", "-o", "name"},
		{"receive", "-F", "tank/nd/win11"},
		// 同上，receive 后查找 ndexport- 快照。
		{"list", "-H", "-t", "snapshot", "-o", "name"},
		{"get", "-Hp", "-o", "value", "volsize", "tank/nd/win11"},
		{"snapshot", "tank/nd/win11@0"},
		{"clone", "tank/nd/win11@0", "tank/nd/win11_default"},
		{"destroy", "-r", "tank/nd/win11@0"},
		{"destroy", "-r", "tank/nd/win11"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestRollbackImportImageRestoresSource(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	err := agent.RollbackImportImage(context.Background(),
		storage.ImportImageReq{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: "windows"},
		storage.ImportImageResult{
			Image:  domain.Image{ID: "win11"},
			Config: domain.Config{ID: "win11_default"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"destroy", "-r", "tank/nd/win11_default"},
		{"destroy", "tank/nd/win11@0"},
		{"destroy", "-r", "tank/nd/win11"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func importImageFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	root := t.TempDir()
	sourcePath := filepath.Join(root, name)
	if strings.HasSuffix(strings.ToLower(name), ".gzip") || strings.HasSuffix(strings.ToLower(name), ".zfs.gz") {
		f, err := os.Create(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if _, err := gz.Write([]byte("zfs stream")); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return root, sourcePath
	}
	if err := os.WriteFile(sourcePath, []byte("zfs stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, sourcePath
}

func TestCreateConfigClonesImageReductionZero(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	result, err := agent.CreateConfig(context.Background(), "img-win", "Test Config", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := result.Config
	if cfg.ImageID != "img-win" || cfg.Name != "Test Config" || cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != cfg.ID+"_0" {
		t.Fatalf("result = %#v", result)
	}
	if got := runner.args()[0]; !reflect.DeepEqual(got, []string{"clone", "tank/nd/img-win@0", "tank/nd/" + cfg.ID}) {
		t.Fatalf("args = %#v", runner.args())
	}
}

// 新配置必须自带初始快照：下游都克隆 "<config>@<reduction>"，没有快照的配置启动不了任何客户机。
func TestCreateConfigSnapshotsInitialReduction(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	result, err := agent.CreateConfig(context.Background(), "img-win", "Test Config", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"snapshot", "tank/nd/" + result.Config.ID + "@0"}
	if got := runner.args(); len(got) < 2 || !reflect.DeepEqual(got[1], want) {
		t.Fatalf("args = %#v, want %v as second call", got, want)
	}
}

// 基线还原点要作为可入库的还原点返回：有自己的 ID，配置的默认指向该 ID，
// 而不是裸快照名（它对应不到任何还原点记录）。
func TestCreateConfigReturnsInitialReduction(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	result, err := agent.CreateConfig(context.Background(), "img-win", "Test Config", nil)
	if err != nil {
		t.Fatal(err)
	}
	red := result.Reduction
	if red.ID != result.Config.ID+"_0" || red.ConfigID != result.Config.ID || red.Name != "@0" || red.Status != domain.ReductionStatusReady {
		t.Fatalf("reduction = %#v", red)
	}
	if result.Config.DefaultReductionID == nil || *result.Config.DefaultReductionID != red.ID {
		t.Fatalf("default reduction = %#v, want %s", result.Config.DefaultReductionID, red.ID)
	}
}

// 基线快照失败时半成品配置必须删掉：它启动不了，库里也没有记录能事后清理它。
func TestCreateConfigRollsBackWhenSnapshotFails(t *testing.T) {
	runner := &fakeRunner{errAt: 2, err: errors.New("snapshot failed")}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if _, err := agent.CreateConfig(context.Background(), "img-win", "Test Config", nil); err == nil {
		t.Fatal("expected error")
	}
	cloned := runner.args()[0]
	dataset := cloned[len(cloned)-1]
	var destroyed bool
	for _, call := range runner.args() {
		if call[0] == "destroy" && call[len(call)-1] == dataset {
			destroyed = true
		}
	}
	if !destroyed {
		t.Fatalf("dataset %s was not destroyed; args = %#v", dataset, runner.args())
	}
}

func TestCreateConfigFromConfigUsesGivenReduction(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	reduction := "r-2023"

	result, err := agent.CreateConfig(context.Background(), "cfg-win", "restore", &reduction)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.args()[0]; !reflect.DeepEqual(got[0:2], []string{"clone", "tank/nd/cfg-win@r-2023"}) {
		t.Fatalf("args = %#v", runner.args())
	}
	// 派生克隆指定的源快照，但自己的还原点从 @0 开始；源快照名不是派生的还原点。
	cfg := result.Config
	if cfg.Name != "restore" || cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != cfg.ID+"_0" {
		t.Fatalf("result = %#v", result)
	}
}

func TestDeleteConfigDestroysDataset(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.DeleteConfig(context.Background(), "cfg-win"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"list", "-H", "-t", "volume", "-o", "name,origin"},
		{"destroy", "-r", "tank/nd/cfg-win"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestDeleteConfigCleansOrphanClientClonesFirst(t *testing.T) {
	runner := &fakeRunner{
		originOutput: "tank/run/CLIENT-AABBCCDDEEFF\ttank/nd/cfg-win@0\ntank/nd/OTHER\ttank/nd/other@0\n",
		listOutput:   "tank/run/CLIENT-AABBCCDDEEFF\ntank/nd/OTHER\n",
	}
	exporter := &fakeExporter{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))), exporter)

	if err := agent.DeleteConfig(context.Background(), "cfg-win"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"list", "-H", "-t", "volume", "-o", "name,origin"},
		{"list", "-H", "-o", "name"},
		{"destroy", "-r", "tank/run/CLIENT-AABBCCDDEEFF"},
		{"destroy", "-r", "tank/nd/cfg-win"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	if exporter.deletedTarget != iscsi.TargetForMAC("AABBCCDDEEFF") {
		t.Fatalf("deleted target = %q", exporter.deletedTarget)
	}
	if !reflect.DeepEqual(exporter.deletedLUNs, []int{0}) {
		t.Fatalf("deleted luns = %#v", exporter.deletedLUNs)
	}
}

// 合并后池的形态应与全新导入一致：镜像为链根带 @0，配置是它的克隆。顺序见 MergeConfig：
// 先删保留配置自己的快照，否则 promote 报 "conflicting snapshot '0' from parent"；
// 继承来的基线要等兄弟配置和旧镜像删掉后才能删。
func TestMergeConfigPromotesConfigIntoImageAndRebuildsIt(t *testing.T) {
	z := seedMergePool()
	agent := New("server-a", z)

	result, err := agent.MergeConfig(context.Background(), mergeReq())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"destroy tank/nd/cfg-drop",
		"destroy-snapshot tank/nd/cfg-keep@0",
		"destroy-snapshot tank/nd/cfg-keep@r1",
		"promote tank/nd/cfg-keep",
		"destroy tank/nd/img",
		"destroy-snapshot tank/nd/cfg-keep@0",
		"rename tank/nd/cfg-keep",
		"snapshot tank/nd/img@0",
		"clone tank/nd/cfg-keep",
		"snapshot tank/nd/cfg-keep@0",
	}
	if !reflect.DeepEqual(z.ops, want) {
		t.Fatalf("ops = %#v, want %#v", z.ops, want)
	}
	if got := z.state(); !equalStrings(got, wantMerged) {
		t.Fatalf("state = %#v, want %#v", got, wantMerged)
	}
	red := result.Reduction
	if red.ID != "cfg-keep_0" || red.ConfigID != "cfg-keep" || red.Name != "@0" || red.Status != domain.ReductionStatusReady {
		t.Fatalf("reduction = %#v", red)
	}
}

// 被合并掉的配置之间可能互相依赖，顺序事先未知。删不掉的要在其依赖者删掉后重试，
// 而不是让合并直接失败。
func TestMergeConfigDestroysSiblingsInDependencyOrder(t *testing.T) {
	z := &dependentZFS{
		fakePool: poolWith("tank/nd/img-win", "tank/nd/cfg-keep", "tank/nd/cfg-drop", "tank/nd/cfg-fork"),
		// cfg-fork 克隆自 cfg-drop，cfg-fork 删掉前 cfg-drop 删不了。
		blockedBy: map[string]string{"tank/nd/cfg-drop": "tank/nd/cfg-fork"},
	}
	agent := New("server-a", z)

	if _, err := agent.MergeConfig(context.Background(), storage.MergeConfigReq{
		ImageID:         "img-win",
		ConfigID:        "cfg-keep",
		DeleteConfigIDs: []string{"cfg-drop", "cfg-fork"},
		ReductionNames:  []string{"@0"},
	}); err != nil {
		t.Fatal(err)
	}
	// 两个兄弟都已删除，被阻塞的那个在依赖者删掉后重试成功。
	for _, dataset := range []string{"tank/nd/cfg-fork", "tank/nd/cfg-drop"} {
		var found bool
		for _, d := range z.destroyed {
			if d == dataset {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was not destroyed; destroyed = %#v", dataset, z.destroyed)
		}
	}
}

// dependentZFS 在阻塞者仍存在时拒绝销毁，模拟 "has dependent clones" 规则。
type dependentZFS struct {
	*fakePool
	blockedBy map[string]string
	alive     map[string]bool
}

func (d *dependentZFS) Destroy(ctx context.Context, dataset string) error {
	if d.alive == nil {
		d.alive = map[string]bool{}
		for _, blocker := range d.blockedBy {
			d.alive[blocker] = true
		}
	}
	if blocker, ok := d.blockedBy[dataset]; ok && d.alive[blocker] {
		return errors.New("cannot destroy: snapshot has dependent clones")
	}
	delete(d.alive, dataset)
	return d.fakePool.Destroy(ctx, dataset)
}

func TestMergeConfigRequiresImageAndConfig(t *testing.T) {
	agent := New("server-a", zfs.New("tank", &fakeRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if _, err := agent.MergeConfig(context.Background(), storage.MergeConfigReq{}); err == nil {
		t.Fatal("merge succeeded")
	}
}

func TestCreateReductionSnapshotsConfig(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	result, err := agent.CreateReduction(context.Background(), "cfg-win", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigID != "cfg-win" || result.Name != "@r1" {
		t.Fatalf("result = %#v", result)
	}
	want := [][]string{{"snapshot", "tank/nd/cfg-win@r1"}}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v", runner.args())
	}
}

func TestDeleteReductionDestroysSnapshot(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.DeleteReduction(context.Background(), "cfg-win", "@r1"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"destroy", "tank/nd/cfg-win@r1"}}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v", runner.args())
	}
}

func TestMergeReductionDeletesSnapshots(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	destroyed, err := agent.MergeReduction(context.Background(), "cfg-win", []string{"@0", "@1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(destroyed, []string{"@0", "@1"}) {
		t.Fatalf("destroyed = %#v", destroyed)
	}
	want := [][]string{
		{"destroy", "tank/nd/cfg-win@0"},
		{"destroy", "tank/nd/cfg-win@1"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v", runner.args())
	}
}

func TestDeleteImageDestroysDataset(t *testing.T) {
	runner := &fakeRunner{listOutput: "tank/nd/win11\n"}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.DeleteImage(context.Background(), "win11", nil); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"list", "-H", "-o", "name"}, {"destroy", "-r", "tank/nd/win11"}}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

// 用量按镜像/配置 ID（去掉池前缀）返回，且只含池里真正的卷。
func TestSpaceUsageIsPoolRelative(t *testing.T) {
	runner := &fakeRunner{spaceOutput: "" +
		"tank\tfilesystem\t-\t1000\t9000\n" +
		"tank/nd/imports\tfilesystem\t-\t300\t9000\n" +
		"tank/nd/win11\tvolume\t500\t400\t9000\n" +
		"tank/nd/win11_default\tvolume\t500\t8\t9000\n" +
		"other/x\tvolume\t50\t50\t100\n"}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	usage, err := agent.SpaceUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := storage.SpaceUsage{PoolUsed: 1000, PoolAvailable: 9000, Volumes: map[string]storage.VolumeUsage{
		"win11":         {Size: 500, Used: 400},
		"win11_default": {Size: 500, Used: 8},
	}}
	if !reflect.DeepEqual(usage, want) {
		t.Fatalf("usage = %#v, want %#v", usage, want)
	}
}

func TestAddAndAttachDiskPassThroughToZPool(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err := agent.AddDisk(context.Background(), "tank", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{" /dev/sdc", "/dev/sdd "}}); err != nil {
		t.Fatal(err)
	}
	if err := agent.AttachDisk(context.Background(), "tank", " /dev/sda ", "/dev/sdz"); err != nil {
		t.Fatal(err)
	}
	if err := agent.DetachDisk(context.Background(), "tank", " /dev/sdz "); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"add", "tank", "mirror", "/dev/sdc", "/dev/sdd"},
		{"attach", "tank", "/dev/sda", "/dev/sdz"},
		// detach 先读池状态，找出之后要清标签的分区（此假实现无需清）。
		{"list", "-Hp", "-o", "name,health", "tank"},
		{"detach", "tank", "/dev/sdz"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestSpecialAndSparePassThroughToZPool(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err := agent.AddSpecial(context.Background(), "tank", []string{" /dev/nvme0n1", "/dev/nvme1n1 "}); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveSpecial(context.Background(), "tank", " mirror-3 "); err != nil {
		t.Fatal(err)
	}
	if err := agent.AddSpare(context.Background(), "tank", []string{"/dev/sdz"}); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveSpare(context.Background(), "tank", "/dev/sdz"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"add", "tank", "special", "mirror", "/dev/nvme0n1", "/dev/nvme1n1"},
		{"remove", "tank", "mirror-3"},
		{"add", "tank", "spare", "/dev/sdz"},
		{"set", "autoreplace=on", "tank"},
		{"list", "-Hp", "-o", "name,health", "tank"},
		{"remove", "tank", "/dev/sdz"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
	if err := agent.AddSpecial(context.Background(), "tank", []string{"/dev/nvme0n1"}); err == nil {
		t.Fatal("unmirrored special accepted")
	}
}

// 只有 raidz 池才查询能否逐盘扩展，mirror 池每次刷新不能多跑两条命令。
func TestPoolStatusAsksRaidzExpansionOnlyForRaidz(t *testing.T) {
	raidz := `  pool: tank2
 state: ONLINE
config:

	NAME          STATE     READ WRITE CKSUM
	tank2         ONLINE       0     0     0
	  raidz1-0    ONLINE       0     0     0
	    /dev/sdb  ONLINE       0     0     0
	    /dev/sdc  ONLINE       0     0     0
	    /dev/sdd  ONLINE       0     0     0

errors: No known data errors
`
	runner := &fakeRunner{listOutput: "tank2\tONLINE\n", poolSpaceOutput: "250\t750\n", poolStatusOutput: raidz, versionOutput: "zfs-2.2.2-0ubuntu9.4\nzfs-kmod-2.3.4-1ubuntu2\n"}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	status, err := agent.PoolStatus(context.Background(), "tank2")
	if err != nil {
		t.Fatal(err)
	}
	if status.Layout != domain.PoolLayoutRaidz1 || status.RaidzExpandable || !strings.Contains(status.RaidzExpandNote, "2.2.2") {
		t.Fatalf("status = %#v", status)
	}
	if !reflect.DeepEqual(runner.args()[3], []string{"version"}) {
		t.Fatalf("args = %#v, want zpool version asked after status", runner.args())
	}

	mirror := strings.NewReplacer("raidz1-0", "mirror-0").Replace(raidz)
	runner = &fakeRunner{listOutput: "tank2\tONLINE\n", poolSpaceOutput: "250\t750\n", poolStatusOutput: mirror}
	agent = New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if _, err := agent.PoolStatus(context.Background(), "tank2"); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.args() {
		if call[0] == "version" || call[0] == "get" {
			t.Fatalf("mirror pool asked about raidz expansion: %#v", runner.args())
		}
	}
}

// 立即离池的盘（detach、移除 cache）在 zpool 建的分区上仍有 ZFS 标签，不清就显示占用。
// 操作者说的是 /dev/sdc，标签却在 /dev/sdc1 上。
func TestDetachClearsTheLabelOnThePartition(t *testing.T) {
	runner := &fakeRunner{listOutput: "tank\tONLINE\n", poolSpaceOutput: "1\t9\n", poolStatusOutput: `  pool: tank
 state: ONLINE
config:

	NAME           STATE     READ WRITE CKSUM
	tank           ONLINE       0     0     0
	  mirror-0     ONLINE       0     0     0
	    /dev/sda1  ONLINE       0     0     0
	    /dev/sdc1  ONLINE       0     0     0

errors: No known data errors
`}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err := agent.DetachDisk(context.Background(), "tank", "/dev/sdc"); err != nil {
		t.Fatal(err)
	}
	args := runner.args()
	last := args[len(args)-2:]
	want := [][]string{{"detach", "tank", "/dev/sdc"}, {"labelclear", "-f", "/dev/sdc1"}}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("tail of calls = %#v, want %#v (all: %#v)", last, want, args)
	}
}

func TestPoolOperationsUseZPool(t *testing.T) {
	runner := &fakeRunner{listOutput: "tank2\tONLINE\n", poolSpaceOutput: "250\t750\n", poolStatusOutput: `  pool: tank2
 state: ONLINE
config:

	NAME          STATE     READ WRITE CKSUM
	tank2         ONLINE       0     0     0
	  mirror-0    ONLINE       0     0     0
	    /dev/sdb  ONLINE       0     0     0
	    /dev/sdc  ONLINE       0     0     0

errors: No known data errors
`}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	pool, err := agent.CreatePool(context.Background(), "tank2", storage.PoolSpec{Layout: domain.PoolLayoutMirror, GroupWidth: 2, Disks: []string{" /dev/sdb ", "/dev/sdc"}})
	if err != nil {
		t.Fatal(err)
	}
	if pool.ID != storage.PoolID("server-a", "tank2") || pool.ServerID != "server-a" || !reflect.DeepEqual(pool.Disks, []string{"/dev/sdb", "/dev/sdc"}) {
		t.Fatalf("pool = %#v", pool)
	}
	if pool.Layout != domain.PoolLayoutMirror || pool.GroupWidth != 2 {
		t.Fatalf("pool layout = %q/%d, want the spec it was created with", pool.Layout, pool.GroupWidth)
	}
	status, err := agent.PoolStatus(context.Background(), "tank2")
	if err != nil {
		t.Fatal(err)
	}
	if status.PoolID != storage.PoolID("server-a", "tank2") || status.Health != "ONLINE" || status.Capacity != 1000 || status.Used != 250 {
		t.Fatalf("status = %#v", status)
	}
	// 池报告的布局随状态一起经接口返回。
	if status.Layout != domain.PoolLayoutMirror || status.GroupWidth != 2 || len(status.Vdevs) != 1 || status.Vdevs[0].Name != "mirror-0" {
		t.Fatalf("status layout = %#v", status)
	}
	if err := agent.DestroyPool(context.Background(), "tank2"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "mirror", "/dev/sdb", "/dev/sdc"},
		{"list", "-Hp", "-o", "name,health", "tank2"},
		{"list", "-Hp", "-o", "used,available", "tank2"},
		{"status", "-P", "tank2"},
		// 销毁前先读成员盘，之后才能清标签，否则这些盘无法再用。
		{"list", "-Hp", "-o", "name,health", "tank2"},
		{"list", "-Hp", "-o", "used,available", "tank2"},
		{"status", "-P", "tank2"},
		{"destroy", "tank2"},
		{"labelclear", "-f", "/dev/sdb"},
		{"labelclear", "-f", "/dev/sdc"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestPoolDiskOperationsTrimAndForwardToZPool(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.AddDisk(context.Background(), " tank2 ", storage.PoolSpec{Disks: []string{" /dev/sdb ", "/dev/sdc "}}); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveDisk(context.Background(), " tank2 ", " /dev/sdc "); err != nil {
		t.Fatal(err)
	}
	if err := agent.ReplaceDisk(context.Background(), " tank2 ", " /dev/sdc ", "/dev/sdd "); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"add", "tank2", "/dev/sdb", "/dev/sdc"},
		{"remove", "tank2", "/dev/sdc"},
		{"replace", "tank2", "/dev/sdc", "/dev/sdd"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestPoolReadCacheOperationsTrimAndForwardToZPool(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.AddReadCache(context.Background(), " tank2 ", []string{" /dev/sdb ", "/dev/sdc "}); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveReadCache(context.Background(), " tank2 ", " /dev/sdb "); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"add", "tank2", "cache", "/dev/sdb", "/dev/sdc"},
		{"list", "-Hp", "-o", "name,health", "tank2"},
		{"remove", "tank2", "/dev/sdb"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestPoolWriteCacheOperationsTrimAndForwardToZPool(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.AddWriteCache(context.Background(), " tank2 ", []string{" /dev/sdb ", "/dev/sdc "}); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveWriteCache(context.Background(), " tank2 ", " /dev/sdb "); err != nil {
		t.Fatal(err)
	}
	if err := agent.FlushWriteCache(context.Background(), " tank2 "); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"add", "tank2", "log", "/dev/sdb", "/dev/sdc"},
		{"list", "-Hp", "-o", "name,health", "tank2"},
		{"remove", "tank2", "/dev/sdb"},
		{"sync", "tank2"},
	}
	if !reflect.DeepEqual(runner.args(), want) {
		t.Fatalf("args = %#v, want %#v", runner.args(), want)
	}
}

func TestPoolDiskOperationsRejectsInvalidArgs(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.AddDisk(context.Background(), "", storage.PoolSpec{Disks: []string{"/dev/sdb"}}); err == nil {
		t.Fatal("add succeeded")
	}
	if err := agent.AddDisk(context.Background(), "tank", storage.PoolSpec{Disks: []string{"  "}}); err == nil {
		t.Fatal("add succeeded")
	}
	if err := agent.AttachDisk(context.Background(), "tank", " ", "/dev/sdz"); err == nil {
		t.Fatal("attach succeeded")
	}
	if err := agent.RemoveDisk(context.Background(), "tank", " "); err == nil {
		t.Fatal("remove succeeded")
	}
	if err := agent.ReplaceDisk(context.Background(), "tank", " /dev/sda ", " "); err == nil {
		t.Fatal("replace succeeded")
	}
	if err := agent.AddReadCache(context.Background(), "", []string{"/dev/sdb"}); err == nil {
		t.Fatal("add read cache succeeded")
	}
	if err := agent.AddReadCache(context.Background(), "tank", []string{"   "}); err == nil {
		t.Fatal("add read cache succeeded")
	}
	if err := agent.RemoveReadCache(context.Background(), "tank", " "); err == nil {
		t.Fatal("remove read cache succeeded")
	}
	if err := agent.AddWriteCache(context.Background(), "", []string{"/dev/sdb"}); err == nil {
		t.Fatal("add write cache succeeded")
	}
	if err := agent.AddWriteCache(context.Background(), "tank", []string{"   "}); err == nil {
		t.Fatal("add write cache succeeded")
	}
	if err := agent.RemoveWriteCache(context.Background(), "tank", " "); err == nil {
		t.Fatal("remove write cache succeeded")
	}
	if err := agent.FlushWriteCache(context.Background(), " "); err == nil {
		t.Fatal("flush write cache succeeded")
	}
	if len(runner.args()) != 0 {
		t.Fatalf("args = %#v", runner.args())
	}
}

func TestListDisksParsesLsblkJSON(t *testing.T) {
	runner := &fakeRunner{
		lsblkOutput: `{
			"blockdevices": [
				{
					"name": "sda",
					"path": "/dev/sda",
					"type": "disk",
					"size": 2684354560,
					"model": "WDC GREEN",
					"serial": "SDA-001",
					"fstype": "",
					"mountpoint": "",
					"children": [
						{"name":"sda1","path":"/dev/sda1","type":"part","size":1048576,"fstype":"ext4","mountpoint":"/boot"},
						{"name":"sda2","path":"/dev/sda2","type":"part","size":1048576,"fstype":"ext4","mountpoint":"/"},
						{"name":"sda3","path":"/dev/sda3","type":"part","size":2683305984,"fstype":"swap","mountpoint":""}
					]
				},
				{
					"name": "sdb",
					"path": "/dev/sdb",
					"type": "disk",
					"size": 1073741824,
					"model": "Intel SATA",
					"serial": "SDB-002",
					"fstype": "zfs_member",
					"mountpoint": "",
					"children": [
						{"name":"sdb1","path":"/dev/sdb1","type":"part","size":1073741824,"fstype":"zfs_member","mountpoint":""}
					]
				},
				{
					"name": "sdc",
					"path": "/dev/sdc",
					"type": "disk",
					"size": 536870912,
					"model": "WD BLUE",
					"serial": "SDC-003",
					"fstype": "",
					"mountpoint": "",
					"children": [
						{"name":"sdc1","path":"/dev/sdc1","type":"part","size":536870912,"fstype":"ext4","mountpoint":""}
					]
				},
				{
					"name": "sr0",
					"path": "/dev/sr0",
					"type": "rom",
					"size": 1000,
					"model": "DVD-RAM",
					"serial": "OPTICAL"
				}
			]
		}`,
	}
	agent := New("server-a", zfs.New("tank", &fakeRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = runner

	disks, err := agent.ListDisks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 3 {
		t.Fatalf("len(disks) = %d, want 3", len(disks))
	}
	if got := disks[0]; got.Path != "/dev/sda" || got.Name != "sda" || got.Type != "disk" || got.Size != 2684354560 || got.Model != "WDC GREEN" || got.Serial != "SDA-001" || !got.InUse || !strings.Contains(got.UsedBy, "system:/") || !strings.Contains(got.UsedBy, "/boot") {
		t.Fatalf("disks[0] = %#v", got)
	}
	if got := disks[1]; got.Path != "/dev/sdb" || got.Name != "sdb" || got.Type != "disk" || got.Size != 1073741824 || got.Model != "Intel SATA" || got.Serial != "SDB-002" || !got.InUse || !strings.Contains(got.UsedBy, "zfs_member:/dev/sdb") {
		t.Fatalf("disks[1] = %#v", got)
	}
	if got := disks[2]; got.Path != "/dev/sdc" || got.Name != "sdc" || got.Type != "disk" || got.Size != 536870912 || got.Model != "WD BLUE" || got.Serial != "SDC-003" || got.InUse || got.UsedBy != "" {
		t.Fatalf("disks[2] = %#v", got)
	}
	if !reflect.DeepEqual(runner.args(), [][]string{{
		"-J", "-b", "-o", "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,MOUNTPOINT,FSTYPE",
	}}) {
		t.Fatalf("args = %#v", runner.args())
	}
}

type fakeRunner struct {
	calls            []fakeCall
	errAt            int
	err              error
	listOutput       string
	originOutput     string
	lsblkOutput      string
	poolStatusOutput string
	poolSpaceOutput  string // zfs list -o used,available <pool>
	spaceOutput      string // zfs list -r -t filesystem,volume -o name,type,volsize,used,available
	volsizeOutput    string // zfs get volsize
	sendSizeOutput   string // zfs send -nP
	versionOutput    string // zpool version
	recordIO         bool
}

type fakeCall struct {
	name string
	args []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
	if r.errAt == len(r.calls) {
		return nil, r.err
	}
	if name == "lsblk" {
		return []byte(r.lsblkOutput), nil
	}
	if len(args) >= 6 && args[0] == "list" && args[5] == "name,origin" {
		return []byte(r.originOutput), nil
	}
	if len(args) >= 4 && args[0] == "list" && args[3] == "used,available" {
		return []byte(r.poolSpaceOutput), nil
	}
	if len(args) >= 7 && args[0] == "list" && args[6] == "name,type,volsize,used,available" {
		return []byte(r.spaceOutput), nil
	}
	if len(args) > 0 && args[0] == "version" {
		return []byte(r.versionOutput), nil
	}
	if len(args) >= 2 && args[0] == "send" && args[1] == "-nP" {
		return []byte(r.sendSizeOutput), nil
	}
	if len(args) > 0 && args[0] == "get" {
		return []byte(r.volsizeOutput), nil
	}
	if len(args) > 0 && args[0] == "status" {
		return []byte(r.poolStatusOutput), nil
	}
	if len(args) > 0 && args[0] == "list" {
		return []byte(r.listOutput), nil
	}
	return nil, nil
}

func (r *fakeRunner) RunWithIO(_ context.Context, name string, _ io.Reader, _ io.Writer, args ...string) error {
	if r.recordIO {
		r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
		if r.errAt == len(r.calls) {
			return r.err
		}
	}
	return nil
}

func (r *fakeRunner) args() [][]string {
	out := make([][]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, call.args)
	}
	return out
}

type fakeExporter struct {
	reqs          []iscsi.ExportRequest
	tornTarget    string
	tornLUNs      []int
	deletedTarget string
	deletedLUNs   []int
	sessions      []string
}

func (e *fakeExporter) ActiveSessions(_ context.Context) ([]string, error) {
	return e.sessions, nil
}

func (e *fakeExporter) ExportBlocks(_ context.Context, reqs []iscsi.ExportRequest) ([]storage.LUN, error) {
	e.reqs = append(e.reqs, reqs...)
	luns := make([]storage.LUN, 0, len(reqs))
	for _, req := range reqs {
		luns = append(luns, storage.LUN{Target: "target-a", LUN: req.LUN, VolPath: req.VolPath})
	}
	return luns, nil
}

func (e *fakeExporter) TeardownTarget(_ context.Context, target string, luns []int) error {
	e.tornTarget = target
	e.tornLUNs = append(e.tornLUNs, luns...)
	return nil
}

func (e *fakeExporter) DeleteTarget(_ context.Context, target string) error {
	e.deletedTarget = target
	return nil
}

func (e *fakeExporter) DeleteLUNs(_ context.Context, _ string, luns []int) error {
	e.deletedLUNs = append(e.deletedLUNs, luns...)
	return nil
}

func TestCreateClientLUNReleasesLeftoverInjectionMount(t *testing.T) {
	// 泄漏的注入挂载会占住 zvol，导致之后每次开机拆除时 `zfs destroy` 都报
	// "dataset is busy"，客户机卡死直到手工卸载。
	zfsRunner := &fakeRunner{listOutput: "tank/run/CLIENT-AABBCCDDEEFF\n"}
	umounts := &umountRunner{}
	agent := New("server-a", zfs.New("tank", zfsRunner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = umounts
	agent.zvolMountsFn = func() []zfs.ZvolMount {
		return []zfs.ZvolMount{{
			Device:     "/dev/zd2800p3",
			Mountpoint: "/tmp/ndiskless-vol-1",
			Dataset:    "tank/run/CLIENT-AABBCCDDEEFF",
		}}
	}

	if _, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{
		MAC:    "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{ConfigID: "cfg-default", SnapshotName: "red-0"},
	}); err != nil {
		t.Fatal(err)
	}

	want := [][]string{{"umount", "/tmp/ndiskless-vol-1"}}
	if !reflect.DeepEqual(umounts.calls, want) {
		t.Fatalf("umount calls = %#v, want %#v", umounts.calls, want)
	}
	// 挂载释放后克隆被销毁，不残留。
	if !reflect.DeepEqual(zfsRunner.args()[1], []string{"destroy", "-r", "tank/run/CLIENT-AABBCCDDEEFF"}) {
		t.Fatalf("zfs args = %#v", zfsRunner.args())
	}
}

func TestCleanupClientClonesToleratesTransientBusy(t *testing.T) {
	// 内核异步释放 zvol，清理时首次 destroy 可能失败，必须重试而不是报错。
	zfsFake := poolWith("tank/run/CLIENT-AABBCCDDEEFF")
	// 先短暂失败一次，之后 zvol 释放。
	zfsFake.destroyErrs["tank/run/CLIENT-AABBCCDDEEFF"] = []error{storage.CommandError{
		Name:   "zfs",
		Args:   []string{"destroy", "-r", "tank/run/CLIENT-AABBCCDDEEFF"},
		Output: "cannot destroy 'tank/run/CLIENT-AABBCCDDEEFF': dataset is busy",
		Err:    errors.New("exit status 1"),
	}}
	agent := New("server-a", zfsFake)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	if err := agent.CleanupClientClones(context.Background(), "aa:bb:cc:dd:ee:ff", nil); err != nil {
		t.Fatalf("transient busy not retried: %v", err)
	}
	if len(zfsFake.destroyAttempts) != 2 || len(zfsFake.destroyed) != 1 {
		t.Fatalf("attempts = %#v destroyed = %#v", zfsFake.destroyAttempts, zfsFake.destroyed)
	}
}

// ZFS 数据集名不接受非 ASCII，中文命名的配置仍须得到可用数据集：只把 ID 转成 ASCII，
// 显示名不变。
func TestCreateConfigAcceptsNonASCIINames(t *testing.T) {
	runner := &fakeRunner{}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	result, err := agent.CreateConfig(context.Background(), "img-win", "美术教室", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Name != "美术教室" {
		t.Fatalf("display name = %q, want it kept", result.Config.Name)
	}
	for _, r := range result.Config.ID {
		if r > 127 {
			t.Fatalf("dataset id %q must stay ASCII", result.Config.ID)
		}
	}
}

// lsblk 把 zvol 报成 TYPE=disk，不能列为 `zpool create` 候选，否则等于允许覆盖其中的
// 镜像、配置或运行中的克隆。
func TestListDisksHidesTheVolumesThePoolItselfHandsOut(t *testing.T) {
	runner := &fakeRunner{
		lsblkOutput: `{
			"blockdevices": [
				{"name":"sda","path":"/dev/sda","type":"disk","size":2684354560,"model":"WDC","serial":"S1","fstype":"","mountpoint":""},
				{"name":"zd96","path":"/dev/zd96","type":"disk","size":85899345920,"model":null,"serial":null,"fstype":"","mountpoint":""},
				{"name":"zd112","path":"/dev/zd112","type":"disk","size":85899345920,"model":null,"serial":null,"fstype":"","mountpoint":"",
				 "children":[{"name":"zd112p3","path":"/dev/zd112p3","type":"part","size":85899345920,"fstype":"ntfs","mountpoint":""}]},
				{"name":"nbd7","path":"/dev/nbd7","type":"disk","size":0,"model":null,"serial":null,"fstype":"","mountpoint":""}
			]
		}`,
	}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	agent.runner = runner

	disks, err := agent.ListDisks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if strings.HasPrefix(d.Name, "zd") {
			t.Fatalf("zvol %s offered as a candidate disk: %#v", d.Name, disks)
		}
	}
	// 零字节的"盘"（未连接的 nbd、空读卡器）无法建池，不列出。
	if len(disks) != 1 || disks[0].Name != "sda" {
		t.Fatalf("disks = %#v, want just the real one", disks)
	}
}

// 销毁池后盘要能再用。`zpool destroy` 会在成员盘上留下 ZFS 标签，不清就一直显示占用。
func TestDestroyPoolClearsTheLabelsSoTheDisksComeBack(t *testing.T) {
	runner := &fakeRunner{listOutput: "e2epool\tONLINE\n", poolSpaceOutput: "100000\t1073641824\n", poolStatusOutput: `  pool: e2epool
 state: ONLINE
config:

	NAME        STATE     READ WRITE CKSUM
	e2epool     ONLINE       0     0     0
	  /dev/sda  ONLINE       0     0     0
	  /dev/sdb  ONLINE       0     0     0

errors: No known data errors
`}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.DestroyPool(context.Background(), "e2epool"); err != nil {
		t.Fatal(err)
	}
	var destroyed bool
	cleared := map[string]bool{}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[0] == "destroy" && call.args[1] == "e2epool" {
			destroyed = true
		}
		if len(call.args) >= 2 && call.args[0] == "labelclear" {
			if !destroyed {
				t.Fatal("labels cleared before the pool was destroyed")
			}
			cleared[call.args[len(call.args)-1]] = true
		}
	}
	if !destroyed {
		t.Fatalf("pool never destroyed: %#v", runner.args())
	}
	for _, disk := range []string{"/dev/sda", "/dev/sdb"} {
		if !cleared[disk] {
			t.Fatalf("label not cleared on %s: %#v", disk, runner.args())
		}
	}
}

// 本产品的压缩导出名为 .zfs.gz，操作者常简写为 .gz：裸 .gz 要交给 zfs receive，
// 不能被拒绝，也不能被当成磁盘镜像。
func TestValidateImportFilePathAcceptsGz(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"win10.zfs.gz", "win10.gz", "win10.gzip", "win10.zfs"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("stream"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := validateImportFilePath(path, dir); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
		if isDiskImageFile(strings.ToLower(name)) {
			t.Errorf("%s routed to the disk-image converter, not zfs receive", name)
		}
	}
}

// gzip 压缩的磁盘镜像 qemu-img 读不了且不报错，会导入一个看似正常的空镜像，必须明确拒绝。
func TestValidateImportFilePathRefusesGzippedDiskImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "win10.vmdk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("disk image payload")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = validateImportFilePath(path, dir)
	if err == nil {
		t.Fatal("a gzipped disk image was accepted; it would import as a few kilobytes of garbage")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
}

// 备份池由推导得出：一台机器最多两个池，非数据池的那个就是备份池，无需手填池名。
func TestBackupPoolNameIsDerivedNotConfigured(t *testing.T) {
	ctx := context.Background()
	pool := newFakePool()
	pool.poolNames = []string{"tank", "backup"}
	agent := New("srv-1", pool)
	got, err := agent.BackupPoolName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "backup" {
		t.Fatalf("backup pool = %q，想要 backup", got)
	}

	// 只有数据池时没有备份池，这是「还没配」，不是错误。
	pool.poolNames = []string{"tank"}
	got, err = agent.BackupPoolName(ctx)
	if err != nil || got != "" {
		t.Fatalf("单池时 = %q err=%v，想要空", got, err)
	}
}

// 备机不能自己打备份快照：复制用 recv -F，备机自建的标记下一轮就被抹掉。快照由写入
// 节点建、随复制下发，各节点推送到本机备份池时复用已有快照，不再新打。
func TestBackupCanReuseAnExistingSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newFakePool()
	pool.poolNames = []string{"tank", "backup"}
	pool.guids["tank/nd"] = nil // 容器在，还没有共同快照
	agent := New("srv-1", pool)

	if _, err := agent.Backup(ctx, storage.BackupReq{
		BackupPool: "backup", Snapshot: "ndbackup-1", Reuse: true,
	}); err != nil {
		t.Fatal(err)
	}
	if n := pool.recursiveSnaps["tank/nd@ndbackup-1"]; n != 0 {
		t.Fatalf("复用模式下不该再打快照，却打了 %d 次", n)
	}
	if len(pool.prunes) != 0 {
		t.Fatalf("复用模式下不该修剪源端快照（那是写入节点的事）：%#v", pool.prunes)
	}
	if len(pool.replicates) == 0 {
		t.Fatal("没有把副本推到备份池")
	}
}
