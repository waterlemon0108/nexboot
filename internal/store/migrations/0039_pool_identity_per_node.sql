-- +goose Up
-- 池 ID 原来只由池名派生（pool-<name>）。数据池按惯例每台都叫 tank，而目录库
-- 是整体复制的——三台的 pools 表是同一张表，于是三个池压成一行，谁最后写谁就
-- 成了主人，另外两台在界面上没有任何存储，尽管它们的盘正给客户机供着。
--
-- 这里把已有的记录改成按节点区分的 ID（pool-<node>--<name>）。只能改到「已经
-- 在表里的那一行」；其余节点的池本来就没有记录，它们会在下次启动时由既有的
-- 「接管产品外准备好的数据池」流程用新规则各自登记一行。
-- pool_disks.pool_id 有外键指向 pools(id)，且只声明了 ON DELETE CASCADE。改 ID
-- 时父子两张表必然有一瞬间对不上：先改子表，父行还是旧 ID；先改父表，子行指向
-- 的父行已经没了。把外键检查推迟到提交时，两张表一起改完再校验。
PRAGMA defer_foreign_keys = ON;

UPDATE pool_disks
SET pool_id = 'pool-' || (SELECT server_id FROM pools WHERE pools.id = pool_disks.pool_id) || '--'
              || substr(pool_disks.pool_id, 6)
WHERE EXISTS (
  SELECT 1 FROM pools
  WHERE pools.id = pool_disks.pool_id
    AND pools.server_id <> ''
    AND pools.id NOT LIKE 'pool-' || pools.server_id || '--%'
);

UPDATE pools
SET id = 'pool-' || server_id || '--' || substr(id, 6)
WHERE server_id <> ''
  AND id NOT LIKE 'pool-' || server_id || '--%';
