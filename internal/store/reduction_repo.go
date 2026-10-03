package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type reductionRepository struct {
	repository[domain.Reduction]
}

func (r reductionRepository) ListByConfig(ctx context.Context, configID string) ([]domain.Reduction, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE config_id = "+r.placeholder(1)+" ORDER BY created_at,id",
		configID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Reduction
	for rows.Next() {
		v, err := r.codec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r reductionRepository) DeleteByConfig(ctx context.Context, configID string) error {
	_, err := r.q.ExecContext(ctx, "DELETE FROM "+r.codec.table+" WHERE config_id = "+r.placeholder(1), configID)
	return err
}
