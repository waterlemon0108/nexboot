package local

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// 首次备份没有基准：把整个目录容器整份复制到备份池。
func TestBackupFullWhenTargetIsEmpty(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = nil // 容器存在，但还没有共同快照
	agent := New("server-a", p)

	res, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "" || res.FellBack {
		t.Fatalf("result = %#v, want full", res)
	}
	if len(p.replicates) != 1 {
		t.Fatalf("replicates = %#v", p.replicates)
	}
	r := p.replicates[0]
	if r.root != "tank/nd" || r.target != "backup/ndiskless/nd" || r.base != "" || r.snapshot != "ndbackup-1" {
		t.Fatalf("replicate = %#v", r)
	}
	if p.recursiveSnaps["tank/nd@ndbackup-1"] != 1 {
		t.Fatalf("recursive snapshot missing: %#v", p.recursiveSnaps)
	}
	if p.ensuredFilesystems["backup/ndiskless"] != 1 {
		t.Fatalf("target parent not ensured: %#v", p.ensuredFilesystems)
	}
}

// 之后的备份只送两侧最新共同快照以来的增量；基准从池本身判定，
// 不用存储的记录，因为 promote 和合并会让记录悄悄失效。
func TestBackupIncrementalFromNewestCommonSnapshot(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-1\tg1", "tank/nd@ndbackup-2\tg2"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1", "backup/ndiskless/nd@ndbackup-2\tg2"}
	agent := New("server-a", p)

	res, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "ndbackup-2" || res.FellBack {
		t.Fatalf("result = %#v, want incremental from ndbackup-2", res)
	}
	if len(p.replicates) != 1 || p.replicates[0].base != "ndbackup-2" {
		t.Fatalf("replicates = %#v", p.replicates)
	}
}

// promote（合并、超管保存）会使增量流无法接收；此时备份应自行整份重建，而不是每轮都失败等人处理。
func TestBackupFallsBackToFullResyncWhenIncrementalFails(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-1\tg1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"}
	p.replicateErrs = map[string]error{"ndbackup-1": fmt.Errorf("cannot receive incremental stream")}
	p.datasets = append(p.datasets, "backup/ndiskless/nd") // 过时副本
	agent := New("server-a", p)

	res, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.FellBack || res.Base != "" {
		t.Fatalf("result = %#v, want full fallback", res)
	}
	if len(p.replicates) != 2 || p.replicates[0].base != "ndbackup-1" || p.replicates[1].base != "" {
		t.Fatalf("replicates = %#v", p.replicates)
	}
	if !movedAsideThenRemoved(p, "backup/ndiskless/nd") {
		t.Fatalf("stale copy must be moved aside before the full resync and removed after: ops=%v", p.ops)
	}
}

// movedAsideThenRemoved 判断旧副本先被改名挪开、之后才删掉的是挪开的那份，原名从没被直接销毁。
func movedAsideThenRemoved(p *fakePool, target string) bool {
	renamed, removed := false, false
	for _, op := range p.ops {
		switch {
		case op == "rename "+target:
			renamed = true
		case strings.HasPrefix(op, "destroy "+target+"-rebuilding-"):
			removed = renamed
		case op == "destroy "+target:
			return false
		}
	}
	return renamed && removed
}

// 整份流失败就报备份失败，不再循环重试。
func TestBackupFullFailureIsReported(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = nil
	p.replicateErrs = map[string]error{"": fmt.Errorf("no space")}
	agent := New("server-a", p)

	_, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-1"})
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("err = %v", err)
	}
}

// 发送端的旧备份标记会累积，每次只保留最新几个。
func TestBackupPrunesItsOwnMarkers(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = nil
	agent := New("server-a", p)
	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-1"}); err != nil {
		t.Fatal(err)
	}
	if len(p.prunes) != 1 || p.prunes[0] != "tank/nd|ndbackup-|3" {
		t.Fatalf("prunes = %#v", p.prunes)
	}
}

func TestBackupRejectsBadRequests(t *testing.T) {
	p := newFakePool()
	agent := New("server-a", p)
	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "tank", Snapshot: "s"}); err == nil {
		t.Fatal("backup pool == data pool must be refused")
	}
	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "", Snapshot: "s"}); err == nil {
		t.Fatal("empty backup pool must be refused")
	}
	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: ""}); err == nil {
		t.Fatal("empty snapshot must be refused")
	}
}

// 副本存在却与源没有共同快照（标记被手工删光，或副本来自池的前一次生命周期）时，
// recv -F 无法在其上接收整份流，须先把过时副本挪开。
func TestBackupFullOverStaleCopyDestroysItFirst(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-9\tg9"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"} // 没有交集
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	agent := New("server-a", p)

	res, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-10"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "" {
		t.Fatalf("result = %#v, want full", res)
	}
	if !movedAsideThenRemoved(p, "backup/ndiskless/nd") {
		t.Fatalf("stale copy must be moved aside before a full stream lands on it: ops=%v", p.ops)
	}
	if len(p.replicates) != 1 || p.replicates[0].base != "" {
		t.Fatalf("replicates = %#v", p.replicates)
	}
}

// 副本根先于子数据集收到标记，只有源端带标记的数据集都到齐才算送完。
func TestCopiedBackupMarkerRequiresTheWholeRound(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{
		"tank/nd\tr", "tank/nd@ndbackup-1\ta1", "tank/nd@ndbackup-2\ta2",
		"tank/nd/img\ti", "tank/nd/img@ndbackup-1\tb1", "tank/nd/img@ndbackup-2\tb2",
		"tank/nd/cfg\tc", "tank/nd/cfg@ndbackup-1\tc1", "tank/nd/cfg@ndbackup-2\tc2",
	}
	// cfg 还停在上一轮
	p.guids["backup/ndiskless/nd"] = []string{
		"backup/ndiskless/nd\tR", "backup/ndiskless/nd@ndbackup-1\ta1", "backup/ndiskless/nd@ndbackup-2\ta2",
		"backup/ndiskless/nd/img\tI", "backup/ndiskless/nd/img@ndbackup-1\tb1", "backup/ndiskless/nd/img@ndbackup-2\tb2",
		"backup/ndiskless/nd/cfg\tC", "backup/ndiskless/nd/cfg@ndbackup-1\tc1",
	}
	agent := New("server-a", p)
	got, err := agent.CopiedBackupMarker(context.Background(), "backup")
	if err != nil || got != "ndbackup-1" {
		t.Fatalf("收到一半的第二轮不算送完，应报上一轮 ndbackup-1，得到 %q err=%v", got, err)
	}

	// 子数据集还没出现在副本里
	p.guids["backup/ndiskless/nd"] = []string{
		"backup/ndiskless/nd\tR", "backup/ndiskless/nd@ndbackup-1\ta1",
		"backup/ndiskless/nd/img\tI", "backup/ndiskless/nd/img@ndbackup-1\tb1",
	}
	got, _ = agent.CopiedBackupMarker(context.Background(), "backup")
	if got != "" {
		t.Fatalf("缺数据集的副本一轮都不算送完，得到 %q", got)
	}
}

// 半截副本的增量基准会是这一轮自己，只能整份重送。
func TestBackupResendsWholeWhenTheCopyHoldsAPartialRound(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd\tr", "tank/nd@ndbackup-1\ta1", "tank/nd/img\ti", "tank/nd/img@ndbackup-1\tb1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd\tR", "backup/ndiskless/nd@ndbackup-1\ta1"}
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	agent := New("server-a", p)

	res, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-1", Reuse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.replicates) != 1 || p.replicates[0].base != "" || res.Base != "" {
		t.Fatalf("应整份重送: replicates=%#v res=%#v", p.replicates, res)
	}
}

// halfReceivingPool 让整份接收失败时在目标名下留下收了一半的副本，像 `zfs recv -s` 那样。
type halfReceivingPool struct {
	*fakePool
}

func (h *halfReceivingPool) Replicate(ctx context.Context, root, target, base, snapshot string) error {
	err := h.fakePool.Replicate(ctx, root, target, base, snapshot)
	if err != nil && base == "" && !h.present(target) {
		h.datasets = append(h.datasets, target)
	}
	return err
}

func hasAside(p *fakePool, target string) bool {
	for _, d := range p.datasets {
		if strings.HasPrefix(d, target+"-rebuilding-") {
			return true
		}
	}
	return false
}

func destroyedExactly(p *fakePool, dataset string) bool {
	for _, d := range p.destroyed {
		if d == dataset {
			return true
		}
	}
	return false
}

// 增量收不下、整份重建又因备份池满失败时，旧备份必须原样留着，不能一份都不剩。
func TestBackupKeepsTheOldCopyWhenTheRebuildFails(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-1\tg1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"}
	p.replicateErrs = map[string]error{
		"ndbackup-1": fmt.Errorf("cannot receive incremental stream"),
		"":           fmt.Errorf("out of space"),
	}
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	agent := New("server-a", p)

	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-2"}); err == nil {
		t.Fatal("rebuild failed, backup must report failure")
	}
	if !p.present("backup/ndiskless/nd") || destroyedExactly(p, "backup/ndiskless/nd") {
		t.Fatalf("old backup must survive under its own name: datasets=%v destroyed=%v", p.datasets, p.destroyed)
	}
	if hasAside(p, "backup/ndiskless/nd") {
		t.Fatalf("moved-aside copy must be renamed back: %v", p.datasets)
	}
}

// 没有共同快照的旧副本同样先挪开；整份流失败就改回原名。
func TestBackupKeepsTheStaleCopyWhenTheFullStreamFails(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-9\tg9"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"}
	p.replicateErrs = map[string]error{"": fmt.Errorf("out of space")}
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	agent := New("server-a", p)

	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-10"}); err == nil {
		t.Fatal("want failure")
	}
	if !p.present("backup/ndiskless/nd") || destroyedExactly(p, "backup/ndiskless/nd") {
		t.Fatalf("old backup must survive: datasets=%v destroyed=%v", p.datasets, p.destroyed)
	}
}

// 上一轮的半截副本也是一份可用的旧备份（更早的轮次都在），重送失败时要保住。
func TestBackupKeepsThePartialRoundWhenTheResendFails(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd\tr", "tank/nd@ndbackup-1\ta1", "tank/nd/img\ti", "tank/nd/img@ndbackup-1\tb1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd\tR", "backup/ndiskless/nd@ndbackup-1\ta1"}
	p.replicateErrs = map[string]error{"": fmt.Errorf("out of space")}
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	agent := New("server-a", p)

	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-1", Reuse: true}); err == nil {
		t.Fatal("want failure")
	}
	if !p.present("backup/ndiskless/nd") || destroyedExactly(p, "backup/ndiskless/nd") {
		t.Fatalf("old backup must survive: datasets=%v destroyed=%v", p.datasets, p.destroyed)
	}
}

// 失败的整份接收会在原名下留残片，挡住改回原名；残片要先删掉，旧副本才能放回去。
func TestBackupDropsTheHalfReceivedCopyBeforeRestoringTheOld(t *testing.T) {
	p := &halfReceivingPool{fakePool: newFakePool()}
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-1\tg1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"}
	p.replicateErrs = map[string]error{
		"ndbackup-1": fmt.Errorf("cannot receive incremental stream"),
		"":           fmt.Errorf("out of space"),
	}
	p.datasets = append(p.datasets, "backup/ndiskless/nd")
	p.snaps["backup/ndiskless/nd@ndbackup-1"] = true
	agent := New("server-a", p)

	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-2"}); err == nil {
		t.Fatal("want failure")
	}
	if !p.present("backup/ndiskless/nd") || hasAside(p.fakePool, "backup/ndiskless/nd") {
		t.Fatalf("old copy must be back under its name: %v", p.datasets)
	}
	if !p.snaps["backup/ndiskless/nd@ndbackup-1"] {
		t.Fatalf("restored copy must be the old one with its snapshots: %v", p.state())
	}
}

// 重建成功后才删挪开的旧副本，之前留下的重建残留也一并回收。
func TestBackupRemovesTheMovedAsideCopyOnlyAfterSuccess(t *testing.T) {
	p := newFakePool()
	p.guids["tank/nd"] = []string{"tank/nd@ndbackup-1\tg1"}
	p.guids["backup/ndiskless/nd"] = []string{"backup/ndiskless/nd@ndbackup-1\tg1"}
	p.replicateErrs = map[string]error{"ndbackup-1": fmt.Errorf("cannot receive incremental stream")}
	p.datasets = append(p.datasets, "backup/ndiskless/nd", "backup/ndiskless/nd-rebuilding-5")
	agent := New("server-a", p)
	var renamedBeforeFull bool
	p.beforeOp = func(op string) { renamedBeforeFull = op == "rename backup/ndiskless/nd" && len(p.replicates) == 1 }

	if _, err := agent.Backup(context.Background(), storage.BackupReq{BackupPool: "backup", Snapshot: "ndbackup-2"}); err != nil {
		t.Fatal(err)
	}
	if !renamedBeforeFull {
		t.Fatalf("old copy must be moved aside after the failed incremental and before the full: ops=%v", p.ops)
	}
	if destroyedExactly(p, "backup/ndiskless/nd") || hasAside(p, "backup/ndiskless/nd") {
		t.Fatalf("only aside copies may be destroyed, and all of them after success: destroyed=%v datasets=%v", p.destroyed, p.datasets)
	}
}
