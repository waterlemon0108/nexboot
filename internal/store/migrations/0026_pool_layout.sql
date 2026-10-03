-- +goose Up
-- A pool's layout decides what adding and removing a disk may do, so the row
-- carries it. Every pool recorded before this is a stripe: that is the only
-- thing the product ever built. Kept as ADD COLUMN — pools has children with
-- ON DELETE CASCADE, so it is never rebuilt.
ALTER TABLE pools ADD COLUMN layout TEXT NOT NULL DEFAULT 'stripe' CHECK (layout IN ('stripe','mirror','raidz1','raidz2','raidz3'));
ALTER TABLE pools ADD COLUMN group_width INTEGER NOT NULL DEFAULT 1 CHECK (group_width >= 1);

-- pool_disks learns the vdev group each disk sits in and two more roles the
-- status parser reports (special, spare). Rebuilt to widen the CHECK; it has
-- no children of its own.
CREATE TABLE pool_disks_layout (
  id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL REFERENCES pools(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('data','read_cache','write_cache','special','spare')),
  vdev TEXT NOT NULL DEFAULT '',
  capacity INTEGER NOT NULL DEFAULT 0 CHECK (capacity >= 0),
  used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
  UNIQUE (pool_id, path, role)
);
INSERT INTO pool_disks_layout (id,pool_id,path,role,vdev,capacity,used)
  SELECT id,pool_id,path,role,'',capacity,used FROM pool_disks;
DROP TABLE pool_disks;
ALTER TABLE pool_disks_layout RENAME TO pool_disks;

-- +goose Down
CREATE TABLE pool_disks_no_layout (
  id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL REFERENCES pools(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('data','read_cache','write_cache')),
  capacity INTEGER NOT NULL DEFAULT 0 CHECK (capacity >= 0),
  used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
  UNIQUE (pool_id, path, role)
);
INSERT INTO pool_disks_no_layout (id,pool_id,path,role,capacity,used)
  SELECT id,pool_id,path,role,capacity,used FROM pool_disks
  WHERE role IN ('data','read_cache','write_cache');
DROP TABLE pool_disks;
ALTER TABLE pool_disks_no_layout RENAME TO pool_disks;
ALTER TABLE pools DROP COLUMN group_width;
ALTER TABLE pools DROP COLUMN layout;
