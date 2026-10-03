-- +goose Up
-- No FK to driver_bundles: this is an advisory marker only (last-injected,
-- not-yet-baked bundle) and must not block bundle deletion or leave dangling
-- references awkward to clean up.
ALTER TABLE terminals ADD COLUMN pending_bundle_id TEXT;
ALTER TABLE terminals ADD COLUMN pending_bundle_at TEXT;

-- +goose Down
ALTER TABLE terminals DROP COLUMN pending_bundle_at;
ALTER TABLE terminals DROP COLUMN pending_bundle_id;
