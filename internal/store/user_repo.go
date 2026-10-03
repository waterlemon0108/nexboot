package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type userRepository struct {
	repository[domain.User]
}

func (r userRepository) GetByUsername(ctx context.Context, username string) (domain.User, error) {
	v, err := r.codec.scan(r.q.QueryRowContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE username = "+r.placeholder(1),
		username,
	))
	return scanOne(v, err, r.codec.table, username)
}
