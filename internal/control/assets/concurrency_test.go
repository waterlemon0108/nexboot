package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// interleavedStore 在第一次读配置后插入别的操作，模拟两个请求交错时前者已读未写的空档。
type interleavedStore struct {
	store.Store
	afterConfigGet func()
}

func (s *interleavedStore) Configs() store.ConfigRepo {
	return interleavedConfigs{ConfigRepo: s.Store.Configs(), hook: &s.afterConfigGet}
}

type interleavedConfigs struct {
	store.ConfigRepo
	hook *func()
}

func (c interleavedConfigs) Get(ctx context.Context, id string) (domain.Config, error) {
	cfg, err := c.ConfigRepo.Get(ctx, id)
	if h := *c.hook; h != nil {
		*c.hook = nil
		h()
	}
	return cfg, err
}

// 「应用还原点 R」读完配置、写回前，「删除 R」已完成；若应用继续写入，当前点和分组都指向已不存在的 R，整组开不了机。
func TestApplyRacingADeleteOfTheSameRestorePointLeavesNoDanglingPointer(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t) // cfg-1 当前点 red-1；另有 red-2
	fake := &fakeReductionStorage{}
	deleter := ReductionService{Store: st, Storage: fake}
	racing := &interleavedStore{Store: st, afterConfigGet: func() {
		if _, err := deleter.Delete(ctx, "red-2"); err != nil {
			t.Fatalf("插入的删除失败: %v", err)
		}
	}}

	_, applyErr := (ReductionService{Store: racing, Storage: fake}).SetCurrent(ctx, "red-2")

	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID != nil && *cfg.DefaultReductionID == "red-2" {
		t.Fatalf("当前点悬空：指向已删除的 red-2（应用返回 err=%v）", applyErr)
	}
	if applyErr == nil {
		t.Fatal("还原点在写回前已被删除，应用应当失败并说明原因")
	}
}

// blockingCreate 让「创建还原点」停在存储那一步，直到测试放行。
type blockingCreate struct {
	*fakeReductionStorage
	entered chan struct{}
	release chan struct{}
}

func (s *blockingCreate) CreateReduction(ctx context.Context, configID, name string) (domain.Reduction, error) {
	close(s.entered)
	<-s.release
	return s.fakeReductionStorage.CreateReduction(ctx, configID, name)
}

// 同一镜像上已有操作在跑时，再提交的操作被拒绝，并点名镜像和正在进行的操作；前者结束后锁释放。
func TestCatalogueOperationOnABusyImageIsRefusedAtSubmit(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	slow := &blockingCreate{fakeReductionStorage: &fakeReductionStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	creating := ReductionService{Store: st, Storage: slow, Async: true}
	created, err := creating.Create(ctx, "cfg-1", CreateReductionRequest{Name: "v3"})
	if err != nil {
		t.Fatal(err)
	}
	<-slow.entered

	other := ReductionService{Store: st, Storage: &fakeReductionStorage{}}
	_, err = other.Delete(ctx, "red-2")
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win") || !strings.Contains(err.Error(), "创建还原点") {
		t.Fatalf("镜像忙时删除应被拒绝并点名镜像和正在进行的操作，得到 %v", err)
	}
	close(slow.release)
	waitTaskDone(t, st, created.TaskID)

	if _, err := other.Delete(ctx, "red-2"); err != nil {
		t.Fatalf("前一个操作完成后应能删除: %v", err)
	}
}

func waitTaskDone(t *testing.T, st store.Store, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		task, err := st.Tasks().Get(context.Background(), id)
		if err == nil && task.Status != domain.TaskStatusPending && task.Status != domain.TaskStatusRunning {
			return
		}
	}
	t.Fatalf("任务 %s 没有结束", id)
}

// 提交时就被拒绝的操作（这里：删除一个还有克隆依赖的还原点）不能把镜像一直占着。
func TestRefusedCatalogueOperationReleasesTheImage(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	svc := ReductionService{Store: st, Storage: &fakeReductionStorage{dependents: map[string][]string{"@0": {"tank/nd/other"}}}}
	if _, err := svc.Delete(ctx, "red-1"); err == nil {
		t.Fatal("有依赖的还原点不应能删除")
	}
	if _, err := svc.Delete(ctx, "red-2"); err != nil {
		t.Fatalf("前一个请求被拒绝后镜像应已释放: %v", err)
	}
}

// blockingDelete 让「删除还原点」停在存储那一步。
type blockingDelete struct {
	*fakeReductionStorage
	entered chan struct{}
	release chan struct{}
}

func (s *blockingDelete) DeleteReduction(ctx context.Context, configID, name string) error {
	close(s.entered)
	<-s.release
	return s.fakeReductionStorage.DeleteReduction(ctx, configID, name)
}

// 删除 R 已提交（在用检查已过）时应用 R 必须等待；否则 R 成为当前点后又被删，当前点被悄悄改回，操作员却以为应用成功。
func TestApplyIsRefusedWhileADeleteOfTheImageRuns(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	slow := &blockingDelete{fakeReductionStorage: &fakeReductionStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	deleted, err := (ReductionService{Store: st, Storage: slow, Async: true}).Delete(ctx, "red-2")
	if err != nil {
		t.Fatal(err)
	}
	<-slow.entered

	_, err = (ReductionService{Store: st, Storage: &fakeReductionStorage{}}).SetCurrent(ctx, "red-2")
	close(slow.release)
	waitTaskDone(t, st, deleted.TaskID)
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "删除还原点") {
		t.Fatalf("删除进行中应拒绝应用并说明原因，得到 %v", err)
	}
}

// probingStore 在「池已改完、库未提交」时尝试打复制标记。能拿到就说明这轮会拍到库有池无的配置：
// 备机收到后若写入者恰好宕机，接任前的完整性检查会让所有备机拒绝接任。
type probingStore struct {
	store.Store
	unguarded []string
}

func (s *probingStore) probe(where string) {
	got := make(chan struct{})
	go func() { storage.HoldCatalogue()(); close(got) }()
	select {
	case <-got:
		s.unguarded = append(s.unguarded, where)
	case <-time.After(100 * time.Millisecond):
	}
}

func (s *probingStore) Tx(ctx context.Context, fn func(store.Store) error) error {
	s.probe("tx")
	return s.Store.Tx(ctx, fn)
}

func (s *probingStore) Configs() store.ConfigRepo {
	return probingConfigs{ConfigRepo: s.Store.Configs(), s: s}
}

type probingConfigs struct {
	store.ConfigRepo
	s *probingStore
}

func (c probingConfigs) Delete(ctx context.Context, id string) error {
	c.s.probe("config delete")
	return c.ConfigRepo.Delete(ctx, id)
}

func TestDeletingAConfigHoldsReplicationUntilTheDatabaseAgrees(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	seedSiblingConfig(t, ctx, st, "img-1")
	probing := &probingStore{Store: st}
	if _, err := (ConfigService{Store: probing, Storage: &fakeConfigStorage{}}).Delete(ctx, "cfg-1"); err != nil {
		t.Fatal(err)
	}
	if len(probing.unguarded) > 0 {
		t.Fatalf("删配置的池/库空档里能打复制标记：%v", probing.unguarded)
	}
}

func TestDeletingAnImageHoldsReplicationUntilTheDatabaseAgrees(t *testing.T) {
	ctx := context.Background()
	st := seedReductionStore(t)
	probing := &probingStore{Store: st}
	if err := (ImageService{Store: probing, Storage: &fakeImageStorage{}}).DeleteImage(ctx, "img-1"); err != nil {
		t.Fatal(err)
	}
	if len(probing.unguarded) > 0 {
		t.Fatalf("删镜像的池/库空档里能打复制标记：%v", probing.unguarded)
	}
}

// probingSuperStop 在换盘那一步试探：超管保存从挪开配置到写进还原点行，整段都不许打标记。
type probingSuperStop struct {
	*fakeSuperStopStorage
	probe func(string)
}

func (s probingSuperStop) SuperStop(ctx context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	s.probe("swap")
	return s.fakeSuperStopStorage.SuperStop(ctx, req)
}

func TestSuperSaveHoldsReplicationFromTheSwapToTheRows(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true, State: domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	probing := &probingStore{Store: st}
	fake := &fakeSuperStopStorage{reduction: domain.Reduction{ID: "cfg-1_saved", ConfigID: "cfg-1", Name: "@saved", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}}
	service := TerminalService{Store: probing, Storage: probingSuperStop{fakeSuperStopStorage: fake, probe: probing.probe}}
	if _, err := service.StopSuper(ctx, terminal.ID, SuperStopRequest{ReductionName: "saved"}); err != nil {
		t.Fatal(err)
	}
	if len(probing.unguarded) > 0 {
		t.Fatalf("超管保存途中能打复制标记：%v", probing.unguarded)
	}
}

// probingBlank 在「建卷、分区、格式化」那一步试探能否打复制标记。
type probingBlank struct {
	*fakeImageStorage
	probe func(string)
}

func (s probingBlank) CreateBlankImage(_ context.Context, req storage.BlankImageReq) (storage.ImportImageResult, error) {
	s.probe("format")
	id := req.Name
	cfg := id + "_default"
	return storage.ImportImageResult{
		Image:     domain.Image{ID: id, Name: req.Name, OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeData},
		Config:    domain.Config{ID: cfg, ImageID: id, Name: "default"},
		Reduction: domain.Reduction{ID: cfg + "_0", ConfigID: cfg, Name: "@0", Status: domain.ReductionStatusReady},
	}, nil
}

// 新建空白数据盘：卷已建好、库还没记录的这段时间不许打复制标记，否则复制出去的是库里没有的卷。
func TestCreatingABlankImageHoldsReplicationUntilItIsRecorded(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	probing := &probingStore{Store: st}
	fake := probingBlank{fakeImageStorage: &fakeImageStorage{}, probe: probing.probe}
	svc := ImageService{Store: probing, Storage: fake}
	if _, err := svc.CreateBlankImage(ctx, BlankImageRequest{Name: "e2e-blank", SizeBytes: 1 << 30, Filesystem: "ntfs"}); err != nil {
		t.Fatal(err)
	}
	if len(probing.unguarded) > 0 {
		t.Fatalf("新建空白数据盘途中能打复制标记：%v", probing.unguarded)
	}
}
