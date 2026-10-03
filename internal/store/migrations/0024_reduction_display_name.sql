-- +goose Up
-- A restore point's name is what the operator typed; the snapshot carrying it
-- has to be ascii because ZFS refuses anything else. Existing rows were named
-- by the snapshot itself, so they backfill from it — "@r1" reads as "r1".
ALTER TABLE reductions ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
UPDATE reductions SET display_name = ltrim(name, '@') WHERE display_name = '';

-- +goose Down
ALTER TABLE reductions DROP COLUMN display_name;
