package ops

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// Issue* 是一致性检查报告的差异类型。数据库与池靠操作顺序手工保持一致，中途
// 失败会产生不可见的漂移。检查只报告不修复：哪边对取决于操作者当时在做什么，
// 猜错会销毁本该保留的数据。
const (
	IssueMissingDataset  = "missing_dataset"
	IssueMissingSnapshot = "missing_snapshot"
	IssueOrphanDataset   = "orphan_dataset"
	IssueOrphanSnapshot  = "orphan_snapshot"
)

type ConsistencyIssue struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Detail string `json:"detail"`
	// Label 是 missing_dataset 对应的镜像或配置名称。
	Label string `json:"-"`
}

type ConsistencyReport struct {
	CheckedAt time.Time          `json:"checked_at"`
	OK        bool               `json:"ok"`
	Issues    []ConsistencyIssue `json:"issues"`
}

type ReconcileService struct {
	Store   store.Store
	Storage storage.StorageAgent
	Logger  *slog.Logger
	Now     func() time.Time
}

func (s ReconcileService) Check(ctx context.Context) (ConsistencyReport, error) {
	return s.check(ctx, true)
}

// check 比对数据库与池。skipImports 跳过正在导入或复制的对象，只在这些任务真正
// 运行的节点上为 true：备机库里写入者的任务会一直显示「运行中」。
func (s ReconcileService) check(ctx context.Context, skipImports bool) (ConsistencyReport, error) {
	if s.Storage == nil {
		// 在告警巡检 goroutine 中运行，空指针会让整个进程崩溃。
		return ConsistencyReport{}, fmt.Errorf("consistency check has no storage to read")
	}
	// 删除、合并、超管保存在这把锁下同时改池和行，在其中间读会把正常删除报成漂移。
	// 等待有上限：宁可跳过一轮也不卡住清单请求或告警巡检。
	holdCtx, cancel := context.WithTimeout(ctx, catalogueWait)
	release, err := storage.HoldCatalogueContext(holdCtx)
	cancel()
	if err != nil {
		return ConsistencyReport{}, fmt.Errorf("catalogue busy, consistency check skipped: %w", err)
	}
	defer release()
	// 先读任务：读行期间完成的导入仍会被跳过，不会被看成既无行也无任务。
	busy := func(string) bool { return false }
	if skipImports {
		if busy, err = s.importing(ctx); err != nil {
			return ConsistencyReport{}, err
		}
	}
	// 先读库再读池：操作在 zfs 步骤之后才写行，反过来读会把中途完成的配置看成有行无数据集。
	type configRows struct {
		cfg        domain.Config
		reductions []domain.Reduction
	}
	type imageRows struct {
		img     domain.Image
		configs []configRows
	}
	images, err := s.Store.Images().List(ctx)
	if err != nil {
		return ConsistencyReport{}, err
	}
	rows := make([]imageRows, 0, len(images))
	for _, img := range images {
		configs, err := s.Store.Configs().ListByImage(ctx, img.ID)
		if err != nil {
			return ConsistencyReport{}, err
		}
		ir := imageRows{img: img}
		for _, cfg := range configs {
			reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
			if err != nil {
				return ConsistencyReport{}, err
			}
			ir.configs = append(ir.configs, configRows{cfg: cfg, reductions: reductions})
		}
		rows = append(rows, ir)
	}

	inv, err := s.Storage.Inventory(ctx)
	if err != nil {
		// 读不到池是错误，不是报一堆差异。
		return ConsistencyReport{}, err
	}
	datasets := make(map[string]bool, len(inv.Datasets))
	for _, d := range inv.Datasets {
		datasets[d] = true
	}
	snapshots := make(map[string]bool, len(inv.Snapshots))
	for _, s := range inv.Snapshots {
		snapshots[s] = true
	}

	var issues []ConsistencyIssue
	claimed := map[string]bool{}
	// 镜像没有还原点行，它的基线快照归镜像本身；配置的快照归对应的还原点。
	claimedSnapshots := map[string]bool{}
	for _, ir := range rows {
		img := ir.img
		claimed[img.ID] = true
		claimedSnapshots[img.ID+"@"+storage.BaselineReduction] = true
		if !datasets[img.ID] && !busy(img.ID) {
			issues = append(issues, ConsistencyIssue{
				Kind: IssueMissingDataset, Ref: img.ID, Label: "镜像「" + img.Name + "」",
				Detail: fmt.Sprintf("镜像「%s」的数据集不存在，该镜像已无法使用", img.Name),
			})
		}
		for _, cr := range ir.configs {
			cfg := cr.cfg
			claimed[cfg.ID] = true
			if !datasets[cfg.ID] && !busy(cfg.ID) {
				issues = append(issues, ConsistencyIssue{
					Kind: IssueMissingDataset, Ref: cfg.ID, Label: "配置「" + cfg.Name + "」",
					Detail: fmt.Sprintf("配置「%s」的数据集不存在，客户机无法从它开机", cfg.Name),
				})
			}
			for _, red := range cr.reductions {
				snap := cfg.ID + "@" + strings.TrimPrefix(red.Name, "@")
				claimedSnapshots[snap] = true
				if !snapshots[snap] && !busy(snap) {
					issues = append(issues, ConsistencyIssue{
						Kind: IssueMissingSnapshot, Ref: red.ID,
						Detail: fmt.Sprintf("配置「%s」的还原点 %s 快照不存在，用它开机会失败", cfg.Name, red.Name),
					})
				}
			}
		}
	}

	orphanDatasets := map[string]bool{}
	for _, dataset := range inv.Datasets {
		if claimed[dataset] || ignoredDataset(dataset) || busy(dataset) {
			continue
		}
		orphanDatasets[dataset] = true
		issues = append(issues, ConsistencyIssue{
			Kind: IssueOrphanDataset, Ref: dataset,
			Detail: fmt.Sprintf("数据集 %s 不属于任何镜像或配置，占用空间且可能阻塞删除", dataset),
		})
	}

	// 无主快照除了占空间，还参与 ZFS 依赖关系，可能挡住删除或与合并的 promote 冲突。
	// 位于已报告的无主数据集上的快照不重复报。
	for _, snapshot := range inv.Snapshots {
		dataset, name, _ := strings.Cut(snapshot, "@")
		if claimedSnapshots[snapshot] || orphanDatasets[dataset] || ignoredDataset(dataset) || busy(dataset) {
			continue
		}
		// 复制和备份标记是产品自己的记账快照，不算漂移。
		if storage.SystemSnapshot(name) {
			continue
		}
		issues = append(issues, ConsistencyIssue{
			Kind: IssueOrphanSnapshot, Ref: snapshot,
			Detail: fmt.Sprintf("快照 %s 不属于任何还原点，占用空间，并可能挡住该配置的删除或合并", snapshot),
		})
	}

	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Kind < issues[j].Kind })
	return ConsistencyReport{CheckedAt: s.now(), OK: len(issues) == 0, Issues: issues}, nil
}

const catalogueWait = 10 * time.Second

// importing 判断名字是否属于正在导入或复制的对象：这两类任务在目录锁外跑几分钟，
// 且先建数据集后写行。
func (s ReconcileService) importing(ctx context.Context) (func(string) bool, error) {
	active, err := s.Store.Tasks().ListActive(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, t := range active {
		if t.TargetRef == "" || (t.Type != domain.TaskTypeImportImage && t.Type != domain.TaskTypeCopyImage) {
			continue
		}
		names[t.TargetRef] = true
		names[storage.DefaultConfigName(t.TargetRef)] = true
	}
	return func(name string) bool {
		dataset, _, _ := strings.Cut(name, "@")
		return names[dataset]
	}, nil
}

// MissingDatasets 列出库里有记录而池里没有数据集的镜像和配置，供备机接任前核对库副本。
func (s ReconcileService) MissingDatasets(ctx context.Context) ([]string, error) {
	report, err := s.check(ctx, false)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, issue := range report.Issues {
		if issue.Kind == IssueMissingDataset {
			missing = append(missing, issue.Label)
		}
	}
	return missing, nil
}

// ignoredDataset 跳过不归本检查判断的数据集：导入目录、保留子数据集，以及由离线
// 回收管理的各类克隆（否则每台在运行的客户机都会被报出来）。
func ignoredDataset(name string) bool {
	if name == "imports" || storage.ReservedCatalogueChild(name) {
		return true
	}
	switch storage.Classify(name).Kind {
	case storage.CarrierClient, storage.CarrierSuper, storage.CarrierInspect, storage.CarrierExport:
		return true
	}
	return false
}

func (s ReconcileService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
