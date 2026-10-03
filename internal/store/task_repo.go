package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type taskRepository struct {
	repository[domain.Task]
}

func (r taskRepository) ListActive(ctx context.Context) ([]domain.Task, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+
			" WHERE status IN ('pending','running') ORDER BY created_at",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Task
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListPaged 按新到旧返回任务（可按状态过滤），以及匹配过滤条件的总行数（用于分页）。
func (r taskRepository) ListPaged(ctx context.Context, status string, limit, offset int) ([]domain.Task, int, error) {
	where := ""
	var args []any
	if status != "" {
		where = " WHERE status = " + r.placeholder(1)
		args = append(args, status)
	}

	var total int
	if err := r.q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+r.codec.table+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + joinColumns(r.codec.columns) + " FROM " + r.codec.table + where +
		" ORDER BY created_at DESC LIMIT " + r.placeholder(len(args)+1) + " OFFSET " + r.placeholder(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.Task
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}
