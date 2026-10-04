package store

import (
	"context"
	"strings"

	"github.com/tianwei/diskless/internal/domain"
)

// AlarmFilter 收窄告警查询，空字段忽略。
type AlarmFilter struct {
	Status   string // active | acknowledged | recovered；"open" = active+acknowledged
	Severity string
}

// AlarmSummary 按严重级别统计活动告警数（用于角标和统计卡片）。
type AlarmSummary struct {
	Active int `json:"active"`
	Error  int `json:"error"`
	Warn   int `json:"warn"`
	Info   int `json:"info"`
}

type alarmRepository struct {
	repository[domain.Alarm]
	quiet repository[domain.Alarm]
}

// Update 让每轮评估只刷新数值和文案的更新不计数，否则只要有一条未关闭的告警，空闲集群就每轮复制。
func (r alarmRepository) Update(ctx context.Context, v domain.Alarm) error {
	if old, err := r.Get(ctx, v.ID); err == nil && valueOnly(old, v) {
		return r.quiet.Update(ctx, v)
	}
	return r.repository.Update(ctx, v)
}

func valueOnly(a, b domain.Alarm) bool {
	return a.AlarmKey == b.AlarmKey && a.Severity == b.Severity && a.Type == b.Type && a.Source == b.Source &&
		a.Resource == b.Resource && a.Threshold == b.Threshold && a.Status == b.Status
}

func alarmWhere(r alarmRepository, f AlarmFilter) (string, []any) {
	var cond []string
	var args []any
	if f.Status == "open" {
		cond = append(cond, "status IN ('active','acknowledged')")
	} else if f.Status != "" {
		args = append(args, f.Status)
		cond = append(cond, "status = "+r.placeholder(len(args)))
	}
	if f.Severity != "" {
		args = append(args, f.Severity)
		cond = append(cond, "severity = "+r.placeholder(len(args)))
	}
	if len(cond) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(cond, " AND "), args
}

func (r alarmRepository) ListPaged(ctx context.Context, f AlarmFilter, limit, offset int) ([]domain.Alarm, int, error) {
	where, args := alarmWhere(r, f)
	var total int
	if err := r.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+r.codec.table+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// 未关闭的告警在前（active/acknowledged 先于 recovered），再按新到旧。
	query := "SELECT " + joinColumns(r.codec.columns) + " FROM " + r.codec.table + where +
		" ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'acknowledged' THEN 1 ELSE 2 END, updated_at DESC" +
		" LIMIT " + r.placeholder(len(args)+1) + " OFFSET " + r.placeholder(len(args)+2)
	args = append(args, limit, offset)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []domain.Alarm
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// ListOpen 返回所有 active/acknowledged 告警，供 sweeper 对账。
func (r alarmRepository) ListOpen(ctx context.Context) ([]domain.Alarm, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE status IN ('active','acknowledged')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Alarm
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Summary 按严重级别返回活动告警数。
func (r alarmRepository) Summary(ctx context.Context) (AlarmSummary, error) {
	var s AlarmSummary
	rows, err := r.q.QueryContext(ctx,
		"SELECT severity, COUNT(*) FROM "+r.codec.table+" WHERE status = 'active' GROUP BY severity")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			return s, err
		}
		s.Active += n
		switch sev {
		case "error":
			s.Error = n
		case "warn":
			s.Warn = n
		case "info":
			s.Info = n
		}
	}
	return s, rows.Err()
}
