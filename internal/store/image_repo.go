package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type imageRepository struct {
	repository[domain.Image]
}

func (r imageRepository) GetByName(ctx context.Context, name string) (domain.Image, error) {
	v, err := r.codec.scan(r.q.QueryRowContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE name = "+r.placeholder(1),
		name,
	))
	return scanOne(v, err, r.codec.table, name)
}
