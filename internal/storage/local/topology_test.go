package local

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func readTestTopology(t *testing.T, p *fakePool) *topology {
	t.Helper()
	top, err := readTopology(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return top
}

// 池编辑中断后只能整体重跑，每步都必须可重入；数据集已不存在时销毁应直接成功。
func TestEnsureDestroyedAcceptsADatasetThatIsAlreadyGone(t *testing.T) {
	p := poolWith("tank/nd/img")
	top := readTestTopology(t, p)
	if err := top.ensureDestroyed(context.Background(), "tank/nd/cfg"); err != nil {
		t.Fatalf("destroying a dataset that is not there: %v", err)
	}
	if top.has("tank/nd/cfg") {
		t.Fatal("topology still reports the dataset")
	}
}

// 重跑时要识别重命名已完成并整步跳过，否则 store 会指向不存在的名字。
func TestEnsureRenamedAcceptsAMoveThatAlreadyHappened(t *testing.T) {
	p := poolWith("tank/nd/img")
	top := readTestTopology(t, p)
	if err := top.ensureRenamed(context.Background(), "tank/nd/cfg", "tank/nd/img"); err != nil {
		t.Fatalf("re-running past a completed rename: %v", err)
	}
	if len(p.ops) != 0 {
		t.Fatalf("ops = %v, want the rename skipped entirely", p.ops)
	}
}

// 新旧名都在说明是两个不同的数据集，必须报错，不能容忍 "already exists" 把数据静默留在错误名下。
func TestEnsureRenamedRefusesWhenBothNamesExist(t *testing.T) {
	p := poolWith("tank/nd/cfg", "tank/nd/cfg_before_super")
	top := readTestTopology(t, p)
	err := top.ensureRenamed(context.Background(), "tank/nd/cfg", "tank/nd/cfg_before_super")
	if err == nil {
		t.Fatal("expected a refusal: the target is a different dataset, not this rename's result")
	}
	if !strings.Contains(err.Error(), "tank/nd/cfg_before_super") {
		t.Fatalf("err = %v, want it to name the dataset in the way", err)
	}
}

func TestEnsureRenamedRefusesWhenNeitherNameExists(t *testing.T) {
	top := readTestTopology(t, newFakePool())
	if err := top.ensureRenamed(context.Background(), "tank/nd/cfg", "tank/nd/img"); err == nil {
		t.Fatal("expected an error: there is nothing to rename and nothing in place")
	}
}

// 已 promote 过时不能再尝试 promote（会报 "conflicting snapshot"），应不发出任何操作。
func TestEnsurePromotedSkipsAPromoteThatAlreadyHappened(t *testing.T) {
	p := newFakePool()
	p.datasets = []string{"tank/nd/img", "tank/nd/cfg"}
	p.snaps = map[string]bool{"tank/nd/cfg@0": true}
	p.origin = map[string]string{"tank/nd/img": "tank/nd/cfg@0"} // cfg 已 promote 到 img 之上
	top := readTestTopology(t, p)

	if err := top.ensurePromoted(context.Background(), "tank/nd/cfg", "tank/nd/img"); err != nil {
		t.Fatalf("re-running past a completed promote: %v", err)
	}
	if len(p.ops) != 0 {
		t.Fatalf("ops = %v, want no promote attempted", p.ops)
	}
}

func TestEnsurePromotedPromotesWhenItHasNotHappened(t *testing.T) {
	p := newFakePool()
	p.datasets = []string{"tank/nd/img", "tank/nd/cfg"}
	p.snaps = map[string]bool{"tank/nd/img@0": true}
	p.origin = map[string]string{"tank/nd/cfg": "tank/nd/img@0"}
	top := readTestTopology(t, p)

	if err := top.ensurePromoted(context.Background(), "tank/nd/cfg", "tank/nd/img"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.ops, []string{"promote tank/nd/cfg"}) {
		t.Fatalf("ops = %v", p.ops)
	}
}

// 从来不是克隆的数据集视为已 promote。
func TestEnsurePromotedAcceptsADatasetThatIsNotAClone(t *testing.T) {
	p := poolWith("tank/nd/img")
	top := readTestTopology(t, p)
	if err := top.ensurePromoted(context.Background(), "tank/nd/img", "tank/nd/cfg"); err != nil {
		t.Fatalf("promoting a root dataset: %v", err)
	}
}

func TestEnsureSnapshotAndCloneAcceptWhatIsAlreadyThere(t *testing.T) {
	ctx := context.Background()
	p := newFakePool()
	p.datasets = []string{"tank/nd/img", "tank/nd/cfg"}
	p.snaps = map[string]bool{"tank/nd/img@0": true}
	p.origin = map[string]string{"tank/nd/cfg": "tank/nd/img@0"}
	top := readTestTopology(t, p)

	if err := top.ensureSnapshot(ctx, "tank/nd/img", "0"); err != nil {
		t.Fatalf("re-snapshotting: %v", err)
	}
	if err := top.ensureCloned(ctx, "tank/nd/img@0", "tank/nd/cfg"); err != nil {
		t.Fatalf("re-cloning: %v", err)
	}
	if err := top.ensureSnapshotDestroyed(ctx, "tank/nd/img", "never-taken"); err != nil {
		t.Fatalf("destroying a snapshot that is not there: %v", err)
	}
}

// 池只在开始读一次，后续步骤要能从 topology 看到本次编辑自己造成的变化。
func TestTopologyAccountsForWhatTheEditDid(t *testing.T) {
	ctx := context.Background()
	p := poolWith("tank/nd/cfg")
	top := readTestTopology(t, p)

	if err := top.ensureRenamed(ctx, "tank/nd/cfg", "tank/nd/img"); err != nil {
		t.Fatal(err)
	}
	if top.has("tank/nd/cfg") || !top.has("tank/nd/img") {
		t.Fatal("topology did not follow the rename")
	}
	if err := top.ensureSnapshot(ctx, "tank/nd/img", "0"); err != nil {
		t.Fatal(err)
	}
	if err := top.ensureCloned(ctx, "tank/nd/img@0", "tank/nd/cfg"); err != nil {
		t.Fatal(err)
	}
	if !top.has("tank/nd/cfg") {
		t.Fatal("topology did not follow the clone")
	}
	if err := top.ensureDestroyed(ctx, "tank/nd/cfg"); err != nil {
		t.Fatal(err)
	}
	if top.has("tank/nd/cfg") {
		t.Fatal("topology did not follow the destroy")
	}
}
