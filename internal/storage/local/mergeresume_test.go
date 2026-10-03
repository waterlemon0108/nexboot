package local

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// seedMergePool 构造合并的起点：带 @0 的镜像、从它克隆且有自身历史的保留配置、一个兄弟配置。
func seedMergePool() *fakePool {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/cfg-keep", "tank/nd/cfg-drop"}
	z.snaps = map[string]bool{
		"tank/nd/img@0":       true,
		"tank/nd/cfg-keep@0":  true,
		"tank/nd/cfg-keep@r1": true,
		"tank/nd/cfg-drop@0":  true,
	}
	z.origin = map[string]string{
		"tank/nd/cfg-keep": "tank/nd/img@0",
		"tank/nd/cfg-drop": "tank/nd/img@0",
	}
	return z
}

func mergeReq() storage.MergeConfigReq {
	return storage.MergeConfigReq{
		ImageID:         "img",
		ConfigID:        "cfg-keep",
		DeleteConfigIDs: []string{"cfg-drop"},
		ReductionNames:  []string{"@0", "@r1"},
	}
}

// 合并的终态与新导入的镜像形状相同；每一步中断后重跑都能到达终态。
var wantMerged = []string{"tank/nd/cfg-keep", "tank/nd/cfg-keep@0", "tank/nd/img", "tank/nd/img@0"}

func TestMergeConfigReachesTheSameEndStateWhenResumed(t *testing.T) {
	ctx := context.Background()
	for _, stopAt := range []string{
		"destroy tank/nd/cfg-drop",
		"destroy-snapshot tank/nd/cfg-keep@r1",
		"promote tank/nd/cfg-keep",
		"destroy tank/nd/img",
		"rename tank/nd/cfg-keep",
		"snapshot tank/nd/img@0",
		"clone tank/nd/cfg-keep",
		"snapshot tank/nd/cfg-keep@0",
	} {
		t.Run(stopAt, func(t *testing.T) {
			z := seedMergePool()
			z.failOn = stopAt
			agent := New("server-a", z)

			if _, err := agent.MergeConfig(ctx, mergeReq()); err == nil {
				t.Fatalf("merge should have stopped at %q", stopAt)
			}
			// 模拟操作者重试失败的任务。
			z.failOn = ""
			if _, err := agent.MergeConfig(ctx, mergeReq()); err != nil {
				t.Fatalf("retry after stopping at %q must recover: %v", stopAt, err)
			}
			if got := z.state(); !equalStrings(got, wantMerged) {
				t.Fatalf("after resume state = %#v, want %#v", got, wantMerged)
			}
		})
	}
}

func TestMergeConfigIsIdempotentOnceComplete(t *testing.T) {
	ctx := context.Background()
	z := seedMergePool()
	agent := New("server-a", z)
	if _, err := agent.MergeConfig(ctx, mergeReq()); err != nil {
		t.Fatal(err)
	}
	if got := z.state(); !equalStrings(got, wantMerged) {
		t.Fatalf("state = %#v", got)
	}
	// 重复提交同一次合并不能破坏结果。
	if _, err := agent.MergeConfig(ctx, mergeReq()); err != nil {
		t.Fatalf("re-running a finished merge: %v", err)
	}
	if got := z.state(); !equalStrings(got, wantMerged) {
		t.Fatalf("state after re-run = %#v, want %#v", got, wantMerged)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 互相派生的配置必须从叶子开始删。调用方按创建顺序传入（父在前），存储层要自己排序，
// 否则删除带派生配置的镜像会报 "volume has dependent clones"。
func TestDeleteImageRemovesForkedConfigsInDependencyOrder(t *testing.T) {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/cfg-a", "tank/nd/cfg-fork"}
	z.snaps = map[string]bool{"tank/nd/img@0": true, "tank/nd/cfg-a@0": true, "tank/nd/cfg-fork@0": true}
	z.origin = map[string]string{
		"tank/nd/cfg-a":    "tank/nd/img@0",
		"tank/nd/cfg-fork": "tank/nd/cfg-a@0", // 从 cfg-a 派生，cfg-a 不能先删
	}
	agent := New("server-a", z)

	if err := agent.DeleteImage(context.Background(), "img", []string{"cfg-a", "cfg-fork"}); err != nil {
		t.Fatal(err)
	}
	if got := z.state(); len(got) != 0 {
		t.Fatalf("leftovers = %#v", got)
	}
}

// 记录可能比数据集活得久（销毁后任务失败、手工清理），数据集不存在时删除仍要成功，否则记录永远删不掉。
func TestDeletingWhatIsAlreadyGoneSucceeds(t *testing.T) {
	ctx := context.Background()
	z := newFakePool()
	agent := New("server-a", z)

	if err := agent.DeleteImage(ctx, "vanished", []string{"vanished_default"}); err != nil {
		t.Fatalf("image: %v", err)
	}
	if err := agent.DeleteConfig(ctx, "vanished_default"); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := agent.DeleteReduction(ctx, "vanished_default", "@0"); err != nil {
		t.Fatalf("reduction: %v", err)
	}
}

// 两个不同中文名会折叠成同一 ASCII id；Agent 要把池中已有名字传给命名规则，第二次导入不能落到第一次的数据集上。
// 折叠本身由 storage.ImageName 的测试覆盖。
func TestImportImageDerivesDistinctASCIIDatasets(t *testing.T) {
	ctx := context.Background()
	agent := New("server-a", newFakePool())
	root, sourcePath := importImageFixture(t, "class.zfs")

	first, err := agent.ImportImage(ctx, storage.ImportImageReq{
		Name: "教学一班", SourcePath: sourcePath, ImportDir: root, OSType: "windows",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.ImportImage(ctx, storage.ImportImageReq{
		Name: "教学二班", SourcePath: sourcePath, ImportDir: root, OSType: "windows",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Image.ID == second.Image.ID {
		t.Fatalf("both imports took the dataset %q", first.Image.ID)
	}
	if first.Config.ID == second.Config.ID {
		t.Fatalf("both imports took the config dataset %q", first.Config.ID)
	}
}

// seedSuperPool 构造超管机关机保存的起点：被编辑的配置和机器一直在写的持久克隆。
func seedSuperPool() *fakePool {
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/cfg", "tank/run/SCLIENT-AABBCCDDEEFF"}
	z.snaps = map[string]bool{"tank/nd/img@0": true, "tank/nd/cfg@0": true}
	z.origin = map[string]string{
		"tank/nd/cfg":                   "tank/nd/img@0",
		"tank/run/SCLIENT-AABBCCDDEEFF": "tank/nd/cfg@0",
	}
	return z
}

func superStopReq() storage.SuperStopReq {
	return storage.SuperStopReq{MAC: "AA:BB:CC:DD:EE:FF", ConfigID: "cfg", ReductionName: "@v1"}
}

// 保存超管机修改是用克隆替换配置的六步破坏性操作；在任意一步中断后，重跑关机都必须能恢复，
// 否则配置会落在 store 不认识的名字下。
func TestSuperStopReachesTheSameEndStateWhenResumed(t *testing.T) {
	ctx := context.Background()
	for _, stopAt := range []string{
		"promote tank/run/SCLIENT-AABBCCDDEEFF",
		"rename tank/nd/cfg",
		"rename tank/run/SCLIENT-AABBCCDDEEFF",
		"destroy tank/nd/cfg_before_super",
		"snapshot tank/nd/cfg@v1",
	} {
		t.Run(stopAt, func(t *testing.T) {
			z := seedSuperPool()
			z.failOn = stopAt
			agent := New("server-a", z)

			if _, err := agent.SuperStop(ctx, superStopReq()); err == nil {
				t.Fatalf("shutdown should have stopped at %q", stopAt)
			}
			z.failOn = ""
			res, err := agent.SuperStop(ctx, superStopReq())
			if err != nil {
				t.Fatalf("retry after stopping at %q must recover: %v", stopAt, err)
			}
			if red := res.System; red.Name != "@v1" || red.ConfigID != "cfg" {
				t.Fatalf("reduction = %#v", red)
			}
			// 修改成为配置，保留继承的历史和新还原点；克隆和被替换的旧配置都已删除。保存不折叠历史，与合并不同。
			want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/cfg@v1", "tank/nd/img", "tank/nd/img@0"}
			if got := z.state(); !equalStrings(got, want) {
				t.Fatalf("after resume state = %#v, want %#v", got, want)
			}
		})
	}
}

// 早先中断的关机留下的旧配置占着本次要用的名字时，必须报错；容忍 "already exists" 会让克隆没换上却报成功，
// 修改被静默丢弃而界面显示有新还原点。
func TestSuperStopSurvivesALeftoverSupersededConfig(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPool()
	z.datasets = append(z.datasets, "tank/nd/cfg_before_super")
	agent := New("server-a", z)

	res, err := agent.SuperStop(ctx, superStopReq())
	if err != nil {
		t.Fatal(err)
	}
	if red := res.System; red.Name != "@v1" {
		t.Fatalf("reduction = %#v", red)
	}
	// 克隆已成为配置，不能遗留在 SCLIENT- 下。
	want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/cfg@v1", "tank/nd/img", "tank/nd/img@0"}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestSuperStopIsIdempotentOnceComplete(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPool()
	agent := New("server-a", z)
	if _, err := agent.SuperStop(ctx, superStopReq()); err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/cfg@v1", "tank/nd/img", "tank/nd/img@0"}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	if _, err := agent.SuperStop(ctx, superStopReq()); err != nil {
		t.Fatalf("re-running a finished shutdown: %v", err)
	}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("state after re-run = %#v, want %#v", got, want)
	}
}

// 合并 promote 后配置成了镜像数据集的父级，先删配置会报 "dependent clones"；删除顺序要从池里推出，不靠调用方。
func TestDeleteImageHandlesAConfigPromotedOverIt(t *testing.T) {
	ctx := context.Background()
	z := newFakePool()
	z.datasets = []string{"tank/nd/img", "tank/nd/cfg"}
	z.snaps = map[string]bool{"tank/nd/cfg@0": true}
	z.origin = map[string]string{"tank/nd/img": "tank/nd/cfg@0"} // 已经 promote 过
	agent := New("server-a", z)

	if err := agent.DeleteImage(ctx, "img", []string{"cfg"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(z.datasets) != 0 {
		t.Fatalf("left behind: %#v", z.datasets)
	}
}

// seedSuperPoolWithData 在 seedSuperPool 基础上加一块数据盘：数据镜像、其配置、以 LUN 1 导出的持久数据克隆。
func seedSuperPoolWithData() *fakePool {
	z := seedSuperPool()
	z.datasets = append(z.datasets, "tank/nd/dimg", "tank/nd/dcfg", "tank/run/SCLIENT-AABBCCDDEEFF-DATA-1")
	z.snaps["tank/nd/dimg@0"] = true
	z.snaps["tank/nd/dcfg@0"] = true
	z.origin["tank/nd/dcfg"] = "tank/nd/dimg@0"
	z.origin["tank/run/SCLIENT-AABBCCDDEEFF-DATA-1"] = "tank/nd/dcfg@0"
	return z
}

func superStopWithDataReq() storage.SuperStopReq {
	req := superStopReq()
	req.DataDisks = []storage.SuperStopDisk{{LUN: 1, ConfigID: "dcfg", ReductionName: "@games"}}
	return req
}

// 数据盘与系统盘保存方式相同：持久克隆替换数据配置，会话成为该配置的还原点。
func TestSuperStopSavesDataDiskAsReductionOfItsConfig(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	agent := New("server-a", z)

	res, err := agent.SuperStop(ctx, superStopWithDataReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.System.Name != "@v1" || res.System.ConfigID != "cfg" {
		t.Fatalf("system reduction = %#v", res.System)
	}
	if len(res.DataDisks) != 1 || res.DataDisks[0].LUN != 1 || res.DataDisks[0].Reduction.ConfigID != "dcfg" || res.DataDisks[0].Reduction.Name != "@games" || res.DataDisks[0].Reduction.ID != "dcfg_games" {
		t.Fatalf("data reductions = %#v", res.DataDisks)
	}
	want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/cfg@v1", "tank/nd/dcfg", "tank/nd/dcfg@0", "tank/nd/dcfg@games", "tank/nd/dimg", "tank/nd/dimg@0", "tank/nd/img", "tank/nd/img@0"}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	wantOps := []string{
		"promote tank/run/SCLIENT-AABBCCDDEEFF",
		"rename tank/nd/cfg",
		"rename tank/run/SCLIENT-AABBCCDDEEFF",
		"snapshot tank/nd/cfg@v1",
		"destroy tank/nd/cfg_before_super",
		"promote tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
		"rename tank/nd/dcfg",
		"rename tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
		"snapshot tank/nd/dcfg@games",
		"destroy tank/nd/dcfg_before_super",
	}
	if !equalStrings(z.ops, wantOps) {
		t.Fatalf("ops = %#v, want %#v", z.ops, wantOps)
	}
}

// 只保存操作者指定的盘；可以丢弃系统盘而保存数据盘，反之亦然。
func TestSuperStopSavesOnlyTheNamedDisks(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	agent := New("server-a", z)

	req := superStopWithDataReq()
	req.ReductionName = "" // 丢弃系统盘的会话
	res, err := agent.SuperStop(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.System.Name != "" || len(res.DataDisks) != 1 {
		t.Fatalf("result = %#v", res)
	}
	want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/dcfg", "tank/nd/dcfg@0", "tank/nd/dcfg@games", "tank/nd/dimg", "tank/nd/dimg@0", "tank/nd/img", "tank/nd/img@0"}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

// 数据盘替换在任意一步中断，重跑关机都到达同一终态，与系统盘保证相同。
func TestSuperStopWithDataDiskReachesTheSameEndStateWhenResumed(t *testing.T) {
	ctx := context.Background()
	for _, stopAt := range []string{
		"snapshot tank/nd/cfg@v1",
		"promote tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
		"rename tank/nd/dcfg",
		"rename tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
		"destroy tank/nd/dcfg_before_super",
		"snapshot tank/nd/dcfg@games",
	} {
		t.Run(stopAt, func(t *testing.T) {
			z := seedSuperPoolWithData()
			z.failOn = stopAt
			agent := New("server-a", z)

			if _, err := agent.SuperStop(ctx, superStopWithDataReq()); err == nil {
				t.Fatalf("shutdown should have stopped at %q", stopAt)
			}
			z.failOn = ""
			res, err := agent.SuperStop(ctx, superStopWithDataReq())
			if err != nil {
				t.Fatalf("retry after stopping at %q must recover: %v", stopAt, err)
			}
			if res.System.Name != "@v1" || len(res.DataDisks) != 1 || res.DataDisks[0].Reduction.Name != "@games" {
				t.Fatalf("result = %#v", res)
			}
			want := []string{"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/cfg@v1", "tank/nd/dcfg", "tank/nd/dcfg@0", "tank/nd/dcfg@games", "tank/nd/dimg", "tank/nd/dimg@0", "tank/nd/img", "tank/nd/img@0"}
			if got := z.state(); !equalStrings(got, want) {
				t.Fatalf("after resume state = %#v, want %#v", got, want)
			}
		})
	}
}

// 指定保存的数据盘必须确实存在；LUN 填错是调用方缺陷，应报错而不是静默跳过。
func TestSuperStopRefusesSavingADataDiskTheMachineDoesNotHold(t *testing.T) {
	z := seedSuperPool()
	agent := New("server-a", z)
	req := superStopReq()
	req.DataDisks = []storage.SuperStopDisk{{LUN: 3, ConfigID: "dcfg", ReductionName: "@games"}}
	_, err := agent.SuperStop(context.Background(), req)
	var missing storage.SuperSessionMissing
	if !errors.As(err, &missing) || missing.LUN != 3 {
		t.Fatalf("err = %v, want SuperSessionMissing for lun 3", err)
	}
}

// 配置上比超管机基线更新的快照会随旧配置被 destroy -r 静默删掉，所以保存前发现就拒绝。
func TestSuperStopRefusesWhenTheConfigHasNewerRestorePoints(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPool()
	z.snaps["tank/nd/cfg@manual"] = true // 超管机开机之后，有人在这个配置上建的还原点
	before := z.state()
	agent := New("server-a", z)

	_, err := agent.SuperStop(ctx, superStopReq())
	if err == nil {
		t.Fatal("配置上有更新的还原点，保存必须拒绝")
	}
	if !strings.Contains(err.Error(), "@manual") {
		t.Fatalf("要说清是哪个还原点挡住了：%v", err)
	}
	if got := z.state(); !equalStrings(got, before) {
		t.Fatalf("被拒绝了却动了池：\n现在 %#v\n原来 %#v", got, before)
	}
}

// 复制标记这类系统快照天天在长，不能把保存挡住。
func TestSuperStopIgnoresSystemSnapshotsWhenJudgingTheBase(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPool()
	z.snaps["tank/nd/cfg@rep-1789"] = true
	agent := New("server-a", z)

	if _, err := agent.SuperStop(ctx, superStopReq()); err != nil {
		t.Fatalf("系统快照不该挡住保存：%v", err)
	}
}
