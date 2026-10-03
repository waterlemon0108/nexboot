package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type bootFailureRepository struct {
	repository[domain.BootFailure]
}

var bootFailureCodec = entityCodec[domain.BootFailure]{
	table:   "boot_failures",
	columns: []string{"mac", "stage", "code", "platform", "image_id", "at"},
	values: func(v domain.BootFailure) []any {
		return []any{v.MAC, v.Stage, v.Code, v.Platform, v.ImageID, timeValue(v.At)}
	},
	scan: func(s rowScanner) (domain.BootFailure, error) {
		var v domain.BootFailure
		var at string
		if err := s.Scan(&v.MAC, &v.Stage, &v.Code, &v.Platform, &v.ImageID, &at); err != nil {
			return v, err
		}
		t, err := parseTime(at)
		v.At = t
		return v, err
	},
}

func (r bootFailureRepository) List(ctx context.Context) ([]domain.BootFailure, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+joinColumns(r.codec.columns)+" FROM boot_failures ORDER BY mac")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.BootFailure
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r bootFailureRepository) Upsert(ctx context.Context, v domain.BootFailure) error {
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO boot_failures ("+joinColumns(r.codec.columns)+") VALUES ("+r.placeholders(1, 6)+")"+
			" ON CONFLICT(mac) DO UPDATE SET stage = excluded.stage, code = excluded.code, platform = excluded.platform,"+
			" image_id = excluded.image_id, at = excluded.at",
		r.codec.values(v)...,
	)
	return err
}
