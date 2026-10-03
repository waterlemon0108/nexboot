-- +goose Up
-- A group can follow its config's current restore point instead of pinning
-- one, so "make this restore point current" (or a rollback) reaches every
-- following group without editing each. Groups recorded so far were pinned
-- only because that was the sole option; those sitting on their config's
-- current point are the ones an operator meant to follow, so they follow.
ALTER TABLE groups ADD COLUMN follow_current INTEGER NOT NULL DEFAULT 0;
UPDATE groups SET follow_current = 1
  WHERE system_reduction_id = (SELECT default_reduction_id FROM configs WHERE configs.id = groups.system_config_id);

-- +goose Down
ALTER TABLE groups DROP COLUMN follow_current;
