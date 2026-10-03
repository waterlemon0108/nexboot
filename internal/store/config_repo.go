package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type configRepository struct {
	repository[domain.Config]
}

func (r configRepository) ListByImage(ctx context.Context, imageID string) ([]domain.Config, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE image_id = "+r.placeholder(1)+" ORDER BY id",
		imageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Config
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
