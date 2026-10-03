package store

import (
	"context"

	"github.com/tianwei/diskless/internal/domain"
)

type systemSettingsRepository struct {
	repository[domain.SystemSettings]
}

func (r systemSettingsRepository) Upsert(ctx context.Context, value domain.SystemSettings) error {
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO system_settings (id, import_dir, client_iface, allow_cross_subnet, replication_rate_mbps, data_pool) VALUES ("+r.placeholders(1, 6)+")"+
			" ON CONFLICT(id) DO UPDATE SET import_dir = excluded.import_dir, client_iface = excluded.client_iface, allow_cross_subnet = excluded.allow_cross_subnet, replication_rate_mbps = excluded.replication_rate_mbps, data_pool = excluded.data_pool",
		r.codec.values(value)...,
	)
	return err
}
