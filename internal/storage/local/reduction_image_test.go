package local

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// seedImageWithReduction 构造 vmdk -> vmdk_default -> vmdk_default@r1。
func seedImageWithReduction(t *testing.T) (*Agent, *fakePool) {
	t.Helper()
	p := newFakePool()
	img, cfg := p.Dataset("vmdk"), p.Dataset("vmdk_default")
	p.datasets = append(p.datasets, img, cfg)
	p.snaps[img+"@0"] = true
	p.origin[cfg] = img + "@0"
	p.snaps[cfg+"@0"] = true
	p.snaps[cfg+"@r1"] = true
	return New("server-a", p), p
}

func noSnapshotsLeftUnder(t *testing.T, p *fakePool, prefix string) {
	t.Helper()
	for s := range p.snaps {
		if strings.Contains(s, prefix) {
			t.Fatalf("leftover snapshot %s", s)
		}
	}
}

func TestExportFromARestorePointSendsAScratchCloneAndCleansUp(t *testing.T) {
	agent, p := seedImageWithReduction(t)
	var out bytes.Buffer
	if err := agent.ExportImage(context.Background(), storage.ExportImageReq{ConfigID: "vmdk_default", Snapshot: "r1", W: &out}); err != nil {
		t.Fatal(err)
	}
	sent := out.String()
	if !strings.HasPrefix(sent, "STREAM:tank/run/EXPORT-VMDK_DEFAULT-") || !strings.Contains(sent, "@"+storage.ExportSnapshotPrefix) {
		t.Fatalf("sent %q, want the scratch clone's export snapshot", sent)
	}
	for _, d := range p.datasets {
		if strings.Contains(d, "EXPORT-") {
			t.Fatalf("scratch clone left behind: %s", d)
		}
	}
	noSnapshotsLeftUnder(t, p, storage.ExportSnapshotPrefix)
	if !p.snaps[p.Dataset("vmdk_default")+"@r1"] {
		t.Fatal("the restore point itself must survive")
	}
}

func TestCopyReductionToImageMakesAnIndependentImage(t *testing.T) {
	agent, p := seedImageWithReduction(t)
	res, err := agent.CopyReductionToImage(context.Background(), storage.CopyReductionReq{
		ConfigID: "vmdk_default", Snapshot: "r1", Name: "vmdk2", OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID != "vmdk2" || res.Config.ID != "vmdk2_default" || res.Image.OSType != domain.OSTypeWindows {
		t.Fatalf("result = %#v", res)
	}
	img := p.Dataset("vmdk2")
	if !p.present(img) || p.origin[img] != "" {
		t.Fatalf("new image must be a received copy, not a clone: present=%v origin=%q", p.present(img), p.origin[img])
	}
	if !p.snaps[img+"@0"] || !p.present(p.Dataset("vmdk2_default")) || !p.snaps[p.Dataset("vmdk2_default")+"@0"] {
		t.Fatalf("new image lacks its baseline or default config: %v", p.state())
	}
	for _, d := range p.datasets {
		if strings.Contains(d, "EXPORT-") {
			t.Fatalf("scratch clone left behind: %s", d)
		}
	}
	noSnapshotsLeftUnder(t, p, storage.ExportSnapshotPrefix)
}

func TestCopyReductionToImageNamesItsTargetBeforeWriting(t *testing.T) {
	agent, p := seedImageWithReduction(t)
	var target string
	existed := true
	_, err := agent.CopyReductionToImage(context.Background(), storage.CopyReductionReq{
		ConfigID: "vmdk_default", Snapshot: "r1", Name: "vmdk2", OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeSystem,
		OnTarget: func(id string) { target, existed = id, p.present(p.Dataset(id)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if target != "vmdk2" || existed {
		t.Fatalf("target = %q, dataset already there = %v", target, existed)
	}
}

func TestCopyReductionToImageAvoidsANameInUse(t *testing.T) {
	agent, _ := seedImageWithReduction(t)
	res, err := agent.CopyReductionToImage(context.Background(), storage.CopyReductionReq{
		ConfigID: "vmdk_default", Snapshot: "r1", Name: "vmdk", OSType: domain.OSTypeWindows,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID == "vmdk" {
		t.Fatal("copied over the existing image")
	}
}

// 从非最新还原点覆盖镜像：回滚丢弃后面的还原点前，须先删掉从这些点派生的兄弟配置。
func seedOverwriteFromMiddle() *fakePool {
	z := seedMergePool()
	z.snaps["tank/nd/cfg-keep@r2"] = true
	z.snapOrder = []string{"tank/nd/cfg-keep@0", "tank/nd/cfg-keep@r1", "tank/nd/cfg-keep@r2"}
	z.origin["tank/nd/cfg-drop"] = "tank/nd/cfg-keep@r2"
	return z
}

func overwriteReq() storage.MergeConfigReq {
	req := mergeReq()
	req.RollbackTo = "r1"
	return req
}

func TestOverwriteFromAnOlderRestorePointRollsBackAfterSiblingsGo(t *testing.T) {
	z := seedOverwriteFromMiddle()
	if _, err := New("server-a", z).MergeConfig(context.Background(), overwriteReq()); err != nil {
		t.Fatal(err)
	}
	dropAt, rollbackAt := -1, -1
	for i, op := range z.ops {
		switch op {
		case "destroy tank/nd/cfg-drop":
			dropAt = i
		case "rollback tank/nd/cfg-keep@r1":
			rollbackAt = i
		}
	}
	if dropAt < 0 || rollbackAt < 0 || rollbackAt < dropAt {
		t.Fatalf("ops = %#v", z.ops)
	}
	if got := z.state(); !equalStrings(got, wantMerged) {
		t.Fatalf("state = %#v, want %#v", got, wantMerged)
	}
}

func TestOverwriteFromAnOlderRestorePointResumes(t *testing.T) {
	for _, stopAt := range []string{"rollback tank/nd/cfg-keep@r1", "destroy-snapshot tank/nd/cfg-keep@r1", "promote tank/nd/cfg-keep", "rename tank/nd/cfg-keep"} {
		t.Run(stopAt, func(t *testing.T) {
			z := seedOverwriteFromMiddle()
			z.failOn = stopAt
			agent := New("server-a", z)
			if _, err := agent.MergeConfig(context.Background(), overwriteReq()); err == nil {
				t.Fatal("expected the injected failure")
			}
			if _, err := agent.MergeConfig(context.Background(), overwriteReq()); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if got := z.state(); !equalStrings(got, wantMerged) {
				t.Fatalf("state = %#v, want %#v", got, wantMerged)
			}
		})
	}
}

// 另存为新镜像时目标被并发的另一次导入占了，接收失败；回滚不能删掉对方的数据集。
func TestCopyReductionToImageLeavesAConcurrentImportAlone(t *testing.T) {
	agent, p := seedImageWithReduction(t)
	p.beforeOp = func(op string) {
		if strings.HasPrefix(op, "clone tank/run/EXPORT-") {
			p.datasets = append(p.datasets, p.Dataset("vmdk2")) // 另一次导入抢先落地
		}
	}
	if _, err := agent.CopyReductionToImage(context.Background(), storage.CopyReductionReq{
		ConfigID: "vmdk_default", Snapshot: "r1", Name: "vmdk2", OSType: domain.OSTypeWindows,
	}); err == nil {
		t.Fatal("want the receive to fail")
	}
	for _, d := range p.destroyAttempts {
		if d == p.Dataset("vmdk2") {
			t.Fatalf("destroyed the other import's dataset: %v", p.destroyAttempts)
		}
	}
}

// 本次自己收下的副本在后续步骤失败时照样要回滚掉。
func TestCopyReductionToImageRollsBackItsOwnCopy(t *testing.T) {
	agent, p := seedImageWithReduction(t)
	p.failOn = "destroy-snapshot tank/nd/vmdk2@"
	if _, err := agent.CopyReductionToImage(context.Background(), storage.CopyReductionReq{
		ConfigID: "vmdk_default", Snapshot: "r1", Name: "vmdk2", OSType: domain.OSTypeWindows,
	}); err == nil {
		t.Fatal("want failure")
	}
	if p.present(p.Dataset("vmdk2")) {
		t.Fatalf("own partial copy left behind: %v", p.state())
	}
}
