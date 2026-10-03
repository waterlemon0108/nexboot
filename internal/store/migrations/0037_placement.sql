-- +goose Up
-- Placement: a group may pin its clients to a storage node; a terminal
-- records which node last served it, so cleanup finds the clone wherever the
-- boot put it. NULL = this node (the active), today's behaviour.
ALTER TABLE groups ADD COLUMN storage_server_id TEXT REFERENCES servers(id);
ALTER TABLE terminals ADD COLUMN storage_server_id TEXT;

-- +goose Down
ALTER TABLE groups DROP COLUMN storage_server_id;
ALTER TABLE terminals DROP COLUMN storage_server_id;
