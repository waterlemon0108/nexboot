package zfs

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// 布局 v2 把目录放在 <pool>/nd、每台机器的克隆放在 <pool>/run（原因见 storage/layout.go）。
// Client 拼的每个名字都经 storage.ContainerFor，调用方无需配合；这里只负责识别池处于哪种布局、
// 拒绝在旧布局上运行，以及两者之间的一次性离线迁移。

// layoutVersion 读池根的布局属性：容器建好后为 storage.LayoutV2，产品未标记的池为 ""。
func (c *Client) layoutVersion(ctx context.Context) (string, error) {
	args := []string{"get", "-H", "-o", "value", storage.LayoutProperty, c.pool}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return "", storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	value := strings.TrimSpace(string(out))
	if value == "-" {
		return "", nil
	}
	return value, nil
}

// topLevelDatasets 列出池的直接子数据集（散落数据集和容器都算），不含池本身。
func (c *Client) topLevelDatasets(ctx context.Context) ([]string, error) {
	args := []string{"list", "-H", "-d", "1", "-o", "name", c.pool}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == c.pool {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// keepTopLevel 判断 v2 下应留在池顶层的数据集：两个容器，以及导入目录。导入目录是已挂载的文件系统，
// 操作者和导入配置都指向 "/<pool>/imports"，挪进 mountpoint=none 的容器会让它中途被卸载。
func (c *Client) keepTopLevel(full string) bool {
	return full == path.Join(c.pool, storage.CatalogueRoot) ||
		full == path.Join(c.pool, storage.RunRoot) ||
		full == path.Join(c.pool, "imports")
}

func (c *Client) ensureContainers(ctx context.Context, existing []string) error {
	have := map[string]bool{}
	for _, name := range existing {
		have[name] = true
	}
	for _, root := range []string{storage.CatalogueRoot, storage.RunRoot} {
		full := path.Join(c.pool, root)
		if have[full] {
			continue
		}
		if err := c.run(ctx, "create", "-o", "mountpoint=none", full); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) stampLayout(ctx context.Context) error {
	return c.run(ctx, "set", storage.LayoutProperty+"="+storage.LayoutV2, c.pool)
}

// EnsureLayout 是启动关口：v2 池放行，空池原地初始化，顶层仍有散落数据集的是旧布局，
// 拒绝并给出修复命令。新程序跑在旧布局上会看到空目录，把已有镜像都当成不存在。
func (c *Client) EnsureLayout(ctx context.Context) error {
	version, err := c.layoutVersion(ctx)
	if err != nil {
		return err
	}
	tops, err := c.topLevelDatasets(ctx)
	if err != nil {
		return err
	}
	if version == storage.LayoutV2 {
		// 标记是 v2，但容器可能被手工删了；不补建的话，nd/ 没了的备机再也收不了复制。
		return c.ensureContainers(ctx, tops)
	}
	var loose []string
	for _, full := range tops {
		if !c.keepTopLevel(full) {
			loose = append(loose, full)
		}
	}
	if len(loose) > 0 {
		return fmt.Errorf("存储池 %s 使用旧版数据集布局。请先停止 ndiskless 服务,再执行: ndiskless migrate-layout (待迁移: %s)",
			c.pool, strings.Join(loose, ", "))
	}
	if err := c.ensureContainers(ctx, tops); err != nil {
		return err
	}
	return c.stampLayout(ctx)
}

// MigrateLayout 把顶层散落的数据集改名进对应容器并把池标为 v2，只改元数据，秒级完成。
// 只能离线使用：调用方须确认服务已停、无 iSCSI 会话，否则已导出的 LUN 会指向不存在的路径。
func (c *Client) MigrateLayout(ctx context.Context) (moved int, err error) {
	version, err := c.layoutVersion(ctx)
	if err != nil {
		return 0, err
	}
	tops, err := c.topLevelDatasets(ctx)
	if err != nil {
		return 0, err
	}
	var loose []string
	for _, full := range tops {
		if !c.keepTopLevel(full) {
			loose = append(loose, full)
		}
	}
	if version == storage.LayoutV2 && len(loose) == 0 {
		return 0, nil
	}
	if err := c.ensureContainers(ctx, tops); err != nil {
		return 0, err
	}
	for _, full := range loose {
		name := storage.DatasetID(full)
		target := path.Join(c.pool, storage.ContainerFor(name), name)
		if err := c.run(ctx, "rename", full, target); err != nil {
			return moved, err
		}
		moved++
	}
	if err := c.stampLayout(ctx); err != nil {
		return moved, err
	}
	return moved, nil
}

// EnsureDBCopyDataset 确保 <pool>/nd/db 存在并已挂载，返回 VACUUM INTO 写库副本的目录。
// 首次使用时才建，不在迁移时建。mountpoint 显式设为 DBCopyDir，因为上层容器是 mountpoint=none 且会被继承。
func (c *Client) EnsureDBCopyDataset(ctx context.Context) (string, error) {
	dataset := path.Join(c.pool, storage.CatalogueRoot, "db")
	mountpoint := DBCopyDir(c.pool)
	args := []string{"get", "-H", "-o", "value", "mountpoint", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err == nil {
		dir := strings.TrimSpace(string(out))
		// 不是本节点自己的路径就改回来。除了未设置，还可能是 `zfs send -R` 带来的发送端路径
		// （含对方池名）；布局属于本节点，收到什么都要改回。
		if dir != mountpoint {
			if err := c.run(ctx, "set", "mountpoint="+mountpoint, dataset); err != nil {
				return "", err
			}
			// 这里不能返回：`zfs set mountpoint=` 只在旧值为 none/legacy 或已挂载时才顺带挂载，
			// 而备机副本经 recv -u 收下，两者都不满足；只看属性就返回，副本仍不在盘上。
			dir = mountpoint
		}
		// 备机副本经 recv -u 收下时 mountpoint 已设但未挂载，未挂载的副本读起来是空目录，
		// 激活时会误判为「库副本从未到达」。
		mountedOut, mountedErr := c.runner.Run(ctx, "zfs", "get", "-H", "-o", "value", "mounted", dataset)
		isMounted := mountedErr == nil && strings.TrimSpace(string(mountedOut)) == "yes"
		if !isMounted {
			if err := c.run(ctx, "mount", dataset); err != nil {
				return "", err
			}
			return dir, nil
		}
		// 已挂载不等于可用：`recv -F` 在挂载中回滚数据集会让内核留下陈旧挂载，属性仍为 yes，读取却返回 EIO。
		// 只信属性会让备机永远卡在「库副本未到达」，失去接任资格。
		if statErr := c.readable(dir); statErr != nil {
			_, _ = c.runner.Run(ctx, "zfs", "unmount", "-f", dataset)
			if err := c.run(ctx, "mount", dataset); err != nil {
				return "", err
			}
		}
		return dir, nil
	}
	getErr := storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	if !IsNotExist(getErr) {
		return "", getErr
	}
	if err := c.run(ctx, "create", "-p", "-o", "mountpoint="+mountpoint, dataset); err != nil {
		return "", err
	}
	return mountpoint, nil
}
