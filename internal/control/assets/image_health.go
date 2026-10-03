package assets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// ErrImageHealthBlocked 是 C-12 门禁：最新体检为阻断级的镜像不能绑定到分组。
var ErrImageHealthBlocked = errs.Invalid("镜像体检结果为阻断级")

type HealthCheckResult struct {
	TaskID string `json:"task_id"`
}

// dropHealthReport 删除数据盘的体检报告：数据盘上没有系统可检，残留报告会持续告警。
func (s ImageService) dropHealthReport(ctx context.Context, imageID string) {
	if old, err := s.Store.ImageHealthReports().GetByImage(ctx, imageID); err == nil {
		_ = s.Store.ImageHealthReports().Delete(ctx, old.ID)
	}
}

// RunHealthCheck 为镜像发起 image_health_check 任务。
func (s ImageService) RunHealthCheck(ctx context.Context, imageID string) (HealthCheckResult, error) {
	img, err := s.Store.Images().Get(ctx, imageID)
	if err != nil {
		return HealthCheckResult{}, err
	}
	if img.Purpose == domain.ImagePurposeData {
		s.dropHealthReport(ctx, img.ID)
		return HealthCheckResult{}, errs.Invalid(fmt.Sprintf("「%s」是数据盘，里面没有系统，不需要体检", img.Name))
	}
	task, err := s.runner().Run(ctx, domain.TaskTypeImageHealthCheck, img.ID, func(ctx context.Context, task domain.Task) error {
		return s.executeHealthCheck(ctx, task, img)
	})
	if err != nil {
		return HealthCheckResult{}, err
	}
	return HealthCheckResult{TaskID: task.ID}, nil
}

func (s ImageService) executeHealthCheck(ctx context.Context, task domain.Task, img domain.Image) error {
	configID, snapshotName, err := s.healthCheckSource(ctx, img.ID)
	if err != nil {
		return err
	}
	report, err := s.Storage.InspectImage(ctx, storage.InspectImageReq{
		ImageID:      img.ID,
		ConfigID:     configID,
		SnapshotName: snapshotName,
		OSType:       img.OSType,
	})
	if err != nil {
		return err
	}
	items := make([]domain.HealthCheckItem, 0, len(report.Items))
	for _, item := range report.Items {
		items = append(items, domain.HealthCheckItem{Name: item.Name, Level: item.Level, Detail: item.Detail})
	}
	record := domain.ImageHealthReport{
		ID:             "health-" + img.ID,
		ImageID:        img.ID,
		Level:          report.Level,
		PartitionStyle: report.PartitionStyle,
		BootModes:      report.BootModes,
		Items:          items,
		NICPCIIDs:      report.NICPCIIDs,
		CreatedAt:      s.now(),
	}
	// 每个镜像只保留最新一份报告，重跑覆盖旧报告。
	if _, err := s.Store.ImageHealthReports().GetByImage(ctx, img.ID); err == nil {
		if err := s.Store.ImageHealthReports().Update(ctx, record); err != nil {
			return err
		}
	} else if errs.IsNotFound(err) {
		if err := s.Store.ImageHealthReports().Create(ctx, record); err != nil {
			return err
		}
	} else {
		return err
	}
	task.Status = domain.TaskStatusSuccess
	task.Progress = 100
	task.Result = string(report.Level)
	finished := s.now()
	task.FinishedAt = &finished
	return s.Store.Tasks().Update(ctx, task)
}

// healthCheckSource 按镜像 → 默认配置 → 最新就绪还原点解析，返回 (configID, zfs 快照名)。
func (s ImageService) healthCheckSource(ctx context.Context, imageID string) (string, string, error) {
	configs, err := s.Store.Configs().ListByImage(ctx, imageID)
	if err != nil {
		return "", "", err
	}
	if len(configs) == 0 {
		return "", "", fmt.Errorf("image %s has no config to inspect", imageID)
	}
	cfg := configs[0]
	for _, c := range configs {
		if c.Name == "default" {
			cfg = c
			break
		}
	}
	reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
	if err != nil {
		return "", "", err
	}
	var latest *domain.Reduction
	var latestAt time.Time
	for i, r := range reductions {
		if r.Status != domain.ReductionStatusReady {
			continue
		}
		if latest == nil || r.CreatedAt.After(latestAt) {
			latest = &reductions[i]
			latestAt = r.CreatedAt
		}
	}
	if latest == nil {
		return "", "", fmt.Errorf("config %s has no ready reduction to inspect", cfg.ID)
	}
	return cfg.ID, strings.TrimPrefix(latest.Name, "@"), nil
}

func (s ImageService) GetHealth(ctx context.Context, imageID string) (domain.ImageHealthReport, error) {
	if _, err := s.Store.Images().Get(ctx, imageID); err != nil {
		return domain.ImageHealthReport{}, err
	}
	return s.Store.ImageHealthReports().GetByImage(ctx, imageID)
}
