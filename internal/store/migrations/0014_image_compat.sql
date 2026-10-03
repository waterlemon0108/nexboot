-- +goose Up
CREATE TABLE image_compat (
  id TEXT PRIMARY KEY,
  reduction_id TEXT NOT NULL REFERENCES reductions(id) ON DELETE CASCADE,
  profile_id TEXT NOT NULL REFERENCES hardware_profiles(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK (status IN ('verified','injected_unverified','incompatible','unknown')),
  verified_at TEXT,
  task_ref TEXT NOT NULL DEFAULT '',
  remark TEXT NOT NULL DEFAULT '',
  UNIQUE (reduction_id, profile_id)
);

-- +goose Down
DROP TABLE image_compat;
