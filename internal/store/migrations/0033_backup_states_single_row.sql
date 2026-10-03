-- +goose Up
-- The backup no longer ships datasets one by one: the whole catalogue
-- container travels as a single replication stream, and its incremental base
-- is read from the two pools each round. Per-dataset state rows are dead
-- records from the old scheme — and their stored bases are exactly what a
-- merge or a super save silently invalidated.
DELETE FROM backup_state;

-- +goose Down
-- Nothing to restore: the old rows carried per-dataset incremental bases the
-- new code never reads, and the next backup rebuilds its state on its own.
