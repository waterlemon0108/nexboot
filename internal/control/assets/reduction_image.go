package assets

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// SaveAsImageRequest 是还原点另存为镜像时的新镜像名。
type SaveAsImageRequest struct {
	Name string `json:"name"`
}

// restorePointSource 解析导出或复制还原点所需的信息，拒绝镜像导出同样拒绝的状态。
func (s ImageService) restorePointSource(ctx context.Context, reductionID string) (domain.Reduction, domain.Image, error) {
	red, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return domain.Reduction{}, domain.Image{}, err
	}
	if red.Status != domain.ReductionStatusReady {
		return domain.Reduction{}, domain.Image{}, errs.Conflict(fmt.Sprintf("还原点「%s」还没就绪，请等它完成后再操作", displayNameOf(red)))
	}
	cfg, err := s.Store.Configs().Get(ctx, red.ConfigID)
	if err != nil {
		return domain.Reduction{}, domain.Image{}, err
	}
	img, err := s.exportableImage(ctx, cfg.ImageID)
	return red, img, err
}

// ReductionExportName 返回还原点下载的附件名。
func (s ImageService) ReductionExportName(ctx context.Context, reductionID string, compress bool) (string, error) {
	red, img, err := s.restorePointSource(ctx, reductionID)
	if err != nil {
		return "", err
	}
	name := sanitizeExportName(img.Name + "-" + displayNameOf(red))
	if compress {
		return name + ".zfs.gz", nil
	}
	return name + ".zfs", nil
}

// StreamReduction 把还原点写成可重新导入的镜像流。
func (s ImageService) StreamReduction(ctx context.Context, reductionID string, w io.Writer, compress bool) error {
	red, img, err := s.restorePointSource(ctx, reductionID)
	if err != nil {
		return err
	}
	defer beginExport(img.ID)()
	return s.streamExport(ctx, storage.ExportImageReq{ConfigID: red.ConfigID, Snapshot: strings.TrimPrefix(red.Name, "@")}, w, compress)
}

// ExportReductionToDir 把还原点导出到本节点的导入目录，见 exportToDir。
func (s ImageService) ExportReductionToDir(ctx context.Context, reductionID string) (ExportImageResult, error) {
	red, img, err := s.restorePointSource(ctx, reductionID)
	if err != nil {
		return ExportImageResult{}, err
	}
	return s.exportToDir(ctx, img, red.ConfigID, img.Name+"-"+displayNameOf(red),
		storage.ExportImageReq{ConfigID: red.ConfigID, Snapshot: strings.TrimPrefix(red.Name, "@")})
}

// SaveReductionAsImage 把还原点完整复制成独立镜像，之后两者可各自删除。
func (s ImageService) SaveReductionAsImage(ctx context.Context, reductionID string, req SaveAsImageRequest) (ImportImageResult, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ImportImageResult{}, errs.Invalid("请填写新镜像的名称")
	}
	red, src, err := s.restorePointSource(ctx, reductionID)
	if err != nil {
		return ImportImageResult{}, err
	}
	claim, err := claimImageName(name, "另存为")
	if err != nil {
		return ImportImageResult{}, err
	}
	defer claim.Drop()
	if _, err := s.Store.Images().GetByName(ctx, name); err == nil {
		return ImportImageResult{}, ErrImageExists
	} else if !errs.IsNotFound(err) {
		return ImportImageResult{}, err
	}
	if err := s.ensureCopyFits(ctx, red, name); err != nil {
		return ImportImageResult{}, err
	}
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeCopyImage, src.ID, func(ctx context.Context, task domain.Task) error {
		return s.executeSaveAsImage(ctx, task, red, src, name)
	})
	if err != nil {
		return ImportImageResult{}, err
	}
	return ImportImageResult{TaskID: task.ID}, nil
}

func (s ImageService) executeSaveAsImage(ctx context.Context, task domain.Task, red domain.Reduction, src domain.Image, name string) error {
	copied, err := s.Storage.CopyReductionToImage(ctx, storage.CopyReductionReq{
		ConfigID: red.ConfigID, Snapshot: strings.TrimPrefix(red.Name, "@"), Name: name, OSType: src.OSType, Purpose: src.Purpose,
		OnTarget: func(imageID string) { recordTarget(ctx, s.Store, &task, imageID) },
		OnProgress: func(percent int, message string) {
			t := task
			t.Status, t.Progress, t.Message = domain.TaskStatusRunning, percent, message
			_ = s.Store.Tasks().Update(ctx, t)
		},
	})
	if err != nil {
		return err
	}
	// 内容（含启动脚本）来自源镜像，脚本版本随之继承。
	copied.Image.MountScriptVersion = src.MountScriptVersion
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		for _, err := range []error{tx.Images().Create(ctx, copied.Image), tx.Configs().Create(ctx, copied.Config), tx.Reductions().Create(ctx, copied.Reduction)} {
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if rbErr := s.Storage.DeleteImage(context.Background(), copied.Image.ID, []string{copied.Config.ID}); rbErr != nil {
			slog.Warn("copied image not rolled back", "image", copied.Image.ID, "error", rbErr)
		}
		return err
	}
	if copied.Image.Purpose != domain.ImagePurposeData {
		_, _ = s.RunHealthCheck(ctx, copied.Image.ID)
	}
	return s.runner().Finish(ctx, s.Store, task, copied.Image.ID)
}

// ensureCopyFits 拒绝池明显放不下的复制；估算失败时不拦，由复制本身报真实错误。
func (s ImageService) ensureCopyFits(ctx context.Context, red domain.Reduction, name string) error {
	need, err := s.Storage.ExportImageSize(ctx, red.ConfigID)
	if err != nil || need <= 0 {
		return nil
	}
	usage, err := s.Storage.SpaceUsage(ctx)
	if err != nil || need <= usage.PoolAvailable {
		return nil
	}
	return errs.Conflict(fmt.Sprintf("请先给存储池腾出空间再另存为「%s」：可用 %s，需要约 %s。删除不用的镜像或还原点，或给存储池加盘",
		name, fmtBytes(usage.PoolAvailable), fmtBytes(need)))
}
