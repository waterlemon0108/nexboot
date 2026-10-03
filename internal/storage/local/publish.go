package local

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
)

// PublishDataDisk 在超管机不关机的情况下把一块数据盘存为配置的还原点：数据盘是独立 LUN，
// 可以摘下再装回而会话和 LUN 0 系统盘不受影响；系统盘 Windows 不放手，不能这么做。
// 摘盘期间只做元数据操作；这里不让 Windows 静默，盘是否已安静由调用方判断。
func (a *Agent) PublishDataDisk(ctx context.Context, req storage.PublishDataDiskReq) (storage.PublishDataDiskResult, error) {
	mac := storage.NormalizeMAC(req.MAC)
	if mac == "" {
		return storage.PublishDataDiskResult{}, fmt.Errorf("mac is required")
	}
	if req.LUN < 1 {
		return storage.PublishDataDiskResult{}, fmt.Errorf("仅数据盘支持在线发布，系统盘需关机后保存")
	}
	if strings.TrimSpace(req.ConfigID) == "" || strings.TrimSpace(req.ReductionName) == "" {
		return storage.PublishDataDiskResult{}, fmt.Errorf("config and reduction are required")
	}
	defer a.lockClient(mac)()

	cloneName := storage.SuperClientDataCloneName(mac, req.LUN)
	dataset := a.zfs.Dataset(cloneName)
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return storage.PublishDataDiskResult{}, err
	}
	// 必须在摘盘前检查，否则会白白摘掉一块正常工作的盘。
	if !top.has(dataset) {
		return storage.PublishDataDiskResult{}, fmt.Errorf("%s does not exist: %w", dataset, storage.SuperSessionMissing{LUN: req.LUN})
	}

	target := iscsi.TargetForMAC(mac)
	if a.exporter != nil {
		// backstore 会占住 zvol，挡住下面的重命名。
		if err := a.exporter.DeleteLUNs(ctx, target, []int{req.LUN}); err != nil {
			return storage.PublishDataDiskResult{}, err
		}
	}

	reduction, saveErr := a.saveSuperClone(ctx, top, req.LUN, cloneName, req.ConfigID, req.ReductionName)
	if saveErr != nil {
		// 失败时先把盘还给机器再报原因。只重新导出旧路径不够：克隆可能已被重命名到配置上，原路径为空。
		if err := a.recloneDataDisk(ctx, req, dataset); err != nil {
			return storage.PublishDataDiskResult{}, errors.Join(saveErr, err)
		}
		if err := a.exportDataDisk(ctx, mac, req, dataset); err != nil {
			return storage.PublishDataDiskResult{}, errors.Join(saveErr, err)
		}
		return storage.PublishDataDiskResult{}, saveErr
	}

	// 机器改用刚发布内容的新克隆，下次发布只带增量；数据集同名，LUN 仍指向原路径。
	if err := top.ensureCloned(ctx, a.zfs.Snapshot(req.ConfigID, strings.TrimPrefix(reduction.Name, "@")), dataset); err != nil {
		return storage.PublishDataDiskResult{Reduction: reduction}, storage.DiskNotReattached{LUN: req.LUN, Err: err}
	}
	lun := storage.LUN{LUN: req.LUN, MountTarget: req.MountTarget, VolPath: a.zfs.VolumePath(dataset)}
	if a.exporter != nil {
		if err := a.exportDataDisk(ctx, mac, req, dataset); err != nil {
			return storage.PublishDataDiskResult{Reduction: reduction}, storage.DiskNotReattached{LUN: req.LUN, Err: err}
		}
		lun.Target = target
	}
	return storage.PublishDataDiskResult{Reduction: reduction, LUN: lun}, nil
}

// recloneDataDisk 在发布失败后按配置当前状态重建数据盘克隆；克隆还在则什么都不做。
func (a *Agent) recloneDataDisk(ctx context.Context, req storage.PublishDataDiskReq, dataset string) error {
	top, err := readTopology(ctx, a.zfs)
	if err != nil {
		return err
	}
	if top.has(dataset) {
		return nil
	}
	configDataset := a.zfs.Dataset(req.ConfigID)
	snaps, err := a.zfs.SnapshotsOf(ctx, configDataset)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		return fmt.Errorf("%s 无可用还原点，无法重新挂载数据盘", req.ConfigID)
	}
	return top.ensureCloned(ctx, configDataset+"@"+snaps[len(snaps)-1], dataset)
}

// exportDataDisk 把数据盘按原 LUN 号装回机器已有的 target。
func (a *Agent) exportDataDisk(ctx context.Context, mac string, req storage.PublishDataDiskReq, dataset string) error {
	if a.exporter == nil {
		return nil
	}
	export := iscsi.ExportRequest{MAC: mac, VolPath: a.zfs.VolumePath(dataset), LUN: req.LUN}
	if iscsi.CHAPEnabled(a.chapSecret) {
		creds := iscsi.CHAPCredentials(a.chapSecret, mac)
		export.CHAP = &creds
	}
	_, err := a.exporter.ExportBlocks(ctx, []iscsi.ExportRequest{export})
	return err
}

// SuperDiskWritten 返回该盘自所基于的还原点以来写入的字节数，即发布会保存的量；
// 采样两次即可判断盘是否已安静，无需询问客户机。
func (a *Agent) SuperDiskWritten(ctx context.Context, mac string, lun int) (int64, error) {
	name := storage.SuperClientCloneName(storage.NormalizeMAC(mac))
	if lun > 0 {
		name = storage.SuperClientDataCloneName(storage.NormalizeMAC(mac), lun)
	}
	return a.zfs.Written(ctx, a.zfs.Dataset(name))
}

// PreservedCopies 报告本节点留存的目录副本，供写入者展示到告警页；不上报的话数据虽在，操作者却无从知晓。
func (a *Agent) PreservedCopies(ctx context.Context) ([]storage.PreservedCopy, error) {
	// 目录根是任一目录数据集的父级（<池>/nd）
	root := path.Dir(a.zfs.Dataset("probe"))
	names, err := a.zfs.ListAsideCopies(ctx, root)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	snaps, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]storage.PreservedCopy, 0, len(names))
	for _, name := range names {
		copy := storage.PreservedCopy{Dataset: name, Kind: storage.PreservedRebuilding}
		if idx := strings.LastIndex(name, "-diverged-"); idx >= 0 {
			copy.Kind = storage.PreservedDiverged
		}
		if stamp := name[strings.LastIndex(name, "-")+1:]; stamp != "" {
			if ns, cerr := strconv.ParseInt(stamp, 10, 64); cerr == nil {
				copy.Created = time.Unix(0, ns).UTC()
			}
		}
		if used, uerr := a.zfs.Used(ctx, name); uerr == nil {
			copy.Used = used
		}
		for _, full := range snaps {
			ds, snap, ok := strings.Cut(full, "@")
			if !ok || (ds != name && !strings.HasPrefix(ds, name+"/")) {
				continue
			}
			if !storage.SystemSnapshot(snap) {
				copy.Points++
			}
		}
		out = append(out, copy)
	}
	return out, nil
}
