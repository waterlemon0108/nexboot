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
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrReductionExists        = errs.Conflict("还原点已存在")
	ErrReductionInUse         = errs.Conflict("还原点正在使用中")
	ErrReductionHasDependents = errs.Conflict("该还原点正被其它对象占用")
	ErrReductionNameRequired  = errs.Invalid("还原点名称必填")
	ErrReductionNameTooLong   = errs.Invalid("还原点名称最多 60 个字符")
	ErrReductionKeepRequired  = errs.Invalid("必须指定要保留的还原点")
)

// reductionNameMaxRunes 远低于 ZFS 限制，是为了名字在界面列表里放得下。
const reductionNameMaxRunes = 60

// validateReductionName 返回原样保留的显示名和要创建的 ascii 快照名。手动新建和超管机保存都走这里，
// 以便在转入异步、以 zfs 原文失败之前，按同样理由拒绝同样的名字。
// 只对显示名做不区分大小写的查重；不同名字折成同一快照名不算冲突，由 SnapshotName 区分。
func validateReductionName(raw string, existing []domain.Reduction) (display, snapshot string, err error) {
	// 开头的「@」是 ZFS 快照分隔符而非名字的一部分：输入「@0」应与已有的「0」冲突，而不是另建一个。
	display = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "@"))
	if display == "" {
		return "", "", ErrReductionNameRequired
	}
	if len([]rune(display)) > reductionNameMaxRunes {
		return "", "", ErrReductionNameTooLong
	}
	taken := make(map[string]bool, len(existing))
	for _, item := range existing {
		if strings.EqualFold(displayNameOf(item), display) {
			return "", "", ErrReductionExists
		}
		taken[strings.TrimPrefix(item.Name, "@")] = true
	}
	return display, "@" + storage.SnapshotName(display, func(name string) bool { return taken[name] }), nil
}

// displayNameOf 返回还原点的显示名；早期行没有显示名，退回快照名。
func displayNameOf(r domain.Reduction) string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return strings.TrimPrefix(r.Name, "@")
}

type ReductionService struct {
	Store   store.Store
	Storage storage.StorageAgent
	Now     func() time.Time
	Async   bool
}

type ReductionListResult struct {
	Items []domain.Reduction `json:"items"`
	Total int                `json:"total"`
}

type CreateReductionRequest struct {
	Name string `json:"name"`
	// SetCurrent 为真时同时把新还原点设为配置当前应用点，跟随该配置的分组一并切换。
	SetCurrent bool `json:"set_current"`
}

// SetCurrentResult 返回配置当前应用点的新位置。
type SetCurrentResult struct {
	ConfigID    string `json:"config_id"`
	ReductionID string `json:"reduction_id"`
}

type MergeReductionsRequest struct {
	KeepReductionID    string   `json:"keep_reduction_id"`
	DeleteReductionIDs []string `json:"delete_reduction_ids"`
}

type ReductionOperationResult struct {
	TaskID    string           `json:"task_id"`
	Reduction domain.Reduction `json:"reduction"`
}

type ReductionTaskResult struct {
	TaskID string `json:"task_id"`
}

func (s ReductionService) List(ctx context.Context, configID string) (ReductionListResult, error) {
	if _, err := s.Store.Configs().Get(ctx, configID); err != nil {
		return ReductionListResult{}, err
	}
	items, err := s.Store.Reductions().ListByConfig(ctx, configID)
	if err != nil {
		return ReductionListResult{}, err
	}
	if items == nil {
		items = []domain.Reduction{}
	}
	return ReductionListResult{Items: items, Total: len(items)}, nil
}

func (s ReductionService) Create(ctx context.Context, configID string, req CreateReductionRequest) (ReductionOperationResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	if strings.TrimSpace(req.Name) == "" {
		return ReductionOperationResult{}, ErrReductionNameRequired
	}
	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return ReductionOperationResult{}, err
	}
	claim, err := claimConfigImage(ctx, s.Store, domain.TaskTypeCreateReduction, cfg.ImageID, cfg.ID)
	if err != nil {
		return ReductionOperationResult{}, err
	}
	defer claim.Drop()
	if err := refuseSuperConfigChange(ctx, s.Store, configID, "新建还原点"); err != nil {
		return ReductionOperationResult{}, err
	}

	reductions, err := s.Store.Reductions().ListByConfig(ctx, configID)
	if err != nil {
		return ReductionOperationResult{}, err
	}
	display, snapshot, err := validateReductionName(req.Name, reductions)
	if err != nil {
		return ReductionOperationResult{}, err
	}
	var reduction domain.Reduction
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeCreateReduction, configID, func(ctx context.Context, task domain.Task) error {
		var err error
		reduction, err = s.executeCreate(ctx, task, cfg, configID, display, snapshot, req.SetCurrent)
		return err
	})
	if err != nil {
		return ReductionOperationResult{}, err
	}
	if s.Async {
		return ReductionOperationResult{TaskID: task.ID}, nil
	}
	return ReductionOperationResult{TaskID: task.ID, Reduction: reduction}, nil
}

func (s ReductionService) executeCreate(ctx context.Context, task domain.Task, cfg domain.Config, configID, display, snapshot string, setCurrent bool) (domain.Reduction, error) {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	reduction, err := s.Storage.CreateReduction(ctx, configID, snapshot)
	if err != nil {
		return domain.Reduction{}, err
	}
	reduction.ConfigID = configID
	reduction.Name = snapshot
	reduction.DisplayName = display
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Reductions().Create(ctx, reduction); err != nil {
			return err
		}
		if cfg.DefaultReductionID == nil || setCurrent {
			if err := setConfigCurrent(ctx, tx, configID, reduction.ID); err != nil {
				return err
			}
		}
		return s.runner().Finish(ctx, tx, task, reduction.ID)
	}); err != nil {
		_ = s.Storage.DeleteReduction(ctx, configID, reduction.Name)
		return domain.Reduction{}, err
	}
	return reduction, nil
}

// SetCurrent 应用一个还原点：配置下的普通机下次开机即用它，用于一键下发或回滚。
// 只移动指针、不动存储，运行中的机器不受影响；配置下的分组一并切换。
func (s ReductionService) SetCurrent(ctx context.Context, reductionID string) (SetCurrentResult, error) {
	reduction, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return SetCurrentResult{}, err
	}
	if reduction.Status != domain.ReductionStatusReady {
		return SetCurrentResult{}, errs.Conflict(fmt.Sprintf("还原点 %s 还没就绪（%s），就绪后再应用", displayOf(reduction), reduction.Status))
	}
	cfg, err := s.Store.Configs().Get(ctx, reduction.ConfigID)
	if err != nil {
		return SetCurrentResult{}, err
	}
	// 占住镜像，防止已提交的删除在它刚成为当前点后把它删掉、静默改走指针。
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	if err := refuseSuperConfigChange(ctx, s.Store, cfg.ID, "应用还原点"); err != nil {
		return SetCurrentResult{}, err
	}

	claim, err := claimConfigImage(ctx, s.Store, taskTypeApplyReduction, cfg.ImageID, cfg.ID)
	if err != nil {
		return SetCurrentResult{}, err
	}
	defer claim.Drop()
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		return setConfigCurrent(ctx, tx, cfg.ID, reduction.ID)
	}); err != nil {
		return SetCurrentResult{}, err
	}
	return SetCurrentResult{ConfigID: cfg.ID, ReductionID: reduction.ID}, nil
}

func displayOf(r domain.Reduction) string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return r.Name
}

// setConfigCurrent 移动配置的当前应用点并同步改该配置下所有分组的还原点 ID（启动、DHCP、删除守卫读的是它），
// 所以所有 DefaultReductionID 写入都必须走这里。
// 在事务内重读配置和分组：调用方读取后可能已有删除或编辑提交，写回旧副本会指向不存在的还原点。
func setConfigCurrent(ctx context.Context, tx store.Store, configID, reductionID string) error {
	cfg, err := tx.Configs().Get(ctx, configID)
	if err != nil {
		return err
	}
	reduction, err := tx.Reductions().Get(ctx, reductionID)
	if errs.IsNotFound(err) || (err == nil && reduction.ConfigID != configID) {
		return errs.Conflict(fmt.Sprintf("要应用的还原点已在此期间被删除，配置 %s 的应用点没有改动；请刷新后重新选择", cfg.Name))
	}
	if err != nil {
		return err
	}
	cfg.DefaultReductionID = &reductionID
	if err := tx.Configs().Update(ctx, cfg); err != nil {
		return err
	}
	return retargetGroupsOnConfig(ctx, tx, cfg.ID, reductionID)
}

func retargetGroupsOnConfig(ctx context.Context, tx store.Store, configID, reductionID string) error {
	groups, err := tx.Groups().List(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.SystemConfigID != configID || g.SystemReductionID == reductionID {
			continue
		}
		g.SystemReductionID = reductionID
		if err := tx.Groups().Update(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

func (s ReductionService) Delete(ctx context.Context, reductionID string) (ReductionTaskResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	reduction, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	cfg, err := s.Store.Configs().Get(ctx, reduction.ConfigID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	if err := refuseSuperConfigChange(ctx, s.Store, cfg.ID, "删除还原点"); err != nil {
		return ReductionTaskResult{}, err
	}

	claim, err := claimConfigImage(ctx, s.Store, domain.TaskTypeDeleteReduction, cfg.ImageID, cfg.ID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	defer claim.Drop()
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	if err := s.ensureReductionRemovable(ctx, ix, &cfg, reduction); err != nil {
		return ReductionTaskResult{}, err
	}
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeDeleteReduction, reductionID, func(ctx context.Context, task domain.Task) error {
		return s.executeDelete(ctx, task, reduction, reductionID)
	})
	if err != nil {
		return ReductionTaskResult{}, err
	}
	return ReductionTaskResult{TaskID: task.ID}, nil
}

func (s ReductionService) executeDelete(ctx context.Context, task domain.Task, reduction domain.Reduction, reductionID string) error {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	if err := s.Storage.DeleteReduction(ctx, reduction.ConfigID, reduction.Name); err != nil {
		return err
	}
	return s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Reductions().Delete(ctx, reductionID); err != nil {
			return err
		}
		if err := s.repairDefaultReduction(ctx, tx, reduction.ConfigID, ""); err != nil {
			return err
		}
		return s.runner().Finish(ctx, tx, task, reductionID)
	})
}

func (s ReductionService) Merge(ctx context.Context, configID string, req MergeReductionsRequest) (ReductionTaskResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	req.KeepReductionID = strings.TrimSpace(req.KeepReductionID)
	if req.KeepReductionID == "" {
		return ReductionTaskResult{}, ErrReductionKeepRequired
	}
	keep, err := s.Store.Reductions().Get(ctx, req.KeepReductionID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	if keep.ConfigID != configID {
		return ReductionTaskResult{}, ErrReductionMismatch
	}
	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	if err := refuseSuperConfigChange(ctx, s.Store, cfg.ID, "合并还原点"); err != nil {
		return ReductionTaskResult{}, err
	}

	claim, err := claimConfigImage(ctx, s.Store, domain.TaskTypeMergeReduction, cfg.ImageID, cfg.ID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	defer claim.Drop()
	reductions, err := s.Store.Reductions().ListByConfig(ctx, configID)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	wanted := map[string]bool{}
	for _, id := range req.DeleteReductionIDs {
		if id = strings.TrimSpace(id); id != "" && id != keep.ID {
			wanted[id] = true
		}
	}
	for id := range wanted {
		item, err := s.Store.Reductions().Get(ctx, id)
		if err != nil {
			return ReductionTaskResult{}, err
		}
		if item.ConfigID != configID {
			return ReductionTaskResult{}, ErrReductionMismatch
		}
	}
	// 按列表顺序而非 map 迭代：ZFS 逐个销毁、可能中途失败，失败后哪些已删必须可预测。
	var doomed []domain.Reduction
	for _, item := range reductions {
		if item.ID == keep.ID || (len(wanted) > 0 && !wanted[item.ID]) {
			continue
		}
		doomed = append(doomed, item)
	}
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return ReductionTaskResult{}, err
	}
	for _, item := range doomed {
		if err := s.ensureReductionRemovable(ctx, ix, nil, item); err != nil {
			return ReductionTaskResult{}, err
		}
	}
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeMergeReduction, configID, func(ctx context.Context, task domain.Task) error {
		return s.executeMerge(ctx, task, configID, keep.ID, doomed)
	})
	if err != nil {
		return ReductionTaskResult{}, err
	}
	return ReductionTaskResult{TaskID: task.ID}, nil
}

func (s ReductionService) executeMerge(ctx context.Context, task domain.Task, configID string, keepID string, doomed []domain.Reduction) error {
	// 池与数据库在此一起变更，中间不得插入复制轮次（见 storage.ChangeCatalogue）。
	defer storage.ChangeCatalogue()()
	names := make([]string, 0, len(doomed))
	for _, item := range doomed {
		names = append(names, item.Name)
	}
	destroyed, mergeErr := s.Storage.MergeReduction(ctx, configID, names)
	gone := make(map[string]bool, len(destroyed))
	for _, name := range destroyed {
		gone[name] = true
	}
	txErr := s.Store.Tx(ctx, func(tx store.Store) error {
		// 只删 ZFS 实际销毁的行；部分失败时其余行保留，库里不会出现快照已没的还原点。
		for _, item := range doomed {
			if !gone[item.Name] {
				continue
			}
			if err := tx.Reductions().Delete(ctx, item.ID); err != nil {
				return err
			}
		}
		if err := s.repairDefaultReduction(ctx, tx, configID, keepID); err != nil {
			return err
		}
		if mergeErr != nil {
			return nil // the runner marks the task failed; the rows above are already true
		}
		return s.runner().Finish(ctx, tx, task, keepID)
	})
	if mergeErr != nil {
		return mergeErr
	}
	return txErr
}

// ensureReductionRemovable 同时查库内引用（分组、客户机克隆）和池上依赖（fork 直接克隆快照，库里不记）。
// 不查池，ZFS 会在执行中以原文失败，还会建议会连带删掉依赖的 `destroy -R`。
// 调用方自己改配置指向时（合并）cfg 为 nil。
func (s ReductionService) ensureReductionRemovable(ctx context.Context, ix refIndex, cfg *domain.Config, reduction domain.Reduction) error {
	dependents, err := s.Storage.ReductionDependents(ctx, reduction.ConfigID, reduction.Name)
	if err != nil {
		if errors.Is(err, storage.ErrNotImplemented) {
			// 存储层答不了这个问题时，「正在使用」仍然要拦。
			if err := s.ensureNotApplied(ctx, ix, cfg, reduction); err != nil {
				return err
			}
			if ix.reductionInUse(reduction.ID) {
				return ErrReductionInUse
			}
			return nil
		}
		// 快照已丢失的还原点开不了机，必须删得掉：无人引用直接放行；仍被引用时不替运维决定改指哪里，
		// 但要说清先切当前应用点再删，而不是只回「正在使用中」。
		if errors.Is(err, storage.ErrSnapshotMissing) {
			if !ix.reductionInUse(reduction.ID) {
				return nil
			}
			return errs.Conflict(fmt.Sprintf(
				"还原点 %s 的快照在存储池上已丢失，它已经不能用来开机，但仍被引用着。"+
					"请先把该配置的当前应用点切到其它还原点，再删除它",
				displayNameOf(reduction)))
		}
		return err
	}
	if len(dependents) > 0 {
		return blocked(ErrReductionHasDependents, explainBlockers(ctx, s.Store, dependents, "该还原点"))
	}
	if err := s.ensureNotApplied(ctx, ix, cfg, reduction); err != nil {
		return err
	}
	if ix.reductionInUse(reduction.ID) {
		return ErrReductionInUse
	}
	return nil
}

// repairDefaultReduction 保证配置默认还原点存在。用户选的默认点只要还在就不动；
// 它没了才改用 fallbackID（合并保留的点），配置没有还原点时清空默认值。
func (s ReductionService) repairDefaultReduction(ctx context.Context, st store.Store, configID string, fallbackID string) error {
	cfg, err := st.Configs().Get(ctx, configID)
	if errs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	remaining, err := st.Reductions().ListByConfig(ctx, configID)
	if err != nil {
		return err
	}
	alive := make(map[string]bool, len(remaining))
	for _, item := range remaining {
		alive[item.ID] = true
	}
	if cfg.DefaultReductionID != nil && alive[*cfg.DefaultReductionID] {
		return nil
	}
	var next *string
	switch {
	case alive[fallbackID]:
		next = &fallbackID
	case len(remaining) > 0:
		last := remaining[len(remaining)-1].ID
		next = &last
	}
	if next != nil {
		return setConfigCurrent(ctx, st, configID, *next)
	}
	cfg.DefaultReductionID = nil
	return st.Configs().Update(ctx, cfg)
}

func (s ReductionService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now, Async: s.Async}
}

func normalizeReductionName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "@") {
		return name
	}
	return "@" + name
}

// ensureNotApplied 拒绝删除配置当前应用的或唯一的还原点；快照已丢失的还原点不走这里。
func (s ReductionService) ensureNotApplied(ctx context.Context, ix refIndex, cfg *domain.Config, reduction domain.Reduction) error {
	if cfg == nil {
		return nil
	}
	siblings, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
	if err != nil {
		return err
	}
	if len(siblings) <= 1 {
		return errs.Conflict(fmt.Sprintf("还原点「%s」是配置「%s」唯一的还原点，删掉它配置就没有可开机的内容；请先新建一个还原点，或者删除整个配置",
			displayNameOf(reduction), cfg.Name))
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != reduction.ID {
		return nil
	}
	who := ""
	if users := ix.describeConfigUsers(cfg.ID); len(users) > 0 {
		who = "，" + strings.Join(users, "、") + " 正用它开机"
	}
	return errs.Conflict(fmt.Sprintf("还原点「%s」是配置「%s」当前应用的还原点%s；请先应用该配置的其它还原点，再删除它",
		displayNameOf(reduction), cfg.Name, who))
}
