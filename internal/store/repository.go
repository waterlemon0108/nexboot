package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound 在单行查询中替代 sql.ErrNoRows，store 之外的调用方因此不依赖 database/sql。
var ErrNotFound = errors.New("not found")

// scanOne 包装单行扫描结果，把 sql.ErrNoRows 转成带表名和查询键的 ErrNotFound。
func scanOne[T any](v T, err error, table, key string) (T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("%s %q: %w", table, key, ErrNotFound)
	}
	return v, err
}

type rowScanner interface {
	Scan(...any) error
}

type entityCodec[T any] struct {
	table   string
	columns []string
	values  func(T) []any
	scan    func(rowScanner) (T, error)
}

type repository[T any] struct {
	q       queryer
	dialect string
	codec   entityCodec[T]
}

func newRepository[T any](q queryer, dialect string, codec entityCodec[T]) repository[T] {
	return repository[T]{q: q, dialect: dialect, codec: codec}
}

func (r repository[T]) Create(ctx context.Context, v T) error {
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO "+r.codec.table+" ("+strings.Join(r.codec.columns, ",")+") VALUES ("+r.placeholders(1, len(r.codec.columns))+")",
		r.codec.values(v)...,
	)
	return err
}

func (r repository[T]) Get(ctx context.Context, id string) (T, error) {
	v, err := r.codec.scan(r.q.QueryRowContext(ctx,
		"SELECT "+strings.Join(r.codec.columns, ",")+" FROM "+r.codec.table+" WHERE id = "+r.placeholder(1),
		id,
	))
	return scanOne(v, err, r.codec.table, id)
}

func (r repository[T]) List(ctx context.Context) ([]T, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+strings.Join(r.codec.columns, ",")+" FROM "+r.codec.table+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []T
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r repository[T]) Update(ctx context.Context, v T) error {
	sets := make([]string, 0, len(r.codec.columns)-1)
	for i, col := range r.codec.columns[1:] {
		sets = append(sets, col+" = "+r.placeholder(i+1))
	}
	values := r.codec.values(v)
	args := append(append([]any{}, values[1:]...), values[0])
	_, err := r.q.ExecContext(ctx,
		"UPDATE "+r.codec.table+" SET "+strings.Join(sets, ",")+" WHERE id = "+r.placeholder(len(args)),
		args...,
	)
	return err
}

func (r repository[T]) Delete(ctx context.Context, id string) error {
	_, err := r.q.ExecContext(ctx, "DELETE FROM "+r.codec.table+" WHERE id = "+r.placeholder(1), id)
	return err
}

func (r repository[T]) placeholders(start, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = r.placeholder(start + i)
	}
	return strings.Join(out, ",")
}

func joinColumns(columns []string) string {
	return strings.Join(columns, ",")
}

func (r repository[T]) placeholder(n int) string {
	if r.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func isNotFound(err error) bool {
	return err == sql.ErrNoRows
}
