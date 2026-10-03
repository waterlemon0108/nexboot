package local

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tianwei/diskless/internal/storage/zfs"
)

// topology 承载池编辑（配置合并进镜像、超管机克隆替换配置）：这些是没有事务的破坏性 ZFS 步骤，
// 中断后只能整体重跑，盲目重放会毁掉上次已挪走的数据。所以每一步声明目标状态，池已处于该状态即视为完成。
// 只在开始时读一次池，之后由 topology 记录本次编辑已做的变化，不必重读。
type topology struct {
	z       ZFS
	present map[string]bool
}

func readTopology(ctx context.Context, z ZFS) (*topology, error) {
	datasets, err := z.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(datasets))
	for _, dataset := range datasets {
		present[dataset] = true
	}
	return &topology{z: z, present: present}, nil
}

func (t *topology) has(dataset string) bool { return t.present[dataset] }

// datasets 返回截至当前步骤池中的数据集（已排序）。
func (t *topology) datasets() []string {
	out := make([]string, 0, len(t.present))
	for dataset := range t.present {
		out = append(out, dataset)
	}
	sort.Strings(out)
	return out
}

func (t *topology) ensureDestroyed(ctx context.Context, dataset string) error {
	if !t.has(dataset) {
		return nil
	}
	// 与 ensureRenamed 相同的短暂占用，见那里。
	err := retryWhileBusy(ctx, func() error { return t.z.Destroy(ctx, dataset) })
	if err != nil && !zfs.IsNotExist(err) {
		return err
	}
	delete(t.present, dataset)
	return nil
}

// retryWhileBusy 在 ZFS 报数据集被占用时重试，窗口与 destroyClone 一致。
func retryWhileBusy(ctx context.Context, op func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := op()
		if err == nil || !zfs.IsBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensureRenamed 让数据集落在 `to`。只有 `to` 说明上次已做完；两者都在则是两个不同的数据集
// （其一是早先尝试留下的），重命名上去会失败，容忍 "already exists" 又会让源数据静默留在原处，所以报错。
func (t *topology) ensureRenamed(ctx context.Context, from, to string) error {
	switch {
	case !t.has(from) && t.has(to):
		return nil
	case !t.has(from):
		return fmt.Errorf("cannot rename %s: neither it nor %s exists", from, to)
	case t.has(to):
		return fmt.Errorf("cannot rename %s onto %s: %s already exists and is not this rename's result", from, to, to)
	}
	// 新建的 zvol 被 udev 扫描的几百毫秒里 ZFS 报 busy，不重试则新盘首次发布会失败。
	if err := retryWhileBusy(ctx, func() error { return t.z.Rename(ctx, from, to) }); err != nil {
		return err
	}
	delete(t.present, from)
	t.present[to] = true
	return nil
}

// promotedOver 判断克隆是否已位于 over 中某个数据集之上，即上次已完成 promote。
// 单独提供是因为已 promote 的编辑还要跳过为 promote 做准备的步骤。
func (t *topology) promotedOver(ctx context.Context, clone string, over ...string) (bool, error) {
	children, err := t.z.ListDependentClones(ctx, clone)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		for _, want := range over {
			if child == want {
				return true, nil
			}
		}
	}
	return false, nil
}

// ensurePromoted 让克隆成为链根。第二次 promote 报的是 "conflicting snapshot" 而非
// "not a cloned filesystem"，无法靠容忍错误做成幂等，所以先用 promotedOver 询问。
func (t *topology) ensurePromoted(ctx context.Context, clone string, over ...string) error {
	promoted, err := t.promotedOver(ctx, clone, over...)
	if err != nil || promoted {
		return err
	}
	if err := t.z.Promote(ctx, clone); err != nil && !zfs.IsNotClone(err) {
		return err
	}
	return nil
}

func (t *topology) ensureSnapshot(ctx context.Context, dataset, snapshot string) error {
	if err := t.z.SnapshotVolume(ctx, dataset, snapshot); err != nil && !zfs.IsExists(err) {
		return err
	}
	return nil
}

func (t *topology) ensureSnapshotDestroyed(ctx context.Context, dataset, snapshot string) error {
	if err := t.z.DestroySnapshot(ctx, dataset, snapshot); err != nil && !zfs.IsNotExist(err) {
		return err
	}
	return nil
}

func (t *topology) ensureCloned(ctx context.Context, snapshot, dataset string) error {
	if t.has(dataset) {
		return nil
	}
	if err := t.z.Clone(ctx, snapshot, dataset); err != nil && !zfs.IsExists(err) {
		return err
	}
	t.present[dataset] = true
	return nil
}
