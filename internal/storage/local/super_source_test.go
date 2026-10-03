package local

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// seedForeignSuperPool 在 seedSuperPool 之外再放一个兄弟配置 cfgB；超管机的克隆仍来自 cfg。
func seedForeignSuperPool() *fakePool {
	z := seedSuperPool()
	z.datasets = append(z.datasets, "tank/nd/cfgB")
	z.origin["tank/nd/cfgB"] = "tank/nd/img@0"
	z.snaps["tank/nd/cfgB@0"] = true
	return z
}

func noOpStartingWith(t *testing.T, ops []string, prefix string) {
	t.Helper()
	for _, op := range ops {
		if strings.HasPrefix(op, prefix) {
			t.Fatalf("op %q must not run: ops=%v", op, ops)
		}
	}
}

// 超管机换了分组（或分组换了配置）后开机，同名持久克隆装的是别的配置的盘和未保存的修改：
// 不能把它当成新配置的盘挂给机器，要拒绝并点名机器和两边的配置。
func TestSuperStartRefusesAPersistentCloneOfAnotherConfig(t *testing.T) {
	z := seedForeignSuperPool()
	exporter := &fakeExporter{}
	agent := New("server-a", z, exporter)

	_, err := agent.SuperStart(context.Background(), storage.ClientReq{
		MAC:    "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{ConfigID: "cfgB", SnapshotName: "0"},
	})
	var foreign SuperCloneForeign
	if !errors.As(err, &foreign) {
		t.Fatalf("err = %v, want SuperCloneForeign", err)
	}
	for _, want := range []string{"AA:BB:CC:DD:EE:FF", "cfgB", "cfg", "系统盘"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q must name %q", err.Error(), want)
		}
	}
	if !z.present("tank/run/SCLIENT-AABBCCDDEEFF") || z.origin["tank/run/SCLIENT-AABBCCDDEEFF"] != "tank/nd/cfg@0" {
		t.Fatalf("the unsaved clone must be left untouched: %v", z.state())
	}
	if len(exporter.reqs) != 0 {
		t.Fatalf("nothing may be exported: %#v", exporter.reqs)
	}
}

// 数据盘增删会让 LUN 下标错位：SCLIENT-…-DATA-1 原本是 dcfg 的盘，现在 LUN 1 该是 dcfg2。
func TestSuperStartRefusesADataDiskCloneOfAnotherConfig(t *testing.T) {
	z := seedSuperPoolWithData()
	z.datasets = append(z.datasets, "tank/nd/dcfg2")
	z.origin["tank/nd/dcfg2"] = "tank/nd/dimg@0"
	z.snaps["tank/nd/dcfg2@0"] = true
	agent := New("server-a", z, &fakeExporter{})

	_, err := agent.SuperStart(context.Background(), storage.ClientReq{
		MAC:       "aa:bb:cc:dd:ee:ff",
		System:    storage.ClientSource{ConfigID: "cfg", SnapshotName: "0"},
		DataDisks: []storage.ClientSource{{LUN: 1, ConfigID: "dcfg2", SnapshotName: "0"}},
	})
	var foreign SuperCloneForeign
	if !errors.As(err, &foreign) || foreign.LUN != 1 || foreign.Have != "dcfg" || foreign.Want != "dcfg2" {
		t.Fatalf("err = %v (%#v), want data disk lun 1 refused", err, foreign)
	}
	if !strings.Contains(err.Error(), "数据盘") {
		t.Fatalf("message %q must say which disk", err.Error())
	}
}

// 同一配置换了当前还原点不算来源变化：复用持久克隆，基点过时由保存时的检查负责。
func TestSuperStartReusesItsOwnCloneWhenTheRestorePointMoved(t *testing.T) {
	z := seedSuperPool()
	z.snaps["tank/nd/cfg@v2"] = true
	agent := New("server-a", z, &fakeExporter{})

	if _, err := agent.SuperStart(context.Background(), storage.ClientReq{
		MAC:    "aa:bb:cc:dd:ee:ff",
		System: storage.ClientSource{ConfigID: "cfg", SnapshotName: "v2"},
	}); err != nil {
		t.Fatal(err)
	}
	noOpStartingWith(t, z.ops, "clone ")
	noOpStartingWith(t, z.ops, "destroy ")
}

// 保存到别的配置会 promote→改名→destroy -r，把 cfgB 换成 cfg 的盘并删掉 cfgB 的历史；
// 必须在 promote 之前拒绝。
func TestSuperStopRefusesToSaveACloneOfAnotherConfig(t *testing.T) {
	z := seedForeignSuperPool()
	agent := New("server-a", z, &fakeExporter{})

	_, err := agent.SuperStop(context.Background(), storage.SuperStopReq{MAC: "AA:BB:CC:DD:EE:FF", ConfigID: "cfgB", ReductionName: "@v1"})
	var foreign SuperCloneForeign
	if !errors.As(err, &foreign) {
		t.Fatalf("err = %v, want SuperCloneForeign", err)
	}
	for _, want := range []string{"AA:BB:CC:DD:EE:FF", "cfgB", "cfg"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q must name %q", err.Error(), want)
		}
	}
	noOpStartingWith(t, z.ops, "promote ")
	noOpStartingWith(t, z.ops, "rename ")
	noOpStartingWith(t, z.ops, "destroy ")
	if !z.present("tank/nd/cfgB") || !z.snaps["tank/nd/cfgB@0"] || !z.present("tank/run/SCLIENT-AABBCCDDEEFF") {
		t.Fatalf("pool must be untouched: %v", z.state())
	}
}

// 数据盘下标错位时保存同样拒绝，系统盘也不能先存进去（整次关机要么都对，要么都不动）。
func TestSuperStopRefusesAMisalignedDataDisk(t *testing.T) {
	z := seedSuperPoolWithData()
	z.datasets = append(z.datasets, "tank/nd/dcfg2")
	z.origin["tank/nd/dcfg2"] = "tank/nd/dimg@0"
	z.snaps["tank/nd/dcfg2@0"] = true
	agent := New("server-a", z, &fakeExporter{})
	req := superStopReq()
	req.DataDisks = []storage.SuperStopDisk{{LUN: 1, ConfigID: "dcfg2", ReductionName: "@games"}}

	_, err := agent.SuperStop(context.Background(), req)
	var foreign SuperCloneForeign
	if !errors.As(err, &foreign) || foreign.LUN != 1 {
		t.Fatalf("err = %v, want data disk lun 1 refused", err)
	}
	noOpStartingWith(t, z.ops, "promote ")
	if !z.snaps["tank/nd/dcfg2@0"] || !z.present("tank/run/SCLIENT-AABBCCDDEEFF-DATA-1") {
		t.Fatalf("pool must be untouched: %v", z.state())
	}
}

// 开机前离线适配同样只能往本配置的克隆里写。
func TestPrepareSuperAdaptationRefusesACloneOfAnotherConfig(t *testing.T) {
	z := seedForeignSuperPool()
	agent := New("server-a", z)

	err := agent.PrepareSuperAdaptation(context.Background(), storage.SuperAdaptationReq{
		MAC:         "aa:bb:cc:dd:ee:ff",
		Source:      storage.ClientSource{ConfigID: "cfgB", SnapshotName: "0"},
		AdaptScript: []byte("adapt"),
	})
	var foreign SuperCloneForeign
	if !errors.As(err, &foreign) {
		t.Fatalf("err = %v, want SuperCloneForeign", err)
	}
}
