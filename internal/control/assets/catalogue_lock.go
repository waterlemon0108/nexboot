package assets

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// 一个镜像的配置与还原点在池上是同一棵克隆树，删还原点、合并配置、超管保存都会在其中 promote、rename 或 destroy。
// 两个操作交错会把树留在任何一方都不会产生的状态，且各自的前置检查都在对方改动前完成。
// 所以每个镜像同时只跑一个目录操作，第二个在提交时就点名拒绝，而不是做到一半失败。
// 锁在进程内：目录操作只在写入者上执行；客户机启动、导出、备份只读这棵树，不加锁。
var imagesInUse = struct {
	sync.Mutex
	held map[string]imageOperation
}{held: map[string]imageOperation{}}

type imageOperation struct {
	typ      domain.TaskType
	configID string // 空表示操作会改写该镜像的全部配置。
}

// imageClaim 从提交到任务结束一直持有镜像，使加锁后做的前置检查在任务执行时仍然成立。
//
//	claim, err := claimImages(ctx, st, typ, imageID)
//	if err != nil { return err }
//	defer claim.Drop()
//	... 前置检查 ...
//	task, err := claim.Run(ctx, runner, typ, target, fn)
type imageClaim struct {
	release func()
	handed  bool
}

// claimImages 要么锁住全部镜像，要么一个都不锁。
func claimImages(ctx context.Context, st store.Store, typ domain.TaskType, imageIDs ...string) (*imageClaim, error) {
	ids := uniqueSorted(imageIDs)
	imagesInUse.Lock()
	for _, id := range ids {
		if running, busy := imagesInUse.held[id]; busy {
			imagesInUse.Unlock()
			return nil, errs.Conflict(fmt.Sprintf("镜像 %s 正在%s，请等该任务完成后再操作", imageLabel(ctx, st, id), taskLabel(running.typ)))
		}
	}
	for _, id := range ids {
		imagesInUse.held[id] = imageOperation{typ: typ}
	}
	imagesInUse.Unlock()
	var once sync.Once
	return &imageClaim{release: func() {
		once.Do(func() {
			imagesInUse.Lock()
			for _, id := range ids {
				delete(imagesInUse.held, id)
			}
			imagesInUse.Unlock()
		})
	}}, nil
}

// 仍按镜像串行目录任务，但启用超管时只拒绝正在改写它所用的配置。
func claimConfigImage(ctx context.Context, st store.Store, typ domain.TaskType, imageID, configID string) (*imageClaim, error) {
	claim, err := claimImages(ctx, st, typ, imageID)
	if err != nil {
		return nil, err
	}
	imagesInUse.Lock()
	imagesInUse.held[imageID] = imageOperation{typ: typ, configID: configID}
	imagesInUse.Unlock()
	return claim, nil
}

func refuseSuperDuringConfigChange(ctx context.Context, st store.Store, configIDs map[string]bool) error {
	for configID := range configIDs {
		cfg, err := st.Configs().Get(ctx, configID)
		if err != nil {
			return err
		}
		imagesInUse.Lock()
		operation, busy := imagesInUse.held[cfg.ImageID]
		imagesInUse.Unlock()
		if !busy || (operation.configID != "" && operation.configID != configID) {
			continue
		}
		switch operation.typ {
		case domain.TaskTypeCreateReduction, taskTypeApplyReduction, domain.TaskTypeDeleteReduction,
			domain.TaskTypeMergeReduction, domain.TaskTypeDeleteConfig, domain.TaskTypeMergeConfig, taskTypeDeleteImage:
			return errs.Conflict(fmt.Sprintf("配置「%s」正在%s，请等任务完成后再启用超管模式", cfg.Name, taskLabel(operation.typ)))
		}
	}
	return nil
}

// Drop 释放镜像，除非已由任务接管。
func (c *imageClaim) Drop() {
	if !c.handed {
		c.release()
	}
}

// Run 把镜像交给任务，任务结束时无论同步还是后台执行都会释放。
func (c *imageClaim) Run(ctx context.Context, r tasks.Runner, typ domain.TaskType, target string, fn func(context.Context, domain.Task) error) (domain.Task, error) {
	c.handed = true
	task, err := r.Run(ctx, typ, target, func(ctx context.Context, task domain.Task) error {
		defer c.release()
		return fn(ctx, task)
	})
	if err != nil && task.ID == "" {
		c.release() // 任务行没建成，fn 从未执行
	}
	return task, err
}

// 新镜像的库行要到拷贝结束才写入，拷贝期间只查库挡不住同名的第二次提交。
// 名字和存储层将选的 ID 都要占：纯中文名都折成同一个兜底 ID，存储层要等数据集出现才会避让。
var pendingImages = struct {
	sync.Mutex
	byName map[string]pendingImage
	byID   map[string]pendingImage
}{byName: map[string]pendingImage{}, byID: map[string]pendingImage{}}

type pendingImage struct {
	name string
	verb string
}

// claimImageName 在提交时占住新镜像名，之后再按库查重才不会被并发提交绕过。
// 返回的 imageClaim 用法同 claimImages：Run 交给任务，任务结束释放。
func claimImageName(name, verb string) (*imageClaim, error) {
	id := storage.ImageName(name, nil)
	pendingImages.Lock()
	if p, busy := pendingImages.byName[name]; busy {
		pendingImages.Unlock()
		return nil, errs.Conflict(fmt.Sprintf("镜像「%s」正在%s，请等它完成", p.name, p.verb))
	}
	if p, busy := pendingImages.byID[id]; busy {
		pendingImages.Unlock()
		return nil, errs.Conflict(fmt.Sprintf("请等镜像「%s」%s完成再提交「%s」：两个名字在存储池里会落到同一个名字 %s", p.name, p.verb, name, id))
	}
	p := pendingImage{name: name, verb: verb}
	pendingImages.byName[name], pendingImages.byID[id] = p, p
	pendingImages.Unlock()
	var once sync.Once
	return &imageClaim{release: func() {
		once.Do(func() {
			pendingImages.Lock()
			delete(pendingImages.byName, name)
			delete(pendingImages.byID, id)
			pendingImages.Unlock()
		})
	}}, nil
}

// 导出在镜像上打 ndexport- 快照并持续 send，合并、覆盖或删除镜像会让它中途失败或卡住 destroy。
// 导出是只读的，不占 imagesInUse，只登记在这里供改写类操作在提交时拒绝。
var exportsInFlight = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// beginExport 登记镜像上的一次导出，返回的函数注销它，可重复调用。
func beginExport(imageID string) func() {
	exportsInFlight.Lock()
	exportsInFlight.n[imageID]++
	exportsInFlight.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			exportsInFlight.Lock()
			if exportsInFlight.n[imageID]--; exportsInFlight.n[imageID] <= 0 {
				delete(exportsInFlight.n, imageID)
			}
			exportsInFlight.Unlock()
		})
	}
}

// exportRefusal 在镜像正被导出时拒绝 action。
func exportRefusal(ctx context.Context, st store.Store, imageID, action string) error {
	exportsInFlight.Lock()
	busy := exportsInFlight.n[imageID] > 0
	exportsInFlight.Unlock()
	if !busy {
		return nil
	}
	return errs.Conflict(fmt.Sprintf("镜像「%s」正在导出，请等导出结束再%s", imageLabel(ctx, st, imageID), action))
}

func uniqueSorted(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func imageLabel(ctx context.Context, st store.Store, id string) string {
	if img, err := st.Images().Get(ctx, id); err == nil && img.Name != "" {
		return img.Name
	}
	return id
}

// taskLabel 返回任务列表中显示的操作名（与前端 TASK_LABELS 一致），只列会占用镜像的任务。
func taskLabel(typ domain.TaskType) string {
	switch typ {
	case domain.TaskTypeCreateConfig:
		return "创建配置"
	case domain.TaskTypeDeleteConfig:
		return "删除配置"
	case domain.TaskTypeMergeConfig:
		return "合并配置"
	case domain.TaskTypeCreateReduction:
		return "创建还原点"
	case domain.TaskTypeDeleteReduction:
		return "删除还原点"
	case domain.TaskTypeMergeReduction:
		return "合并还原点"
	case domain.TaskTypeSuperStop:
		return "关机存还原点"
	case domain.TaskTypePublishDataDisk:
		return "发布数据盘"
	case taskTypeDeleteImage:
		return "删除镜像"
	case taskTypeApplyReduction:
		return "应用还原点"
	}
	return string(typ)
}

// 删除镜像和应用还原点同步执行、没有任务行，但同样占用镜像。
const (
	taskTypeDeleteImage    domain.TaskType = "delete_image"
	taskTypeApplyReduction domain.TaskType = "apply_reduction"
)
