package local

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// 在线发布数据盘：摘 LUN → 与关机保存相同的替换并打还原点 → 按新还原点重建同名超管盘 → 原样装回 LUN。
// 系统盘（LUN 0）全程不碰。
func TestPublishDataDiskSwapsAndPutsTheDiskBack(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	res, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 1, ConfigID: "dcfg", ReductionName: "@games"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reduction.ConfigID != "dcfg" || res.Reduction.Name != "@games" {
		t.Fatalf("还原点 = %#v", res.Reduction)
	}

	// 配置上有了新还原点，超管机的盘还在、并且已经基于这个新还原点
	want := []string{
		"tank/nd/cfg", "tank/nd/cfg@0", "tank/nd/dcfg", "tank/nd/dcfg@0", "tank/nd/dcfg@games",
		"tank/nd/dimg", "tank/nd/dimg@0", "tank/nd/img", "tank/nd/img@0",
		"tank/run/SCLIENT-AABBCCDDEEFF", "tank/run/SCLIENT-AABBCCDDEEFF-DATA-1",
	}
	if got := z.state(); !equalStrings(got, want) {
		t.Fatalf("发布后池里 = %#v\n想要 %#v", got, want)
	}
	if origin := z.origin["tank/run/SCLIENT-AABBCCDDEEFF-DATA-1"]; origin != "tank/nd/dcfg@games" {
		t.Fatalf("超管机的盘要重新基于刚发布的点，现在基于 %q", origin)
	}

	// 只摘这一个 LUN，再原样加回去；系统盘那条会话不能被碰
	if len(ex.deletedLUNs) != 1 || ex.deletedLUNs[0] != 1 {
		t.Fatalf("摘掉的 LUN = %v，只应摘数据盘这一个", ex.deletedLUNs)
	}
	if ex.tornTarget != "" {
		t.Fatalf("整个 target 被拆了：%s", ex.tornTarget)
	}
	if len(ex.reqs) != 1 || ex.reqs[0].LUN != 1 || !strings.HasSuffix(ex.reqs[0].VolPath, "SCLIENT-AABBCCDDEEFF-DATA-1") {
		t.Fatalf("加回去的 LUN = %#v", ex.reqs)
	}
	if res.LUN.LUN != 1 {
		t.Fatalf("结果里的 LUN = %#v", res.LUN)
	}
}

// 发布被拒（配置上有更新的还原点，保存会删掉它）时，盘必须原样回到机器上。
func TestPublishDataDiskPutsTheDiskBackWhenItRefuses(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	z.snaps["tank/nd/dcfg@manual"] = true
	before := z.state()
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	_, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 1, ConfigID: "dcfg", ReductionName: "@games"})
	if err == nil || !strings.Contains(err.Error(), "@manual") {
		t.Fatalf("应当拒绝并指出是哪个还原点挡住了：%v", err)
	}
	if got := z.state(); !equalStrings(got, before) {
		t.Fatalf("被拒绝了却动了池：\n现在 %#v\n原来 %#v", got, before)
	}
	if len(ex.reqs) != 1 || ex.reqs[0].LUN != 1 {
		t.Fatalf("摘下来的盘没装回去：%#v", ex.reqs)
	}
}

// 系统盘不能在线发布：Windows 运行时拍到的是拔电源状态，会发给整个分组。应拒绝且提示关机保存。
func TestPublishDataDiskRefusesTheSystemDisk(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	_, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 0, ConfigID: "cfg", ReductionName: "@v1"})
	if err == nil || !strings.Contains(err.Error(), "关机") {
		t.Fatalf("要明确拒绝系统盘、并说清楚它得关机保存：%v", err)
	}
	if len(ex.deletedLUNs) != 0 || len(ex.reqs) != 0 {
		t.Fatalf("什么都不该动：删=%v 导出=%#v", ex.deletedLUNs, ex.reqs)
	}
}

// 机器没有这块超管盘（没开过机或盘号填错）时，要在摘 LUN 之前就拒绝。
func TestPublishDataDiskRefusesADiskTheMachineNeverHeld(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	_, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 2, ConfigID: "dcfg", ReductionName: "@games"})
	var missing storage.SuperSessionMissing
	if err == nil || !errors.As(err, &missing) {
		t.Fatalf("应当报「没有这块超管盘」：%v", err)
	}
	if len(ex.deletedLUNs) != 0 {
		t.Fatalf("还没确认就摘了 LUN：%v", ex.deletedLUNs)
	}
}

// 交换在 rename 之后失败时，回滚要重新克隆再导出；只重新导出原路径不够，数据集已被改名成配置，
// 机器会少一块盘。
func TestPublishDataDiskLeavesTheMachineADiskWhenTheSwapFailsHalfway(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	z.failOn = "destroy tank/nd/dcfg_before_super" // 交换的最后一步失败
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	_, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 1, ConfigID: "dcfg", ReductionName: "@games"})
	if err == nil {
		t.Fatal("交换失败了就要报出来")
	}
	if !z.present("tank/run/SCLIENT-AABBCCDDEEFF-DATA-1") {
		t.Fatalf("机器的数据盘没了：%#v", z.state())
	}
	if len(ex.reqs) != 1 || ex.reqs[0].LUN != 1 {
		t.Fatalf("盘没装回机器上：%#v", ex.reqs)
	}
}

// 刚 clone 出的 zvol 被 udev 扫描的几百毫秒里是 busy，改名要容忍这种瞬时占用，否则新盘首次发布必败。
func TestPublishDataDiskRidesOutATransientBusyRename(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	z.renameErrs = map[string][]error{
		"tank/run/SCLIENT-AABBCCDDEEFF-DATA-1": {zfsErr("cannot rename 'tank/run/SCLIENT-AABBCCDDEEFF-DATA-1': dataset is busy")},
	}
	ex := &fakeExporter{}
	agent := New("server-a", z, ex)

	res, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 1, ConfigID: "dcfg", ReductionName: "@games"})
	if err != nil {
		t.Fatalf("一次瞬时占用不该让发布失败：%v", err)
	}
	if res.Reduction.Name != "@games" {
		t.Fatalf("还原点 = %#v", res.Reduction)
	}
}

// 交换最后删除刚改过名的旧配置时，内核可能还占着它几百毫秒，销毁要容忍这种瞬时 busy。
func TestPublishDataDiskRidesOutATransientBusyDestroy(t *testing.T) {
	ctx := context.Background()
	z := seedSuperPoolWithData()
	z.destroyErrs = map[string][]error{
		"tank/nd/dcfg_before_super": {zfsErr("cannot destroy 'tank/nd/dcfg_before_super': dataset is busy")},
	}
	agent := New("server-a", z, &fakeExporter{})

	if _, err := agent.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: "AA:BB:CC:DD:EE:FF", LUN: 1, ConfigID: "dcfg", ReductionName: "@games"}); err != nil {
		t.Fatalf("一次瞬时占用不该让发布失败：%v", err)
	}
	if z.present("tank/nd/dcfg_before_super") {
		t.Fatalf("被替换下来的旧配置应当已经删掉：%#v", z.state())
	}
}

// 删配置可能撞上复制正在 send 的快照（busy），要容忍这种瞬时占用。
func TestDeleteConfigRidesOutATransientBusyDestroy(t *testing.T) {
	ctx := context.Background()
	z := newFakePool()
	z.datasets = []string{"tank/nd/dimg", "tank/nd/dcfg"} // 没有超管盘挂在上面的配置
	z.snaps = map[string]bool{"tank/nd/dimg@0": true, "tank/nd/dcfg@0": true}
	z.origin = map[string]string{"tank/nd/dcfg": "tank/nd/dimg@0"}
	z.destroyErrs = map[string][]error{
		"tank/nd/dcfg": {zfsErr("cannot destroy snapshot tank/nd/dcfg@rep-178: dataset is busy")},
	}
	agent := New("server-a", z, &fakeExporter{})

	if err := agent.DeleteConfig(ctx, "dcfg"); err != nil {
		t.Fatalf("一次瞬时占用不该让删配置失败：%v", err)
	}
	if z.present("tank/nd/dcfg") {
		t.Fatalf("配置应当已经删掉：%#v", z.state())
	}
}

// 每台机器要能报出本机留存的目录副本（告警只在写入者上评估）；-diverged- 与 -rebuilding-
// 要区分，否则正常重建也会天天告警。
func TestPreservedCopiesReportsWhatIsHeldAside(t *testing.T) {
	ctx := context.Background()
	z := newFakePool()
	z.datasets = []string{
		"tank/nd", "tank/nd/cfg",
		"tank/nd-diverged-1700000000000000000", "tank/nd-diverged-1700000000000000000/cfg",
		"tank/nd-rebuilding-1700000000000000001",
	}
	z.snaps = map[string]bool{
		"tank/nd/cfg@0": true,
		"tank/nd-diverged-1700000000000000000/cfg@0":        true,
		"tank/nd-diverged-1700000000000000000/cfg@装完office": true,
		"tank/nd-diverged-1700000000000000000@rep-17":       true, // 记账，不算内容
		"tank/nd-rebuilding-1700000000000000001@rep-18":     true,
	}
	z.used = map[string]int64{
		"tank/nd-diverged-1700000000000000000":   13 << 30,
		"tank/nd-rebuilding-1700000000000000001": 1 << 30,
	}
	agent := New("server-a", z)

	got, err := agent.PreservedCopies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("两份都要报出来：%#v", got)
	}
	var diverged, rebuilding storage.PreservedCopy
	for _, c := range got {
		switch c.Kind {
		case storage.PreservedDiverged:
			diverged = c
		case storage.PreservedRebuilding:
			rebuilding = c
		}
	}
	if diverged.Dataset != "tank/nd-diverged-1700000000000000000" || diverged.Used != 13<<30 {
		t.Fatalf("保留副本 = %#v", diverged)
	}
	if diverged.Points != 2 { // @0 和 @装完office，不含 rep-
		t.Fatalf("要数清里面有几个真内容快照：%#v", diverged)
	}
	if diverged.Created.IsZero() {
		t.Fatalf("名字里就带着时间，应当解析出来：%#v", diverged)
	}
	if rebuilding.Dataset != "tank/nd-rebuilding-1700000000000000001" || rebuilding.Points != 0 {
		t.Fatalf("重建副本 = %#v", rebuilding)
	}
}
