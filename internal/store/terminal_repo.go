package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type terminalRepository struct {
	repository[domain.Terminal]
}

func (r terminalRepository) GetByMAC(ctx context.Context, mac string) (domain.Terminal, error) {
	v, err := r.codec.scan(r.q.QueryRowContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE mac = "+r.placeholder(1),
		mac,
	))
	return scanOne(v, err, r.codec.table, mac)
}
