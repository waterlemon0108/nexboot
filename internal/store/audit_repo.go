package store

import (
	"context"
	"strings"

	"github.com/tianwei/diskless/internal/domain"
)

// AuditFilter 收窄审计日志查询，空字段忽略。Search 在 username/action/detail 中做子串匹配。
type AuditFilter struct {
	Type     string
	Username string
	Module   string
	Status   string
	Search   string
	From     string // created_at >=（含）
	To       string // created_at <=（含）
}

type auditLogRepository struct {
	repository[domain.AuditLog]
}

// ListPaged 按新到旧返回匹配过滤条件的审计日志，以及用于分页的总数。
func (r auditLogRepository) ListPaged(ctx context.Context, f AuditFilter, limit, offset int) ([]domain.AuditLog, int, error) {
	var cond []string
	var args []any
	eq := func(col, v string) {
		if v != "" {
			args = append(args, v)
			cond = append(cond, col+" = "+r.placeholder(len(args)))
		}
	}
	eq("type", f.Type)
	eq("username", f.Username)
	eq("module", f.Module)
	eq("status", f.Status)
	if f.Search != "" {
		like := "%" + f.Search + "%"
		a, b, c := len(args)+1, len(args)+2, len(args)+3
		cond = append(cond, "(username LIKE "+r.placeholder(a)+" OR action LIKE "+r.placeholder(b)+" OR detail LIKE "+r.placeholder(c)+")")
		args = append(args, like, like, like)
	}
	if f.From != "" {
		args = append(args, f.From)
		cond = append(cond, "created_at >= "+r.placeholder(len(args)))
	}
	if f.To != "" {
		args = append(args, f.To)
		cond = append(cond, "created_at <= "+r.placeholder(len(args)))
	}
	where := ""
	if len(cond) > 0 {
		where = " WHERE " + strings.Join(cond, " AND ")
	}

	var total int
	if err := r.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+r.codec.table+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + joinColumns(r.codec.columns) + " FROM " + r.codec.table + where +
		" ORDER BY created_at DESC, id DESC LIMIT " + r.placeholder(len(args)+1) + " OFFSET " + r.placeholder(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.AuditLog
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}
