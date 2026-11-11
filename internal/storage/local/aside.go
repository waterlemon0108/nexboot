package local

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// DeleteAsideImage 从重建留下的旧副本里删掉操作者明确删除的镜像和配置，新镜像及其依赖的基线、共享的文件和数据库保留。
// 分叉副本不碰：它可能装着失联期间写下的内容，只能由操作者确认后删。正被重建当作退路的副本也不碰。
func (a *Agent) DeleteAsideImage(ctx context.Context, imageID string, configIDs []string) error {
	ids := append(append([]string{}, configIDs...), imageID)
	for _, id := range ids {
		if !identitySourcePart.MatchString(id) || id == "." || id == ".." || storage.Classify(id).Kind != storage.CarrierEntity || storage.ReservedCatalogueChild(id) {
			return fmt.Errorf("旧副本清理对象无效，请核对镜像或配置：%s", id)
		}
	}
	unlock, err := storage.HoldAsideCopies(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	root := a.zfs.PoolName() + "/" + storage.CatalogueRoot
	asides, err := a.zfs.ListAsideCopies(ctx, root)
	if err != nil {
		return err
	}
	var failed []error
	for _, aside := range asides {
		if !isRebuildingRoot(aside, root) || storage.AsideInUse(aside) {
			continue
		}
		targets := make([]string, 0, len(ids))
		for _, id := range ids {
			targets = append(targets, aside+"/"+id)
		}
		if _, err := a.ReleaseIdentityLayers(ctx, targets); err != nil {
			failed = append(failed, err)
			continue
		}
		top, err := readTopology(ctx, a.zfs)
		if err != nil {
			return err
		}
		// Unknown entity clones may be newly saved images, never infer that they are configs.
		// Preserve their source chain, including authorized objects needed by that new image.
		authorized := make(map[string]bool, len(targets))
		protected := make(map[string]bool)
		dependencies := make(map[string][]string, len(targets))
		for _, dataset := range targets {
			authorized[dataset] = true
		}
		for _, dataset := range targets {
			deps, err := a.zfs.ListDependentClones(ctx, dataset)
			if err != nil {
				return err
			}
			dependencies[dataset] = deps
			for _, dep := range deps {
				if authorized[dep] {
					continue
				}
				if storage.Classify(path.Base(dep)).Kind != storage.CarrierEntity {
					return fmt.Errorf("请先关闭或结束使用旧副本的客户机/任务 %s，再删除镜像 %s", dep, imageID)
				}
				protected[dataset] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for _, dataset := range targets {
				for _, dep := range dependencies[dataset] {
					if protected[dep] && !protected[dataset] {
						protected[dataset], changed = true, true
					}
				}
			}
		}
		pending := targets[:0]
		for _, dataset := range targets {
			if !protected[dataset] {
				pending = append(pending, dataset)
			}
		}
		targets = pending
		for len(targets) > 0 {
			var stuck []string
			var lastErr error
			for _, dataset := range targets {
				var child string
				for _, other := range top.datasets() {
					if strings.HasPrefix(other, dataset+"/") {
						child = other
						break
					}
				}
				if child != "" {
					lastErr = fmt.Errorf("请先核对旧副本中的派生数据 %s，未删除 %s", child, dataset)
					stuck = append(stuck, dataset)
					continue
				}
				if err := top.ensureDestroyed(ctx, dataset); err != nil {
					stuck = append(stuck, dataset)
					lastErr = err
				}
			}
			if len(stuck) == len(targets) {
				failed = append(failed, lastErr)
				break
			}
			targets = stuck
		}
		// A newly saved image must not be lost to the next whole-tree rebuilding prune.
		if len(protected) > 0 {
			preserved := root + "-diverged-" + strings.TrimPrefix(aside, root+"-rebuilding-")
			if err := a.zfs.Rename(ctx, aside, preserved); err != nil {
				failed = append(failed, fmt.Errorf("请先核对新镜像所在旧副本 %s，保留改名为 %s 失败：%w", aside, preserved, err))
			}
		}
	}
	return errors.Join(failed...)
}

// isRebuildingRoot 只认存储层生成的确切名字，不认任意前缀或其子数据集。
func isRebuildingRoot(dataset, root string) bool {
	suffix, ok := strings.CutPrefix(dataset, root+"-rebuilding-")
	return ok && suffix != "" && strings.Trim(suffix, "0123456789") == ""
}
