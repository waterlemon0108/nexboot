package assets

import (
	"context"
	"errors"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// PublishDataDiskRequest 指定超管机的一块数据盘及发布后还原点的名称。
type PublishDataDiskRequest struct {
	DiskID string `json:"disk_id"`
	Name   string `json:"name"`
}

// PublishDataDiskResult 携带新还原点。发布成功但后续步骤失败时填 Detail，
// 已成功的发布不能报成失败。
type PublishDataDiskResult struct {
	TaskID    string            `json:"task_id,omitempty"`
	Reduction *domain.Reduction `json:"reduction,omitempty"`
	Detail    string            `json:"detail,omitempty"`
}

// quietWindow 是数据盘发布前须保持无写入的时长。
const quietWindow = 3 * time.Second

func (s TerminalService) quietWindow() time.Duration {
	if s.QuietWindow > 0 {
		return s.QuietWindow
	}
	return quietWindow
}

// PublishDataDisk 在超管机不关机的情况下，把一块数据盘存为其配置的还原点并设为当前，
// 客户机下次开机生效。系统盘没有这条路径，只能关机后保存。
func (s TerminalService) PublishDataDisk(ctx context.Context, id string, req PublishDataDiskRequest) (PublishDataDiskResult, error) {
	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	if !terminal.IsSuper {
		return PublishDataDiskResult{}, ErrTerminalSuperRequired
	}
	group, err := s.Store.Groups().Get(ctx, terminal.GroupID)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	disks, err := GroupDataDisks(ctx, s.Store, group.ID)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	disk, lun, ok := findDataDisk(disks, req.DiskID)
	if !ok {
		return PublishDataDiskResult{}, errs.Invalid("分组 " + group.Name + " 不存在该数据盘，请刷新后重试")
	}
	cfg, err := s.Store.Configs().Get(ctx, disk.ConfigID)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	claim, err := claimImages(ctx, s.Store, domain.TaskTypePublishDataDisk, cfg.ImageID)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	defer claim.Drop()
	reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	displayName, name, err := validateReductionName(req.Name, reductions)
	if err != nil {
		return PublishDataDiskResult{}, err
	}

	var result PublishDataDiskResult
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypePublishDataDisk, terminal.MAC, func(ctx context.Context, task domain.Task) error {
		var err error
		result, err = s.executePublish(ctx, task, terminal, disk, cfg, lun, name, displayName)
		return err
	})
	if err != nil {
		return PublishDataDiskResult{}, err
	}
	if s.Async {
		return PublishDataDiskResult{TaskID: task.ID}, nil
	}
	result.TaskID = task.ID
	return result, nil
}

func (s TerminalService) executePublish(ctx context.Context, task domain.Task, terminal domain.Terminal, disk domain.GroupDisk, cfg domain.Config, lun int, name, displayName string) (PublishDataDiskResult, error) {
	// 池与数据库在此一起变更，中间不能插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	if err := s.requireQuietDisk(ctx, terminal, lun, disk.MountTarget); err != nil {
		return PublishDataDiskResult{}, err
	}
	out, err := s.Storage.PublishDataDisk(ctx, storage.PublishDataDiskReq{
		MAC: terminal.MAC, LUN: lun, ConfigID: cfg.ID, ReductionName: name, MountTarget: disk.MountTarget})
	detail := ""
	if err != nil {
		// 已发布但盘没装回：报失败会让操作者重复发布，实际只需重启客户机。
		var notBack storage.DiskNotReattached
		if !errors.As(err, &notBack) {
			return PublishDataDiskResult{}, err
		}
		detail = "已发布；" + disk.MountTarget + " 未重新挂载，重启客户机后恢复"
	}
	reduction := out.Reduction
	reduction.ConfigID = cfg.ID
	reduction.Name = name
	reduction.DisplayName = displayName
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Reductions().Create(ctx, reduction); err != nil {
			return err
		}
		// 发布即下发给客户机：与「设为当前」一样把该点设为当前。
		if err := setConfigCurrent(ctx, tx, cfg.ID, reduction.ID); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, reduction.ID)
	}); err != nil {
		return PublishDataDiskResult{}, err
	}
	if s.BackupTrigger != nil {
		go func() {
			if err := s.BackupTrigger(context.Background()); err != nil && s.Logger != nil {
				s.Logger.Error("backup trigger failed", "error", err)
			}
		}()
	}
	return PublishDataDiskResult{Reduction: &reduction, Detail: detail}, nil
}

// requireQuietDisk 拒绝发布仍在写入的盘：快照按原样截取，安装到一半发布会让所有
// 客户机拿到半成品。通过两次采样写入字节数判断，不依赖客户机内部。
func (s TerminalService) requireQuietDisk(ctx context.Context, terminal domain.Terminal, lun int, mount string) error {
	if s.Storage == nil || terminal.State != domain.TerminalStateOnline {
		return nil // 关机的机器不可能在写
	}
	first, err := s.Storage.SuperDiskWritten(ctx, terminal.MAC, lun)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.quietWindow()):
	}
	second, err := s.Storage.SuperDiskWritten(ctx, terminal.MAC, lun)
	if err != nil {
		return err
	}
	if second != first {
		return errs.Conflict(mount + " 正在写入，请等待写入完成后再发布")
	}
	return nil
}

func findDataDisk(disks []domain.GroupDisk, id string) (domain.GroupDisk, int, bool) {
	for i, disk := range disks {
		if disk.ID == id {
			return disk, DataDiskLUN(i), true
		}
	}
	return domain.GroupDisk{}, 0, false
}

// SuperDiskItem 是控制台展示的一块超管机数据盘：挂载点、当前发布的还原点及之后的变更量。
type SuperDiskItem struct {
	DiskID      string `json:"disk_id"`
	LUN         int    `json:"lun"`
	MountTarget string `json:"mount_target"`
	ConfigID    string `json:"config_id"`
	ConfigName  string `json:"config_name"`
	CurrentName string `json:"current_name"`
	Written     int64  `json:"written"`
	// Ready 为 false 表示该盘分给分组后，这台机器还没以超管机开过机，尚无可发布的盘。
	Ready bool `json:"ready"`
}

type SuperDiskListResult struct {
	Items []SuperDiskItem `json:"items"`
	Total int             `json:"total"`
}

// SuperDisks 列出超管机的数据盘。系统盘不在其中，它靠关机保存而非发布。
func (s TerminalService) SuperDisks(ctx context.Context, id string) (SuperDiskListResult, error) {
	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return SuperDiskListResult{}, err
	}
	if !terminal.IsSuper {
		return SuperDiskListResult{}, ErrTerminalSuperRequired
	}
	disks, err := GroupDataDisks(ctx, s.Store, terminal.GroupID)
	if err != nil {
		return SuperDiskListResult{}, err
	}
	items := make([]SuperDiskItem, 0, len(disks))
	for i, disk := range disks {
		item := SuperDiskItem{DiskID: disk.ID, LUN: DataDiskLUN(i), MountTarget: disk.MountTarget, ConfigID: disk.ConfigID}
		if cfg, err := s.Store.Configs().Get(ctx, disk.ConfigID); err == nil {
			item.ConfigName = cfg.Name
			if cfg.DefaultReductionID != nil && *cfg.DefaultReductionID != "" {
				if red, err := s.Store.Reductions().Get(ctx, *cfg.DefaultReductionID); err == nil {
					item.CurrentName = displayNameOf(red)
				}
			}
		}
		if s.Storage != nil {
			// 出错说明池里还没有这块盘（未以超管机开过机），Ready 保持 false。
			if written, err := s.Storage.SuperDiskWritten(ctx, terminal.MAC, item.LUN); err == nil {
				item.Written, item.Ready = written, true
			}
		}
		items = append(items, item)
	}
	return SuperDiskListResult{Items: items, Total: len(items)}, nil
}
