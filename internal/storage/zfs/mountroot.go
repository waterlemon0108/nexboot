package zfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// MountRoot 是产品管理的所有池的挂载根。用 ZFS 默认的 /<name> 会与系统目录抢名字：
// 叫 srv、mnt 的池会盖住系统目录，叫 opt 的建不出来，旧池留下的空挂载目录还会挡住同名新池。
const MountRoot = "/ndiskless"

// PoolMountpoint 返回 pool 的挂载点。
func PoolMountpoint(pool string) string { return path.Join(MountRoot, pool) }

// DBCopyDir 返回 pool 的目录库副本挂载目录。
func DBCopyDir(pool string) string {
	return path.Join(MountRoot, pool, storage.CatalogueRoot, "db")
}

// MigrateMountpoints 把仍在 ZFS 默认 /<name> 的池移到 MountRoot 下，返回被移动的池。
// 已在其下或按设计不挂载（none、legacy）的池不动。
func (c *Client) MigrateMountpoints(ctx context.Context) ([]string, error) {
	names, err := c.ListPoolNames(ctx)
	if err != nil {
		return nil, err
	}
	var moved []string
	var errs []error
	for _, name := range names {
		out, err := c.runner.Run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", name)
		if err != nil || strings.TrimSpace(string(out)) != "/"+name {
			continue
		}
		if err := c.moveMounts(ctx, name, "/"+name, PoolMountpoint(name)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		moved = append(moved, name)
	}
	return moved, errors.Join(errs...)
}

type mountState struct {
	name, mountpoint, source string
	mounted                  bool
}

// moveMounts 把 pool 从 old 改指到 want。旧树里有自己 mountpoint 的子集（库副本）会占住池根，
// 所以先由深到浅全部卸载再改指。`zfs set mountpoint` 在 ZFS 2.2 上会自行挂到新路径；
// 之后再按先父后子挂载一遍是为不这样做的版本兜底，报 "already mounted" 属预期。
func (c *Client) moveMounts(ctx context.Context, pool, old, want string) error {
	out, err := c.runner.Run(ctx, "zfs", "get", "-H", "-r", "-t", "filesystem",
		"-o", "name,property,value,source", "mountpoint,mounted", pool)
	if err != nil {
		return err
	}
	var order []string
	states := map[string]*mountState{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		st, ok := states[f[0]]
		if !ok {
			st = &mountState{name: f[0]}
			states[f[0]] = st
			order = append(order, f[0])
		}
		switch f[1] {
		case "mountpoint":
			st.mountpoint, st.source = f[2], f[3]
		case "mounted":
			st.mounted = f[2] == "yes"
		}
	}
	var unmounted []string
	remount := func() {
		for i := len(unmounted) - 1; i >= 0; i-- {
			_ = c.run(ctx, "mount", unmounted[i])
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if st := states[order[i]]; st.mounted {
			if err := c.run(ctx, "unmount", st.name); err != nil {
				remount()
				return err
			}
			unmounted = append(unmounted, st.name)
		}
	}
	if err := c.run(ctx, "set", "mountpoint="+want, pool); err != nil {
		remount()
		return err
	}
	for _, name := range order[1:] {
		st := states[name]
		if st.source == "local" && strings.HasPrefix(st.mountpoint, old+"/") {
			if err := c.run(ctx, "set", "mountpoint="+want+strings.TrimPrefix(st.mountpoint, old), name); err != nil {
				c.logger.Warn("could not move a dataset's mountpoint", "dataset", name, "error", err)
			}
		}
	}
	remount()
	c.removeEmpty(old)
	return nil
}

// removeEmpty 删除 dir 及其下所有空目录；含文件的不删，那不归我们管。
func (c *Client) removeEmpty(dir string) {
	if c.removeEmptyDirs != nil {
		c.removeEmptyDirs(dir)
		return
	}
	removeEmptyTree(dir)
}

func removeEmptyTree(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	empty := true
	for _, e := range entries {
		if !e.IsDir() || !removeEmptyTree(filepath.Join(dir, e.Name())) {
			empty = false
		}
	}
	return empty && os.Remove(dir) == nil
}

// ProductPoolNames 列出产品建立或接管的池，即挂在 PoolMountpoint 下的池（产品建的池都是，
// 旧池由 MigrateMountpoints 移过去）。ZFS 根池或操作者另作他用的池不归产品登记。
func (c *Client) ProductPoolNames(ctx context.Context) ([]string, error) {
	names, err := c.ListPoolNames(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		mp, err := c.runner.Run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", name)
		if err == nil && strings.TrimSpace(string(mp)) == PoolMountpoint(name) {
			out = append(out, name)
		}
	}
	return out, nil
}
