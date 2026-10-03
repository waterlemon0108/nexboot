package platform

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tianwei/diskless/internal/store"
)

func newAlarmStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "alarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAlarmReconcileLifecycle(t *testing.T) {
	st := newAlarmStore(t)
	ctx := context.Background()
	failing := false
	svc := AlarmService{Store: st, Rules: []AlarmRule{{
		Source: "pool",
		Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			if failing {
				return []AlarmCondition{{Key: "pool-cap:tank", Severity: "warn", Type: "存储池将满", Resource: "tank", Value: "88%"}}, nil
			}
			return nil, nil
		},
	}}}

	// 健康 -> 无告警
	svc.evaluate(ctx)
	if res, _ := svc.List(ctx, AlarmQuery{}); res.Total != 0 {
		t.Fatalf("expected 0 alarms, got %d", res.Total)
	}

	// 失败 -> 一条活动 warn 告警
	failing = true
	svc.evaluate(ctx)
	res, _ := svc.List(ctx, AlarmQuery{})
	if res.Total != 1 || res.Items[0].Status != "active" || res.Summary.Active != 1 || res.Summary.Warn != 1 {
		t.Fatalf("expected 1 active warn alarm, got total=%d summary=%+v", res.Total, res.Summary)
	}
	id := res.Items[0].ID

	// 仍失败 -> 按键去重，不重复产生
	svc.evaluate(ctx)
	if open, _ := svc.List(ctx, AlarmQuery{Status: "open"}); open.Total != 1 {
		t.Fatalf("dedup failed: open total=%d", open.Total)
	}

	// 确认 -> 静默（不计入活动），但仍未关闭
	if err := svc.Acknowledge(ctx, id); err != nil {
		t.Fatalf("ack: %v", err)
	}
	res, _ = svc.List(ctx, AlarmQuery{})
	if res.Items[0].Status != "acknowledged" || res.Summary.Active != 0 {
		t.Fatalf("ack wrong: status=%s active=%d", res.Items[0].Status, res.Summary.Active)
	}
	// 确认后仍失败 -> 不产生新告警
	svc.evaluate(ctx)
	if open, _ := svc.List(ctx, AlarmQuery{Status: "open"}); open.Total != 1 {
		t.Fatalf("acknowledged alarm must not duplicate: %d", open.Total)
	}

	// 条件消失 -> 自动恢复
	failing = false
	svc.evaluate(ctx)
	res, _ = svc.List(ctx, AlarmQuery{})
	if res.Items[0].Status != "recovered" || res.Items[0].RecoveredAt == nil {
		t.Fatalf("expected recovered with timestamp, got %+v", res.Items[0])
	}

	// 删除历史
	if err := svc.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res, _ := svc.List(ctx, AlarmQuery{}); res.Total != 0 {
		t.Fatalf("expected 0 after delete, got %d", res.Total)
	}
}

func TestAlarmRuleErrorLeavesAlarmsUntouched(t *testing.T) {
	st := newAlarmStore(t)
	ctx := context.Background()
	failMode := false
	svc := AlarmService{Store: st, Rules: []AlarmRule{{
		Source: "service",
		Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			if failMode {
				return nil, errors.New("systemctl unavailable")
			}
			return []AlarmCondition{{Key: "service-down:dnsmasq", Severity: "error", Type: "服务异常", Resource: "dnsmasq.service"}}, nil
		},
	}}}

	svc.evaluate(ctx) // 产生告警
	if open, _ := svc.List(ctx, AlarmQuery{Status: "open"}); open.Total != 1 {
		t.Fatalf("expected 1 alarm, got %d", open.Total)
	}
	// 规则报错即状态未知 -> 不得自动恢复
	failMode = true
	svc.evaluate(ctx)
	open, _ := svc.List(ctx, AlarmQuery{Status: "open"})
	if open.Total != 1 || open.Items[0].Status != "active" {
		t.Fatalf("rule error must not resolve alarms, got total=%d", open.Total)
	}
}
