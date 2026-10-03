package platform

import (
	"context"
	"log/slog"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var ErrAlarmNotFound = errs.NotFound("告警不存在")

// AlarmCondition 是规则评估得出的一条当前未通过的检查。
type AlarmCondition struct {
	Key       string
	Severity  string // info | warn | error
	Type      string
	Resource  string
	Threshold string
	Value     string
	Message   string
}

// AlarmRule 评估一个健康领域并返回当前未通过的条件。返回错误表示未知，
// 扫描器不动该来源的告警，避免误判恢复。
type AlarmRule struct {
	Source string
	Eval   func(ctx context.Context) ([]AlarmCondition, error)
}

// AlarmService 按规则对账告警并提供查询。
type AlarmService struct {
	Store  store.Store
	Rules  []AlarmRule
	Logger *slog.Logger
	Now    func() time.Time
}

type AlarmQuery struct {
	Status   string
	Severity string
	Page     int
	Size     int
}

type AlarmListResult struct {
	Items   []domain.Alarm     `json:"items"`
	Total   int                `json:"total"`
	Page    int                `json:"page"`
	Size    int                `json:"size"`
	Summary store.AlarmSummary `json:"summary"`
}

func (s AlarmService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// RunSweeper 定时评估所有规则，直到 ctx 取消。
func (s AlarmService) RunSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.evaluate(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evaluate(ctx)
		}
	}
}

func (s AlarmService) evaluate(ctx context.Context) {
	for _, rule := range s.Rules {
		conds, err := rule.Eval(ctx)
		if err != nil {
			// 该来源状态未知，不动它的告警。
			if s.Logger != nil {
				s.Logger.Warn("alarm rule evaluation failed", "source", rule.Source, "error", err)
			}
			continue
		}
		if err := s.reconcile(ctx, rule.Source, conds); err != nil && s.Logger != nil {
			s.Logger.Warn("alarm reconcile failed", "source", rule.Source, "error", err)
		}
	}
}

// reconcile 让告警表与某来源当前未通过的条件一致：新增/变化的条件写入未关闭告警，已消失的自动恢复。
func (s AlarmService) reconcile(ctx context.Context, source string, conds []AlarmCondition) error {
	now := s.now()
	open, err := s.Store.Alarms().ListOpen(ctx)
	if err != nil {
		return err
	}
	byKey := map[string]domain.Alarm{}
	for _, a := range open {
		if a.Source == source {
			byKey[a.AlarmKey] = a
		}
	}
	current := map[string]bool{}
	for _, c := range conds {
		current[c.Key] = true
		if existing, ok := byKey[c.Key]; ok {
			existing.Severity = c.Severity
			existing.Type = c.Type
			existing.Resource = c.Resource
			existing.Threshold = c.Threshold
			existing.Value = c.Value
			existing.Message = c.Message
			existing.UpdatedAt = now
			if err := s.Store.Alarms().Update(ctx, existing); err != nil {
				return err
			}
			continue
		}
		alarm := domain.Alarm{
			ID:        "al-" + randID(),
			AlarmKey:  c.Key,
			Severity:  c.Severity,
			Type:      c.Type,
			Source:    source,
			Resource:  c.Resource,
			Threshold: c.Threshold,
			Value:     c.Value,
			Status:    "active",
			Message:   c.Message,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.Store.Alarms().Create(ctx, alarm); err != nil {
			return err
		}
	}
	// 条件已消失的未关闭告警自动恢复。
	for key, a := range byKey {
		if current[key] {
			continue
		}
		a.Status = "recovered"
		a.UpdatedAt = now
		recovered := now
		a.RecoveredAt = &recovered
		if err := s.Store.Alarms().Update(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (s AlarmService) List(ctx context.Context, q AlarmQuery) (AlarmListResult, error) {
	page := q.Page
	if page < 1 {
		page = 1
	}
	size := q.Size
	if size <= 0 || size > 200 {
		size = 20
	}
	items, total, err := s.Store.Alarms().ListPaged(ctx, store.AlarmFilter{Status: q.Status, Severity: q.Severity}, size, (page-1)*size)
	if err != nil {
		return AlarmListResult{}, err
	}
	summary, err := s.Store.Alarms().Summary(ctx)
	if err != nil {
		return AlarmListResult{}, err
	}
	if items == nil {
		items = []domain.Alarm{}
	}
	return AlarmListResult{Items: items, Total: total, Page: page, Size: size, Summary: summary}, nil
}

// Acknowledge 把活动告警静默，条件消失时仍会自动恢复；对非活动告警确认会返回错误。
func (s AlarmService) Acknowledge(ctx context.Context, id string) error {
	alarm, err := s.Store.Alarms().Get(ctx, id)
	if errs.IsNotFound(err) {
		return ErrAlarmNotFound
	} else if err != nil {
		// 数据库故障不等于告警不存在。
		return err
	}
	if alarm.Status != "active" {
		return nil
	}
	alarm.Status = "acknowledged"
	alarm.UpdatedAt = s.now()
	return s.Store.Alarms().Update(ctx, alarm)
}

func (s AlarmService) Delete(ctx context.Context, id string) error {
	if _, err := s.Store.Alarms().Get(ctx, id); errs.IsNotFound(err) {
		return ErrAlarmNotFound
	} else if err != nil {
		return err
	}
	return s.Store.Alarms().Delete(ctx, id)
}
