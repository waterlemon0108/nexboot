package local

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/winreg"
)

func (a *Agent) CreateIdentityLayer(ctx context.Context, req storage.IdentityLayerReq) error {
	unlock := a.lockClient(req.MAC)
	defer unlock()
	dataset := a.zfs.Dataset(storage.IdentityCloneName(req.MAC))
	source := a.zfs.Snapshot(req.ConfigID, req.SnapshotName)
	origins, err := a.zfs.ListOrigins(ctx)
	if err != nil {
		return err
	}
	key := req.TemplateInstanceID + "/" + req.InstanceID + "/" + req.ClassKey
	if req.Template != nil {
		key += "/tpl/" + req.Template.GUID + "/" + req.Template.Reg
		if req.Template.Generate {
			key += "/gen/" + req.Template.Vendor + req.Template.Device + "/" + req.Template.DriverStamp + "/v" + winreg.GeneratorVersion
		}
	}
	sum := sha256.Sum256([]byte(key))
	marker := fmt.Sprintf("ndidentity-%x", sum[:8])
	valid, err := a.snapshotExists(ctx, dataset, marker)
	if err != nil {
		return err
	}
	ready, err := a.snapshotExists(ctx, dataset, "ready")
	if err != nil {
		return err
	}
	if origins[dataset] == source && ready && valid {
		if !req.Background {
			a.provisionedAt.Store(storage.NormalizeMAC(req.MAC), time.Now())
		}
		return nil
	}
	if req.Background {
		if last, ok := a.provisionedAt.Load(storage.NormalizeMAC(req.MAC)); ok && time.Since(last.(time.Time)) < 90*time.Second {
			return fmt.Errorf("客户机 %s 刚开始启动，请稍后重建身份层", req.MAC)
		}
	}
	if a.exporter != nil {
		macs, err := a.exporter.ActiveSessions(ctx)
		if err != nil {
			return err
		}
		for _, mac := range macs {
			if strings.EqualFold(storage.NormalizeMAC(mac), storage.NormalizeMAC(req.MAC)) {
				return fmt.Errorf("请关闭客户机 %s 后重建身份层", req.MAC)
			}
		}
	}
	// CLIENT clones may depend on old @ready. They belong to this offline MAC and are removed before the identity layer.
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	if err := a.teardownForBoot(ctx, storage.ClientReq{MAC: req.MAC}, datasets); err != nil {
		return err
	}
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return err
	}
	if err := top.ensureDestroyed(ctx, dataset); err != nil {
		return err
	}
	if err := a.zfs.Clone(ctx, source, dataset); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = a.destroyClone(context.Background(), dataset)
		}
	}()
	if a.identityWriteFn != nil {
		err = a.identityWriteFn(ctx, dataset, req)
	} else {
		r := a.runner
		if r == nil {
			r = execRunner{}
		}
		err = withWindowsPartition(ctx, r, a.zfs.VolumePath(dataset), volRW, clonePartitionWait, func(root string) error {
			hive, ok := lookupPath(root, "Windows/System32/config/SYSTEM")
			if !ok {
				hive = filepath.Join(root, "Windows", "System32", "config", "SYSTEM")
			}
			// 断电保存的旧还原点可能带未提交的 SYSTEM：只在本终端的克隆上回放，不碰还原点本身。
			if _, err := winreg.RecoverHive(ctx, hive); err != nil {
				return err
			}
			if req.Template != nil && req.Template.Generate && req.Template.Reg == "" {
				return winreg.ApplyGenerated(ctx, root, hive, winreg.GenerateSpec{
					Instance: req.InstanceID, GUID: req.Template.GUID, MAC: storage.NormalizeMAC(req.MAC),
					Vendor: req.Template.Vendor, Device: req.Template.Device, DriverZip: req.Template.DriverZip,
				})
			}
			if req.Template != nil {
				return winreg.ApplyTemplate(ctx, hive, req.Template.Reg, winreg.TemplateParams{Instance: req.InstanceID, GUID: req.Template.GUID, MAC: storage.NormalizeMAC(req.MAC)})
			}
			return winreg.CopyIdentity(ctx, hive, req.TemplateInstanceID, req.InstanceID, req.ClassKey)
		})
	}
	if err != nil {
		return err
	}
	if err := a.zfs.SnapshotVolume(ctx, dataset, "ready"); err != nil {
		return err
	}
	if err := a.zfs.SnapshotVolume(ctx, dataset, marker); err != nil {
		return err
	}
	ok = true
	// Protect the interval before BootService next asks CreateClientLUN to use this @ready.
	if !req.Background {
		a.provisionedAt.Store(storage.NormalizeMAC(req.MAC), time.Now())
	}
	return nil
}
func (a *Agent) DeleteIdentityLayer(ctx context.Context, mac string) error {
	unlock := a.lockClient(mac)
	defer unlock()
	if a.exporter != nil {
		sessions, err := a.exporter.ActiveSessions(ctx)
		if err != nil {
			return err
		}
		for _, active := range sessions {
			if storage.NormalizeMAC(active) == storage.NormalizeMAC(mac) {
				return fmt.Errorf("请先关闭客户机 %s，再清理旧节点身份层", mac)
			}
		}
	}
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	if err := a.teardownForBoot(ctx, storage.ClientReq{MAC: mac}, datasets); err != nil {
		return err
	}
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return err
	}
	return top.ensureDestroyed(ctx, a.zfs.Dataset(storage.IdentityCloneName(mac)))
}

// ReleaseIdentityLayers releases only unused local identity caches pinned to the expected sources.
// All lifecycle checks and the origin re-read run under the same MAC lock as boot provisioning.
// Matching busy layers return an error so remote callers cannot mistake a skip for completed cleanup.
func (a *Agent) ReleaseIdentityLayers(ctx context.Context, gone []string) ([]string, error) {
	if len(gone) == 0 {
		return nil, nil
	}
	var err error
	gone, err = a.identitySources(gone)
	if err != nil {
		return nil, err
	}
	if a.exporter == nil {
		return nil, storage.ErrNotImplemented
	}
	origins, err := a.zfs.ListOrigins(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for dataset, origin := range origins {
		carrier := storage.Classify(datasetName(dataset))
		if carrier.Kind != storage.CarrierIdentity || !installMAC.MatchString(carrier.MAC) || dataset != a.zfs.Dataset(storage.IdentityCloneName(carrier.MAC)) || !pinnedBy(origin, gone) {
			continue
		}
		candidates = append(candidates, carrier.MAC)
	}
	sort.Strings(candidates)
	var released []string
	for _, mac := range candidates {
		done, err := a.releaseIdentityLayer(ctx, mac, gone)
		if err != nil {
			return released, err
		}
		if done {
			released = append(released, mac)
		}
	}
	return released, nil
}

func (a *Agent) releaseIdentityLayer(ctx context.Context, mac string, gone []string) (bool, error) {
	unlock := a.lockClient(mac)
	defer unlock()
	origins, err := a.zfs.ListOrigins(ctx)
	if err != nil {
		return false, err
	}
	dataset := a.zfs.Dataset(storage.IdentityCloneName(mac))
	origin := origins[dataset]
	if !pinnedBy(origin, gone) {
		return false, nil
	}
	if last, ok := a.provisionedAt.Load(mac); ok && time.Since(last.(time.Time)) < 90*time.Second {
		return false, fmt.Errorf("请等客户机 %s 本轮开机准备完成后再重试：身份层仍在使用 %s", mac, origin)
	}
	sessions, err := a.ActiveClientMACs(ctx)
	if err != nil {
		return false, err
	}
	for _, active := range sessions {
		if storage.NormalizeMAC(active) == mac {
			return false, fmt.Errorf("请先关闭客户机 %s 再重试：身份层仍在使用 %s", mac, origin)
		}
	}
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return false, err
	}
	for _, other := range datasets {
		carrier := storage.Classify(datasetName(other))
		if strings.HasPrefix(other, dataset+"/") || other != dataset && storage.NormalizeMAC(carrier.MAC) == mac {
			return false, fmt.Errorf("请先关闭客户机 %s 并结束相关任务 %s 再重试：身份层仍在使用 %s", mac, other, origin)
		}
	}
	dependents, err := a.zfs.ListDependentClones(ctx, dataset)
	if err != nil {
		return false, err
	}
	if len(dependents) > 0 {
		return false, fmt.Errorf("请先核对客户机 %s 身份层的派生对象 %s，再重试清理来源 %s", mac, strings.Join(dependents, "、"), origin)
	}
	// Do not tear down targets or dependent clones: they may belong to a different lifecycle.
	if err := a.zfs.Destroy(ctx, dataset); err != nil {
		return false, err
	}
	return true, nil
}

func pinnedBy(origin string, gone []string) bool {
	for _, g := range gone {
		if origin == g || strings.HasPrefix(origin, g+"@") || strings.HasPrefix(origin, g+"/") {
			return true
		}
	}
	return false
}

var identitySourcePart = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// identitySourceRoot 判断完整路径是否落在目录或挪开的副本里；分叉副本的根不算，它不是单个卷。
func identitySourceRoot(parts []string) bool {
	if parts[1] == storage.CatalogueRoot {
		return len(parts) > 2
	}
	kind, ok := storage.AsideKind(parts[1])
	return ok && (kind != storage.PreservedDiverged || len(parts) > 2)
}

// Sources may be relative catalogue entities or full local paths; never accept a pool/run root or traversal.
func (a *Agent) identitySources(sources []string) ([]string, error) {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		dataset, snap, hasSnap := strings.Cut(source, "@")
		if hasSnap && (!identitySourcePart.MatchString(snap) || snap == "." || snap == "..") {
			return nil, fmt.Errorf("身份层清理来源无效：%s", source)
		}
		parts := strings.Split(dataset, "/")
		for _, part := range parts {
			if !identitySourcePart.MatchString(part) || part == "." || part == ".." {
				return nil, fmt.Errorf("身份层清理来源无效：%s", source)
			}
		}
		if len(parts) == 1 {
			if dataset == a.zfs.PoolName() || storage.Classify(dataset).Kind != storage.CarrierEntity || storage.ReservedCatalogueChild(dataset) {
				return nil, fmt.Errorf("身份层清理来源不是配置或还原点：%s", source)
			}
			dataset = a.zfs.Dataset(dataset)
		} else if parts[0] != a.zfs.PoolName() || !identitySourceRoot(parts) {
			return nil, fmt.Errorf("身份层清理来源不在本节点目录或指定旧副本中：%s", source)
		}
		if hasSnap {
			dataset += "@" + snap
		}
		out = append(out, dataset)
	}
	return out, nil
}
