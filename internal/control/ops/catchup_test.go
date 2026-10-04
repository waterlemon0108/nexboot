package ops

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage/zfs"
)

// inv 按「数据集，后跟它的快照（按创建顺序）」写清单，和 zfs list -r -t all 一样。
// 快照写成 "名字=guid"，创建时间按出现顺序递增。
func inv(root string, datasets ...[]string) zfs.GUIDInventory {
	var out zfs.GUIDInventory
	t := int64(100)
	for _, ds := range datasets {
		name, origin := ds[0], ""
		if n, o, ok := strings.Cut(name, "<"); ok {
			name, origin = n, root+o
		}
		out.Entries = append(out.Entries, zfs.GUIDEntry{Name: root + name, GUID: "ds" + name, Origin: origin})
		for _, s := range ds[1:] {
			snap, guid, _ := strings.Cut(s, "=")
			t++
			out.Entries = append(out.Entries, zfs.GUIDEntry{Name: root + name + "@" + snap, GUID: guid, Created: t})
		}
	}
	return out
}

func steps(plan []catchUpStep) string {
	var s []string
	for _, st := range plan {
		s = append(s, st.String())
	}
	return strings.Join(s, "; ")
}

// 超管保存换了配置身份后逐数据集追：被顶替的配置从共同还原点 @1 增量（promote
// 保留了 GUID），其余照常增量，根的标记放最后。
func TestCatchUpAfterASuperSaveSendsOnlyTheNewRestorePoint(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "2=c2new", "rep-2=c2r"}, // 顶替后：@1 同 GUID，rep-1 随旧数据集没了
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/img", "0=i0", "rep-1=i1"},
		[]string{"/cfg</img@0", "1=c1", "rep-1=c1r"},
	)
	plan, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "incremental /img rep-1→rep-2; incremental /cfg 1→rep-2; incremental (根) rep-1→rep-2"
	if got := steps(plan); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// 新 fork 的配置按克隆发到第一个还原点再补增量，备机上仍是克隆，不占整份空间。
func TestCatchUpSendsANewConfigAsAClone(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/fork</img@0", "0=f0", "a=fa", "rep-2=f2"},
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/img", "0=i0", "rep-1=i1"},
	)
	plan, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "incremental /img rep-1→rep-2; clone /fork from /img@0 to 0; incremental /fork 0→rep-2; incremental (根) rep-1→rep-2"
	if got := steps(plan); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// 写入者上删掉的，备机跟着删，深的先删；在这一轮之后才建出来的（没有目标标记）留到下一轮。
func TestCatchUpDeletesWhatTheWriterRemovedAndSkipsWhatIsNewer(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/later</img@0", "0=l0"}, // rep-2 之后才建
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/img", "0=i0", "rep-1=i1"},
		[]string{"/old</img@0", "0=o0", "rep-1=o1"},
		[]string{"/old/child", "0=oc0", "rep-1=oc1"},
	)
	plan, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "incremental /img rep-1→rep-2; destroy /old/child; destroy /old; incremental (根) rep-1→rep-2"
	if got := steps(plan); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// 本机在共同基准之后自己写了对端没有的还原点时不追（recv -F 会回滚掉它），交给分叉处理。
func TestCatchUpRefusesToRollBackRestorePointsWrittenHere(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/img", "0=i0", "rep-1=i1", "mine=m1"},
	)
	base := local.Entries[1].Created // 本机 root@rep-1 的创建时间
	_, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", base)
	if !errors.Is(err, errCatchUpDiverged) || !strings.Contains(err.Error(), "/img@mine") {
		t.Fatalf("err = %v", err)
	}
}

// 同名但无共同快照时不硬追，返回错误。
func TestCatchUpGivesUpOnADatasetWithNothingInCommon(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=x0", "rep-2=x2"},
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/img", "0=i0", "rep-1=i1"},
	)
	if _, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", 0); err == nil || errors.Is(err, errCatchUpDiverged) {
		t.Fatalf("应放弃逐个追（交给整份重建），err = %v", err)
	}
	_ = fmt.Sprint
}

// 重建被打断后重启，库副本数据集会被 EnsureDBCopyDataset 新建成没有快照的空壳。
// 它没有可保留的内容，删掉重收即可，不能让整轮追平失败、退回整份重建。
func TestCatchUpReplacesAnEmptyShellDataset(t *testing.T) {
	sender := inv("data/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/db", "rep-1=d1", "rep-2=d2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
	)
	local := inv("tank/nd",
		[]string{"", "rep-1=r1"},
		[]string{"/db"},
		[]string{"/img", "0=i0", "rep-1=i1"},
	)
	plan, err := planCatchUp(sender, "data/nd", local, "tank/nd", "rep-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "destroy /db; full /db to rep-1; incremental /db rep-1→rep-2; incremental /img rep-1→rep-2; incremental (根) rep-1→rep-2"
	if got := steps(plan); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// CompleteMarker 只在每个数据集都带着根的最新标记时才算数（递归流失败时根先落地）。
func TestCompleteMarkerRequiresEveryDatasetToHoldIt(t *testing.T) {
	whole := inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1", "rep-2=c2"},
	)
	if got := CompleteMarker(whole, "tank/nd"); got != "rep-2" {
		t.Fatalf("完整时 = %q", got)
	}
	partial := inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
		[]string{"/cfg</img@0", "1=c1"},
	)
	if got := CompleteMarker(partial, "tank/nd"); got != "" {
		t.Fatalf("配置没收到这一轮时不该报 rep-2，得到 %q", got)
	}

	// 追平断在克隆刚建好、还没补到这一轮：同样不算完整。
	cut := inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
	)
	cut.Entries = append(cut.Entries, zfs.GUIDEntry{Name: "tank/nd/fork", GUID: "dsfork", Origin: "tank/nd/img@0", Created: 10_000},
		zfs.GUIDEntry{Name: "tank/nd/fork@0", GUID: "f0", Created: 10_001})
	if got := CompleteMarker(cut, "tank/nd"); got != "" {
		t.Fatalf("追赶半截的克隆不该算完整，得到 %q", got)
	}
	// 写入者自己：上一轮之后新建的配置还没有标记，不影响 WriterMarker。
	fresh := inv("tank/nd",
		[]string{"", "rep-1=r1", "rep-2=r2"},
		[]string{"/img", "0=i0", "rep-1=i1", "rep-2=i2"},
	)
	fresh.Entries = append(fresh.Entries, zfs.GUIDEntry{Name: "tank/nd/new", GUID: "dsnew", Created: 10_000})
	if got := WriterMarker(fresh, "tank/nd"); got != "rep-2" {
		t.Fatalf("写入者报自己最新的一轮，得到 %q", got)
	}
}
