-- +goose Up
-- The identity split (issue 041): a server row now carries its real
-- coordinates — where clients reach its disks (portal_ip), where peers call
-- its API (api_url), when it was last seen alive — instead of the id string
-- repeated in every column. epoch and ha_state sit ready for the HA pair.
ALTER TABLE servers ADD COLUMN portal_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN api_url TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN ha_state TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN last_seen_at TEXT;

-- +goose Down
ALTER TABLE servers DROP COLUMN portal_ip;
ALTER TABLE servers DROP COLUMN api_url;
ALTER TABLE servers DROP COLUMN epoch;
ALTER TABLE servers DROP COLUMN ha_state;
ALTER TABLE servers DROP COLUMN last_seen_at;
