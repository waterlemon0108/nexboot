package ops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// 启动时按池里的数据集、快照和血缘重建目录（镜像、配置、还原点）。只增不删：
// 数据集不在的行留着交给 Check 报告，因为没导入的池和被手工删数据的池看起来一样。
// 恢复不了显示名、备注，以及池里本来就没有的分组、客户机、用户：只恢复到可开机。

// RecoveryReport 统计启动扫描收养的对象数。
type RecoveryReport struct {
	Images     int `json:"images"`
	Configs    int `json:"configs"`
	Reductions int `json:"reductions"`
}

func (r RecoveryReport) empty() bool { return r.Images == 0 && r.Configs == 0 && r.Reductions == 0 }

func (r RecoveryReport) String() string {
	return fmt.Sprintf("镜像 %d、配置 %d、还原点 %d", r.Images, r.Configs, r.Reductions)
}

// RecoverCatalogue 收养池里有而库里没有记录的镜像、配置和还原点。
func (s ReconcileService) RecoverCatalogue(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport
	if s.Storage == nil {
		return report, fmt.Errorf("catalogue recovery has no storage to read")
	}
	inv, err := s.Storage.Inventory(ctx)
	if err != nil {
		return report, err
	}
	known, err := s.knownDatasets(ctx)
	if err != nil {
		return report, err
	}

	// 先处理根：非克隆且已有基线 @0 的数据集是镜像。导入最后一步才打 @0，没有 @0
	// 的是未完成的导入（复制也会带过来），收养了就是一块开不了机的盘。
	snapshots := make(map[string]bool, len(inv.Snapshots))
	for _, snap := range inv.Snapshots {
		snapshots[snap] = true
	}
	datasets := append([]string(nil), inv.Datasets...)
	sort.Strings(datasets)
	imageOf := map[string]string{} // 数据集 -> 它最终所属的镜像
	for _, dataset := range datasets {
		if _, cloned := inv.Origins[dataset]; cloned || ignoredDataset(dataset) {
			continue
		}
		if !known[dataset] && !snapshots[dataset+"@"+storage.BaselineReduction] {
			continue
		}
		imageOf[dataset] = dataset
		if known[dataset] {
			continue
		}
		if err := s.adoptImage(ctx, dataset); err != nil {
			return report, err
		}
		report.Images++
	}

	// 再按依赖顺序处理克隆：配置挂在镜像上，fork 的配置挂在别的配置上，父先于子，
	// 循环到没有新的可解析为止。
	for progress := true; progress; {
		progress = false
		for _, dataset := range datasets {
			origin, cloned := inv.Origins[dataset]
			if !cloned || ignoredDataset(dataset) {
				continue
			}
			if _, done := imageOf[dataset]; done {
				continue
			}
			parent, _, _ := strings.Cut(origin, "@")
			image, ok := imageOf[parent]
			if !ok {
				continue // 父数据集还没处理到
			}
			imageOf[dataset] = image
			progress = true
			if known[dataset] {
				continue
			}
			if err := s.adoptConfig(ctx, dataset, image); err != nil {
				return report, err
			}
			report.Configs++
		}
	}

	// 最后把配置的快照收为还原点；镜像自己的快照是基线，没有对应行。
	adopted, err := s.adoptReductions(ctx, inv, imageOf)
	if err != nil {
		return report, err
	}
	report.Reductions = adopted

	if !report.empty() && s.Logger != nil {
		s.Logger.Warn("rebuilt the catalogue from the pool — the database did not describe data the pool still holds",
			"recovered", report.String())
	}
	return report, nil
}

// knownDatasets 返回库里已登记的数据集（镜像和配置），按池内相对名。
func (s ReconcileService) knownDatasets(ctx context.Context) (map[string]bool, error) {
	known := map[string]bool{}
	images, err := s.Store.Images().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, img := range images {
		known[img.ID] = true
		configs, err := s.Store.Configs().ListByImage(ctx, img.ID)
		if err != nil {
			return nil, err
		}
		for _, cfg := range configs {
			known[cfg.ID] = true
		}
	}
	return known, nil
}

func (s ReconcileService) adoptImage(ctx context.Context, dataset string) error {
	// 系统类型从盘上判断：猜错会让该镜像所有客户机拿到错误的启动脚本。
	osType := domain.OSTypeWindows
	if detected, ok := s.Storage.DetectOSType(ctx, dataset); ok {
		osType = detected
	} else if s.Logger != nil {
		s.Logger.Warn("recovered an image whose OS could not be read from its partitions; assuming Windows",
			"image", dataset)
	}
	return s.Store.Images().Create(ctx, domain.Image{
		ID:     dataset,
		Name:   dataset, // 显示名只存在于丢失的库里
		OSType: osType,
		State:  domain.ImageStateNormal,
		Remark: "由池恢复",
		// 大小和烘焙脚本版本未知；版本留空会让开机路径按客户机注入，结果正确。
		CreatedAt: s.now(),
	})
}

func (s ReconcileService) adoptConfig(ctx context.Context, dataset, imageID string) error {
	return s.Store.Configs().Create(ctx, domain.Config{
		ID:        dataset,
		ImageID:   imageID,
		Name:      strings.TrimPrefix(dataset, imageID+"_"),
		CreatedAt: s.now(),
	})
}

// adoptReductions 把配置的快照收为还原点，并补上默认开机的还原点。
func (s ReconcileService) adoptReductions(ctx context.Context, inv storage.PoolInventory, imageOf map[string]string) (int, error) {
	bySnapshot := map[string][]string{}
	for _, snapshot := range inv.Snapshots {
		dataset, name, ok := strings.Cut(snapshot, "@")
		if !ok || dataset == imageOf[dataset] || ignoredDataset(dataset) {
			continue // 镜像自己的基线没有对应行
		}
		if storage.SystemSnapshot(name) {
			continue // 复制/备份记账快照，不是还原点
		}
		if _, isConfig := imageOf[dataset]; !isConfig {
			continue
		}
		bySnapshot[dataset] = append(bySnapshot[dataset], name)
	}

	adopted := 0
	for dataset, names := range bySnapshot {
		cfg, err := s.Store.Configs().Get(ctx, dataset)
		if err != nil {
			continue // 配置未收养，无处挂靠
		}
		existing, err := s.Store.Reductions().ListByConfig(ctx, dataset)
		if err != nil {
			return adopted, err
		}
		have := map[string]bool{}
		for _, red := range existing {
			have[strings.TrimPrefix(red.Name, "@")] = true
		}
		sort.Strings(names)
		for _, name := range names {
			if have[name] {
				continue
			}
			red := domain.Reduction{
				ID:          storage.ReductionID(dataset, name),
				ConfigID:    dataset,
				Name:        "@" + name,
				DisplayName: name,
				CreatedAt:   s.now(),
				Status:      domain.ReductionStatusReady,
			}
			if err := s.Store.Reductions().Create(ctx, red); err != nil {
				return adopted, err
			}
			adopted++
			// 没有默认还原点的配置无法被分组绑定。
			if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID == "" {
				id := red.ID
				cfg.DefaultReductionID = &id
				if err := s.Store.Configs().Update(ctx, cfg); err != nil {
					return adopted, err
				}
			}
		}
	}
	return adopted, nil
}
