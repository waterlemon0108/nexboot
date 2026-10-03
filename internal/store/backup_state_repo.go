package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type backupStateRepository struct {
	repository[domain.BackupState]
}

func (r backupStateRepository) Upsert(ctx context.Context, value domain.BackupState) error {
	if value.ID == "" {
		value.ID = value.SourceName
	}
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO backup_state (id, source_name, last_snapshot, updated_at) VALUES ("+r.placeholders(1, 4)+")"+
			" ON CONFLICT(id) DO UPDATE SET source_name = excluded.source_name, last_snapshot = excluded.last_snapshot, updated_at = excluded.updated_at",
		value.ID,
		value.SourceName,
		value.LastSnapshot,
		timeValue(value.UpdatedAt),
	)
	return err
}
