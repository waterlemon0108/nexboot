package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// withExportScratch 把还原点克隆成临时数据集并打导出快照，发出的流只带接收端会删掉的快照，
// 形态与镜像导出一致。fn 返回后两者都会删除。
func (a *Agent) withExportScratch(ctx context.Context, configID, snapshot string, fn func(streamSnapshot string) error) error {
	if configID == "" || snapshot == "" {
		return fmt.Errorf("config and restore point are required")
	}
	scratch := a.zfs.Dataset(storage.ExportCloneName(configID))
	if err := a.zfs.Clone(ctx, a.zfs.Snapshot(configID, snapshot), scratch); err != nil {
		return err
	}
	defer a.destroyDetached(scratch)
	mark := fmt.Sprintf("%s%d", storage.ExportSnapshotPrefix, time.Now().UTC().UnixNano())
	if err := a.zfs.SnapshotVolume(ctx, scratch, mark); err != nil {
		return err
	}
	defer func() {
		cctx, cancel := cleanupContext()
		defer cancel()
		_ = a.zfs.DestroySnapshot(cctx, scratch, mark)
	}()
	return fn(scratch + "@" + mark)
}

func (a *Agent) exportRestorePoint(ctx context.Context, req storage.ExportImageReq) error {
	return a.withExportScratch(ctx, req.ConfigID, req.Snapshot, func(snap string) error {
		return a.sendWithProgress(ctx, snap, req.W, req.OnProgress)
	})
}

func (a *Agent) sendWithProgress(ctx context.Context, snapshot string, w io.Writer, onProgress func(written, estimated int64)) error {
	if onProgress != nil {
		var estimated int64
		if size, err := a.zfs.SendSize(ctx, snapshot); err != nil {
			slog.Warn("send size estimate unavailable", "snapshot", snapshot, "error", err)
		} else {
			estimated = size
		}
		w = &exportProgressWriter{w: w, estimated: estimated, onProgress: onProgress}
	}
	return a.zfs.SendSnapshot(ctx, snapshot, w)
}

// CopyReductionToImage 把还原点整份复制为新镜像。不用克隆：克隆虽快，但只要副本在，源就删不掉，而「另存为」要求独立。
func (a *Agent) CopyReductionToImage(ctx context.Context, req storage.CopyReductionReq) (storage.ImportImageResult, error) {
	if strings.TrimSpace(req.Name) == "" || req.OSType == "" {
		return storage.ImportImageResult{}, fmt.Errorf("name and os type are required")
	}
	datasets, err := a.existingDatasets(ctx)
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	imageID := storage.ImageName(req.Name, func(id string) bool { return datasets[a.zfs.Dataset(id)] })
	imageDataset := a.zfs.Dataset(imageID)
	configID := storage.DefaultConfigName(imageID)
	if datasets[imageDataset] || datasets[a.zfs.Dataset(configID)] {
		return storage.ImportImageResult{}, fmt.Errorf("target dataset already exists")
	}
	if req.OnTarget != nil {
		req.OnTarget(imageID)
	}
	report := func(percent int, message string) {
		if req.OnProgress != nil {
			req.OnProgress(percent, message)
		}
	}
	report(pgPrepare, "准备复制")
	// 只有本次收下（或收到一半）的数据集才归本次回滚；目标被别人占着时不能删。
	owned := false
	err = a.withExportScratch(ctx, req.ConfigID, req.Snapshot, func(snap string) error {
		pr, pw := io.Pipe()
		sent := make(chan error, 1)
		go func() {
			err := a.sendWithProgress(ctx, snap, pw, func(written, estimated int64) {
				if estimated > 0 {
					report(pgPrepare+int(float64(pgSnapshot-pgPrepare)*min(1, float64(written)/float64(estimated))), "复制数据")
				}
			})
			pw.CloseWithError(err)
			sent <- err
		}()
		recvErr := a.zfs.ReceiveStream(ctx, imageDataset, pr)
		owned = recvErr == nil || !targetTaken(recvErr)
		pr.CloseWithError(recvErr)
		if err := errors.Join(<-sent, recvErr); err != nil {
			return err
		}
		// 收到的副本带着临时克隆的导出快照。
		_, mark, _ := strings.Cut(snap, "@")
		return a.zfs.DestroySnapshot(ctx, imageDataset, mark)
	})
	if err != nil {
		if owned {
			a.destroyDetached(imageDataset)
		}
		return storage.ImportImageResult{}, err
	}
	size, err := a.zfs.VolumeSize(ctx, imageDataset)
	if err != nil {
		a.destroyDetached(imageDataset)
		return storage.ImportImageResult{}, err
	}
	return a.finishImage(ctx, imageID, configID, report, domainImage(req, size), false)
}

func domainImage(req storage.CopyReductionReq, size int64) domain.Image {
	purpose := req.Purpose
	if purpose == "" {
		purpose = domain.ImagePurposeSystem
	}
	return domain.Image{Name: strings.TrimSpace(req.Name), OSType: req.OSType, Size: size, Purpose: purpose, Origin: domain.ImageOriginImported}
}
