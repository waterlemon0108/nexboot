package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type replicationStateRepository struct {
	repository[domain.ReplicationState]
}

// List 覆盖通用实现：这张表的键是 target 而不是 id。
func (r replicationStateRepository) List(ctx context.Context) ([]domain.ReplicationState, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT target, kind, root, last_snapshot, last_ok_at, last_error, updated_at FROM replication_state ORDER BY target")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReplicationState
	for rows.Next() {
		v, err := replicationStateCodec.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r replicationStateRepository) Upsert(ctx context.Context, value domain.ReplicationState) error {
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO replication_state (target, kind, root, last_snapshot, last_ok_at, last_error, updated_at) VALUES ("+r.placeholders(1, 7)+")"+
			" ON CONFLICT(target) DO UPDATE SET kind = excluded.kind, root = excluded.root, last_snapshot = excluded.last_snapshot,"+
			" last_ok_at = excluded.last_ok_at, last_error = excluded.last_error, updated_at = excluded.updated_at",
		value.Target, value.Kind, value.Root, value.LastSnapshot,
		timePtrValue(value.LastOKAt), value.LastError, timeValue(value.UpdatedAt),
	)
	return err
}
