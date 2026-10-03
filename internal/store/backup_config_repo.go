package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type backupConfigRepository struct {
	repository[domain.BackupConfig]
}

func (r backupConfigRepository) Upsert(ctx context.Context, value domain.BackupConfig) error {
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO backup_config (id, backup_pool, enabled, schedule, last_run_at) VALUES ("+r.placeholders(1, 5)+")"+
			" ON CONFLICT(id) DO UPDATE SET backup_pool = excluded.backup_pool, enabled = excluded.enabled, schedule = excluded.schedule, last_run_at = excluded.last_run_at",
		r.codec.values(value)...,
	)
	return err
}
