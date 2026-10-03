package platform

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/ha"
)

func takeoverAt(at time.Time, mine, theirs time.Time) ha.TakeoverReport {
	return ha.TakeoverReport{At: at, Peer: "node-a", Cause: "期限内没有追平",
		Mine: "rep-" + nanos(mine), Theirs: "rep-" + nanos(theirs)}
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// 接任时没追平：运维要在界面上看到丢了多久的改动、是哪台的、接下来会发生什么。
func TestTakeoverAlarmSaysWhatWasLeftBehind(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	report := takeoverAt(now.Add(-time.Hour), now.Add(-70*time.Minute), now.Add(-64*time.Minute))
	rule := TakeoverAlarmRule(func() (ha.TakeoverReport, bool) { return report, true }, func() time.Time { return now })

	conds, err := rule.Eval(context.Background())
	if err != nil || len(conds) != 1 {
		t.Fatalf("conds=%v err=%v", conds, err)
	}
	msg := conds[0].Message
	for _, want := range []string{"node-a", "6 分钟", "保留", "期限内没有追平"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("告警里应有「%s」: %s", want, msg)
		}
	}
	if strings.Contains(msg, "rep-") {
		t.Fatalf("告警里不该出现内部的标记名: %s", msg)
	}
}

// 接任时目录不完整：列出缺了什么，并告诉运维两种可能各该怎么办。
func TestTakeoverAlarmListsWhatIsMissing(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	report := ha.TakeoverReport{At: now.Add(-time.Minute), Missing: []string{"配置「办公」", "镜像「win11」"}}
	rule := TakeoverAlarmRule(func() (ha.TakeoverReport, bool) { return report, true }, func() time.Time { return now })

	conds, _ := rule.Eval(context.Background())
	if len(conds) != 1 || !strings.Contains(conds[0].Message, "配置「办公」") ||
		!strings.Contains(conds[0].Message, "镜像管理") || strings.Contains(conds[0].Message, "系统设置") ||
		!strings.Contains(conds[0].Message, "备份") {
		t.Fatalf("conds=%+v", conds)
	}
}

// 没有记录就没有告警；记录超过一周也不再告警。
func TestTakeoverAlarmIsQuietWithoutARecentReport(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	none := TakeoverAlarmRule(func() (ha.TakeoverReport, bool) { return ha.TakeoverReport{}, false }, func() time.Time { return now })
	if conds, _ := none.Eval(context.Background()); len(conds) != 0 {
		t.Fatalf("conds=%+v", conds)
	}
	old := takeoverAt(now.Add(-8*24*time.Hour), now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour+time.Minute))
	stale := TakeoverAlarmRule(func() (ha.TakeoverReport, bool) { return old, true }, func() time.Time { return now })
	if conds, _ := stale.Eval(context.Background()); len(conds) != 0 {
		t.Fatalf("conds=%+v", conds)
	}
}
