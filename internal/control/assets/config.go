package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrConfigExists          = errs.Conflict("配置已存在")
	ErrConfigInUse           = errs.Conflict("配置正在使用中")
	ErrConfigNameRequired    = errs.Invalid("配置名称必填")
	ErrConfigReductionNeeded = errs.Invalid("必须选择还原点")
	ErrReductionMismatch     = errs.Invalid("还原点不属于源配置")
	ErrConfigHasDependents   = errs.Conflict("该配置正被其它对象占用")
)

type ConfigService struct {
	Store   store.Store
	Storage storage.StorageAgent
	Now     func() time.Time
	Async   bool
	// Replicating 返回正在接收这些数据集（或整个目录）的备机；nil 表示没有。
	// 发送会占住数据集数分钟，期间删除必然失败。
	Replicating func(ids ...string) []string
}

// catalogueBusy 在备机正接收本操作要销毁的数据集时拒绝；action 是操作者发起的动作名（如「删除配置」）。
func (s ConfigService) catalogueBusy(action string, ids ...string) error {
	if s.Replicating == nil {
		return nil
	}
	if standbys := s.Replicating(ids...); len(standbys) > 0 {
		return errs.Conflict(fmt.Sprintf("请待备机 %s 同步完成后再%s：它正在同步这些数据，同步期间删不掉", strings.Join(standbys, "、"), action))
	}
	return nil
}

// imageDatasets 返回镜像及其全部配置，即合并或覆盖会销毁、改写的数据集。
func (s ConfigService) imageDatasets(ctx context.Context, imageID string) []string {
	ids := []string{imageID}
	if configs, err := s.Store.Configs().ListByImage(ctx, imageID); err == nil {
		for _, cfg := range configs {
			ids = append(ids, cfg.ID)
		}
	}
	return ids
}

type ConfigListResult struct {
	Items []domain.Config `json:"items"`
	Total int             `json:"total"`
}

type CreateConfigRequest struct {
	Name string `json:"name"`
}

type CreateConfigFromConfigRequest struct {
	Name        string `json:"name"`
	ReductionID string `json:"reduction_id"`
}

type ConfigOperationResult struct {
	TaskID string        `json:"task_id"`
	Config domain.Config `json:"config"`
}

type ConfigTaskResult struct {
	TaskID string `json:"task_id"`
}

func (s ConfigService) CreateFromImage(ctx context.Context, imageID string, req CreateConfigRequest) (ConfigOperationResult, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return ConfigOperationResult{}, ErrConfigNameRequired
	}
	if _, err := s.Store.Images().Get(ctx, imageID); err != nil {
		return ConfigOperationResult{}, err
	}
	claim, err := claimImages(ctx, s.Store, domain.TaskTypeCreateConfig, imageID)
	if err != nil {
		return ConfigOperationResult{}, err
	}
	defer claim.Drop()
	if err := s.ensureConfigNameUnique(ctx, imageID, req.Name); err != nil {
		return ConfigOperationResult{}, err
	}
	var cfg domain.Config
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeCreateConfig, imageID, func(ctx context.Context, task domain.Task) error {
		var err error
		cfg, err = s.executeCreateFromImage(ctx, task, imageID, req.Name)
		return err
	})
	if err != nil {
		return ConfigOperationResult{}, err
	}
	if s.Async {
		return ConfigOperationResult{TaskID: task.ID}, nil
	}
	return ConfigOperationResult{TaskID: task.ID, Config: cfg}, nil
}

func (s ConfigService) executeCreateFromImage(ctx context.Context, task domain.Task, imageID string, name string) (domain.Config, error) {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	created, err := s.Storage.CreateConfig(ctx, imageID, name, nil)
	if err != nil {
		return domain.Config{}, err
	}
	cfg := created.Config
	cfg.ImageID = imageID
	if cfg.Name == "" {
		cfg.Name = name
	}
	return s.storeNewConfig(ctx, task, cfg, created.Reduction)
}

// storeNewConfig 在一个事务里写入配置及其基线还原点：缺还原点行的配置无法绑定，两者不能只落一半。
// 失败时销毁数据集，快照随之删除。
func (s ConfigService) storeNewConfig(ctx context.Context, task domain.Task, cfg domain.Config, reduction domain.Reduction) (domain.Config, error) {
	reduction.ConfigID = cfg.ID
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Configs().Create(ctx, cfg); err != nil {
			return err
		}
		if err := tx.Reductions().Create(ctx, reduction); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, cfg.ID)
	}); err != nil {
		_ = s.Storage.DeleteConfig(ctx, cfg.ID)
		return domain.Config{}, err
	}
	return cfg, nil
}

func (s ConfigService) CreateFromConfig(ctx context.Context, sourceConfigID string, req CreateConfigFromConfigRequest) (ConfigOperationResult, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.ReductionID = strings.TrimSpace(req.ReductionID)
	if req.Name == "" {
		return ConfigOperationResult{}, ErrConfigNameRequired
	}
	if req.ReductionID == "" {
		return ConfigOperationResult{}, ErrConfigReductionNeeded
	}

	sourceConfig, err := s.Store.Configs().Get(ctx, sourceConfigID)
	if err != nil {
		return ConfigOperationResult{}, err
	}
	claim, err := claimImages(ctx, s.Store, domain.TaskTypeCreateConfig, sourceConfig.ImageID)
	if err != nil {
		return ConfigOperationResult{}, err
	}
	defer claim.Drop()
	if err := s.ensureConfigNameUnique(ctx, sourceConfig.ImageID, req.Name); err != nil {
		return ConfigOperationResult{}, err
	}

	reduction, err := s.Store.Reductions().Get(ctx, req.ReductionID)
	if err != nil {
		return ConfigOperationResult{}, err
	}
	if reduction.ConfigID != sourceConfigID {
		return ConfigOperationResult{}, ErrReductionMismatch
	}
	snapshotName := strings.TrimPrefix(reduction.Name, "@")
	var cfg domain.Config
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeCreateConfig, sourceConfigID, func(ctx context.Context, task domain.Task) error {
		var err error
		cfg, err = s.executeCreateFromConfig(ctx, task, sourceConfig, req.Name, snapshotName)
		return err
	})
	if err != nil {
		return ConfigOperationResult{}, err
	}
	if s.Async {
		return ConfigOperationResult{TaskID: task.ID}, nil
	}
	return ConfigOperationResult{TaskID: task.ID, Config: cfg}, nil
}

func (s ConfigService) executeCreateFromConfig(ctx context.Context, task domain.Task, sourceConfig domain.Config, name string, snapshotName string) (domain.Config, error) {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	created, err := s.Storage.CreateConfig(ctx, sourceConfig.ID, name, &snapshotName)
	if err != nil {
		return domain.Config{}, err
	}
	cfg := created.Config
	cfg.ImageID = sourceConfig.ImageID
	cfg.Name = name
	return s.storeNewConfig(ctx, task, cfg, created.Reduction)
}

func (s ConfigService) List(ctx context.Context, imageID string) (ConfigListResult, error) {
	if _, err := s.Store.Images().Get(ctx, imageID); err != nil {
		return ConfigListResult{}, err
	}
	items, err := s.Store.Configs().ListByImage(ctx, imageID)
	if err != nil {
		return ConfigListResult{}, err
	}
	if items == nil {
		items = []domain.Config{}
	}
	return ConfigListResult{Items: items, Total: len(items)}, nil
}

func (s ConfigService) Delete(ctx context.Context, configID string) (ConfigTaskResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	if err := refuseSuperConfigChange(ctx, s.Store, cfg.ID, "删除配置"); err != nil {
		return ConfigTaskResult{}, err
	}

	claim, err := claimConfigImage(ctx, s.Store, domain.TaskTypeDeleteConfig, cfg.ImageID, cfg.ID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	defer claim.Drop()
	if err := s.ensureNotLastConfig(ctx, cfg); err != nil {
		return ConfigTaskResult{}, err
	}
	if err := s.ensureConfigNotInUse(ctx, configID); err != nil {
		return ConfigTaskResult{}, err
	}
	if err := s.ensureNoDependents(ctx, configID, nil); err != nil {
		return ConfigTaskResult{}, err
	}
	if err := s.catalogueBusy("删除配置", configID); err != nil {
		return ConfigTaskResult{}, err
	}
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeDeleteConfig, configID, func(ctx context.Context, task domain.Task) error {
		return s.executeDelete(ctx, task, configID)
	})
	if err != nil {
		return ConfigTaskResult{}, err
	}
	return ConfigTaskResult{TaskID: task.ID}, nil
}

func (s ConfigService) executeDelete(ctx context.Context, task domain.Task, configID string) error {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	if err := s.Storage.DeleteConfig(ctx, configID); err != nil {
		if zfs.IsBusy(err) {
			if busy := s.catalogueBusy("删除配置", configID); busy != nil {
				return busy
			}
		}
		return err
	}
	return s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Reductions().DeleteByConfig(ctx, configID); err != nil {
			return err
		}
		if err := tx.Configs().Delete(ctx, configID); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, configID)
	})
}

func (s ConfigService) Merge(ctx context.Context, configID string) (ConfigTaskResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	if err := refuseSuperImageChange(ctx, s.Store, cfg.ImageID, "合并配置"); err != nil {
		return ConfigTaskResult{}, err
	}

	if err := s.catalogueBusy("合并配置", s.imageDatasets(ctx, cfg.ImageID)...); err != nil {
		return ConfigTaskResult{}, err
	}
	if err := exportRefusal(ctx, s.Store, cfg.ImageID, "合并配置"); err != nil {
		return ConfigTaskResult{}, err
	}
	reductions, err := s.Store.Reductions().ListByConfig(ctx, configID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	// 合并取配置最新内容；当前应用的若是较早还原点，合并会静默撤掉这次回滚，要求改用「覆盖原镜像」。
	if latest := latestReady(reductions); latest != nil && cfg.DefaultReductionID != nil && *cfg.DefaultReductionID != latest.ID {
		applied := *cfg.DefaultReductionID
		for _, r := range reductions {
			if r.ID == applied {
				applied = displayNameOf(r)
			}
		}
		return ConfigTaskResult{}, errs.Conflict(fmt.Sprintf("配置「%s」当前应用的是还原点「%s」，不是最新的「%s」；合并会用「%s」的内容并撤掉这次回滚。要以「%s」为准，请对它用「覆盖原镜像」",
			cfg.Name, applied, displayNameOf(*latest), displayNameOf(*latest), applied))
	}
	return s.mergeInto(ctx, cfg, "", reductionNames(reductions))
}

// OverwriteImage 用一个还原点替换镜像原内容：删除镜像的其它配置和全部还原点，
// 使用该配置的分组改到新基线。还原点不是最新时先回滚到它。
func (s ConfigService) OverwriteImage(ctx context.Context, reductionID string) (ConfigTaskResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	red, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	if red.Status != domain.ReductionStatusReady {
		return ConfigTaskResult{}, errs.Conflict(fmt.Sprintf("还原点「%s」还没就绪，请等它完成后再覆盖", displayNameOf(red)))
	}
	cfg, err := s.Store.Configs().Get(ctx, red.ConfigID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	if err := refuseSuperImageChange(ctx, s.Store, cfg.ImageID, "覆盖原镜像"); err != nil {
		return ConfigTaskResult{}, err
	}

	if err := s.catalogueBusy("覆盖原镜像", s.imageDatasets(ctx, cfg.ImageID)...); err != nil {
		return ConfigTaskResult{}, err
	}
	if err := exportRefusal(ctx, s.Store, cfg.ImageID, "覆盖原镜像"); err != nil {
		return ConfigTaskResult{}, err
	}
	reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	rollbackTo := ""
	if latest := latestReady(reductions); latest != nil && latest.ID != red.ID {
		rollbackTo = strings.TrimPrefix(red.Name, "@")
	}
	var kept []domain.Reduction
	for _, r := range reductions {
		if !r.CreatedAt.After(red.CreatedAt) {
			kept = append(kept, r)
		}
	}
	return s.mergeInto(ctx, cfg, rollbackTo, reductionNames(kept))
}

// mergeInto 做完两个入口共有的检查后把 cfg 合并进镜像。keptNames 是合并时（回滚之后）仍存在的还原点。
func (s ConfigService) mergeInto(ctx context.Context, cfg domain.Config, rollbackTo string, keptNames []string) (ConfigTaskResult, error) {
	configID := cfg.ID
	claim, err := claimImages(ctx, s.Store, domain.TaskTypeMergeConfig, cfg.ImageID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	defer claim.Drop()

	configs, err := s.Store.Configs().ListByImage(ctx, cfg.ImageID)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return ConfigTaskResult{}, err
	}
	// 被合并掉的配置会消失，绑定它的对象须由操作者先改指向；保留的配置 ID 不变，
	// 使用它的分组只需在下面移动还原点。运行中的客户机都要拦住：它的盘克隆自合并要销毁的快照。
	for _, item := range configs {
		if item.ID == configID {
			continue
		}
		if users := ix.describeConfigUsers(item.ID); len(users) > 0 {
			return ConfigTaskResult{}, blocked(ErrConfigInUse, fmt.Sprintf("合并会删除配置「%s」，请先让 %s 改用别的配置", item.Name, strings.Join(users, "、")))
		}
	}
	if clones := ix.clonesUsingConfig(configID); len(clones) > 0 {
		macs := make([]string, 0, len(clones))
		for _, clone := range clones {
			macs = append(macs, formatMAC(clone.TerminalMAC))
		}
		return ConfigTaskResult{}, blocked(ErrConfigInUse, fmt.Sprintf("请先关闭正在使用该配置的客户机：%s", strings.Join(macs, "、")))
	}
	var deleteIDs []string
	for _, item := range configs {
		if item.ID != configID {
			deleteIDs = append(deleteIDs, item.ID)
		}
	}
	doomed := make(map[string]bool, len(deleteIDs)+1)
	for _, id := range deleteIDs {
		doomed[id] = true
	}
	// 旧镜像数据集也会被销毁。已 promote 过的合并重试时，镜像反挂在保留配置下，不能把它当成阻塞项。
	doomed[cfg.ImageID] = true
	if err := s.ensureNoDependents(ctx, configID, doomed); err != nil {
		return ConfigTaskResult{}, err
	}
	repoint := ix.groupsUsingConfig(configID)
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeMergeConfig, configID, func(ctx context.Context, task domain.Task) error {
		return s.executeMerge(ctx, task, cfg.ImageID, configID, deleteIDs, keptNames, rollbackTo, repoint)
	})
	if err != nil {
		return ConfigTaskResult{}, err
	}
	return ConfigTaskResult{TaskID: task.ID}, nil
}

func (s ConfigService) executeMerge(ctx context.Context, task domain.Task, imageID string, configID string, deleteIDs []string, keptNames []string, rollbackTo string, repoint []domain.Group) error {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	merged, err := s.Storage.MergeConfig(ctx, storage.MergeConfigReq{
		ImageID:         imageID,
		ConfigID:        configID,
		DeleteConfigIDs: deleteIDs,
		ReductionNames:  keptNames,
		RollbackTo:      rollbackTo,
	})
	if err != nil {
		return err
	}
	baseline := merged.Reduction
	baseline.ConfigID = configID
	return s.Store.Tx(ctx, func(tx store.Store) error {
		for _, id := range deleteIDs {
			if err := tx.Reductions().DeleteByConfig(ctx, id); err != nil {
				return err
			}
			if err := tx.Configs().Delete(ctx, id); err != nil {
				return err
			}
		}
		// (config_id, name) 唯一，旧「@0」行会挡住新基线；仍被分组引用的旧行又删不掉。
		// 所以顺序是：先改名旧行（快照已并入镜像，名字已无意义），再放基线、移分组，最后删旧行。
		existing, err := tx.Reductions().ListByConfig(ctx, configID)
		if err != nil {
			return err
		}
		for _, item := range existing {
			stale := item
			stale.Name = item.Name + "~merged"
			if err := tx.Reductions().Update(ctx, stale); err != nil {
				return err
			}
		}
		// 基线 ID 合并前后都是「<config>_0」，可能与旧行重合，所以用 upsert。
		if _, err := tx.Reductions().Get(ctx, baseline.ID); err == nil {
			if err := tx.Reductions().Update(ctx, baseline); err != nil {
				return err
			}
		} else if errs.IsNotFound(err) {
			if err := tx.Reductions().Create(ctx, baseline); err != nil {
				return err
			}
		} else {
			return err
		}
		for _, group := range repoint {
			if group.SystemReductionID == baseline.ID {
				continue
			}
			group.SystemReductionID = baseline.ID
			if err := tx.Groups().Update(ctx, group); err != nil {
				return err
			}
		}
		for _, item := range existing {
			if item.ID == baseline.ID {
				continue
			}
			if err := tx.Reductions().Delete(ctx, item.ID); err != nil {
				return err
			}
		}
		if err := setConfigCurrent(ctx, tx, configID, baseline.ID); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, configID)
	})
}

func (s ConfigService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now, Async: s.Async}
}

func (s ConfigService) ensureConfigNameUnique(ctx context.Context, imageID, name string) error {
	configs, err := s.Store.Configs().ListByImage(ctx, imageID)
	if err != nil {
		return err
	}
	for _, config := range configs {
		if config.Name == name {
			return fmt.Errorf("%w: %s", ErrConfigExists, name)
		}
	}
	return nil
}

// ensureNoDependents 向池查询是否还有克隆依赖本配置的快照。数据库答不了：fork 不记来源，
// 孤儿客户机克隆根本没有行。ignore 是本操作会先销毁、因而不算阻塞的数据集。
func (s ConfigService) ensureNoDependents(ctx context.Context, configID string, ignore map[string]bool) error {
	dependents, err := s.Storage.ConfigDependents(ctx, configID)
	if err != nil {
		if errors.Is(err, storage.ErrNotImplemented) {
			return nil
		}
		return err
	}
	blockers := filterDependents(dependents, ignore)
	if len(blockers) == 0 {
		return nil
	}
	return blocked(ErrConfigHasDependents, explainBlockers(ctx, s.Store, blockers, "该配置"))
}

// ensureNotLastConfig 保证镜像至少留一个配置，否则无法分配给分组，也无法做健康检查。
func (s ConfigService) ensureNotLastConfig(ctx context.Context, cfg domain.Config) error {
	siblings, err := s.Store.Configs().ListByImage(ctx, cfg.ImageID)
	if err != nil {
		return err
	}
	if len(siblings) > 1 {
		return nil
	}
	name := cfg.ImageID
	if img, err := s.Store.Images().Get(ctx, cfg.ImageID); err == nil {
		name = img.Name
	}
	return errs.Conflict(fmt.Sprintf("配置「%s」是镜像「%s」的最后一个配置，删掉它镜像就没法再分配给分组；不要这个镜像了请直接删除镜像，否则先新建另一个配置", cfg.Name, name))
}

func (s ConfigService) ensureConfigNotInUse(ctx context.Context, configID string) error {
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return err
	}
	if ix.configInUse(configID) {
		return blocked(ErrConfigInUse, "请先让 "+strings.Join(ix.describeConfigUsers(configID), "、")+" 改用别的配置，再删除它")
	}
	return nil
}

var _ interface {
	CreateFromImage(context.Context, string, CreateConfigRequest) (ConfigOperationResult, error)
	CreateFromConfig(context.Context, string, CreateConfigFromConfigRequest) (ConfigOperationResult, error)
	List(context.Context, string) (ConfigListResult, error)
	Delete(context.Context, string) (ConfigTaskResult, error)
	Merge(context.Context, string) (ConfigTaskResult, error)
} = ConfigService{}

func latestReady(reductions []domain.Reduction) *domain.Reduction {
	var latest *domain.Reduction
	for i, r := range reductions {
		if r.Status == domain.ReductionStatusReady && (latest == nil || r.CreatedAt.After(latest.CreatedAt)) {
			latest = &reductions[i]
		}
	}
	return latest
}

func reductionNames(reductions []domain.Reduction) []string {
	names := make([]string, 0, len(reductions))
	for _, r := range reductions {
		names = append(names, r.Name)
	}
	return names
}
