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

type fakePublishStorage struct {
	storage.StorageAgent
	req     storage.PublishDataDiskReq
	called  bool
	written []int64 // 每次问「写了多少」依次返回这些值
	result  storage.PublishDataDiskResult
	err     error
	noDisk  map[int]bool // 这些 LUN 在池里没有超管盘
}

func (f *fakePublishStorage) PublishDataDisk(_ context.Context, req storage.PublishDataDiskReq) (storage.PublishDataDiskResult, error) {
	f.req, f.called = req, true
	if f.result.Reduction.ID == "" {
		f.result.Reduction = domain.Reduction{ID: storage.ReductionID(req.ConfigID, strings.TrimPrefix(req.ReductionName, "@")),
			ConfigID: req.ConfigID, Name: req.ReductionName, Status: domain.ReductionStatusReady}
	}
	return f.result, f.err
}

func (f *fakePublishStorage) SuperDiskWritten(_ context.Context, _ string, lun int) (int64, error) {
	if f.noDisk[lun] {
		// 真 zfs 在这台机器没有这块超管盘时就是这么答的
		return 0, errors.New("dataset does not exist")
	}
	if len(f.written) == 0 {
		return 0, nil
	}
	v := f.written[0]
	if len(f.written) > 1 {
		f.written = f.written[1:]
	}
	return v, nil
}

// 一台超管机 + 一块数据盘（D:），机器开着。
func seedPublishStore(t *testing.T) (store.Store, domain.Group, domain.Terminal) {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Now().UTC()
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-games", ImageID: "img-1", Name: "游戏盘", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-games-0", ConfigID: "cfg-games", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd-1", GroupID: group.ID, ConfigID: "cfg-games", ImageID: "img-1", MountTarget: "D:"}); err != nil {
		t.Fatal(err)
	}
	terminal := domain.Terminal{ID: "t-1", Name: "教师机", MAC: "001122334455", IP: "192.168.1.10", GroupID: group.ID,
		IsSuper: true, State: domain.TerminalStateOnline}
	if err := st.Terminals().Create(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	return st, group, terminal
}

// 不关机发布数据盘：存成还原点并立即成为配置当前点，客户机下次开机生效。
func TestPublishDataDiskSavesAndMakesItCurrent(t *testing.T) {
	ctx := context.Background()
	st, _, terminal := seedPublishStore(t)
	pool := &fakePublishStorage{}
	svc := TerminalService{Store: st, Storage: pool, QuietWindow: time.Millisecond}

	res, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-1", Name: "装了永劫无间"})
	if err != nil {
		t.Fatal(err)
	}
	if !pool.called || pool.req.MAC != terminal.MAC || pool.req.LUN != 1 || pool.req.ConfigID != "cfg-games" {
		t.Fatalf("交给存储层的请求 = %#v", pool.req)
	}
	if pool.req.ReductionName != "@装了永劫无间" && !strings.HasPrefix(pool.req.ReductionName, "@") {
		t.Fatalf("还原点名 = %q", pool.req.ReductionName)
	}
	if res.Reduction == nil {
		t.Fatalf("结果里没有还原点：%#v", res)
	}
	// 还原点落库，并成为这个配置的当前点
	if _, err := st.Reductions().Get(ctx, res.Reduction.ID); err != nil {
		t.Fatalf("还原点没落库：%v", err)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-games")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != res.Reduction.ID {
		t.Fatalf("发布点没成为当前点：%#v", cfg.DefaultReductionID)
	}
	// 发布不改超管状态：装完这个还能接着装下一个
	after, _ := st.Terminals().Get(ctx, terminal.ID)
	if !after.IsSuper {
		t.Fatal("发布之后这台还得是超管机")
	}
}

// 盘仍在写入时拒绝发布，否则整个分组拿到半成品；写入量由服务端采样判断。
func TestPublishDataDiskRefusesWhileTheDiskIsStillBeingWritten(t *testing.T) {
	ctx := context.Background()
	st, _, terminal := seedPublishStore(t)
	pool := &fakePublishStorage{written: []int64{100, 4096}} // 两次采样之间还在涨
	svc := TerminalService{Store: st, Storage: pool, QuietWindow: time.Millisecond}

	_, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-1", Name: "装了永劫无间"})
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "写入") {
		t.Fatalf("应当拒绝并说清楚是盘在写：%v", err)
	}
	if pool.called {
		t.Fatal("盘还在写就不该动它")
	}
}

// 发布成功但盘没装回：不能报失败，只在 Detail 里提示重启客户机。
func TestPublishDataDiskReportsAPublishedDiskThatDidNotComeBack(t *testing.T) {
	ctx := context.Background()
	st, _, terminal := seedPublishStore(t)
	pool := &fakePublishStorage{err: storage.DiskNotReattached{LUN: 1, Err: errors.New("export failed")}}
	svc := TerminalService{Store: st, Storage: pool, QuietWindow: time.Millisecond}

	res, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-1", Name: "装了永劫无间"})
	if err != nil {
		t.Fatalf("发布已经成功，不该报错：%v", err)
	}
	if res.Reduction == nil {
		t.Fatalf("结果里没有还原点：%#v", res)
	}
	if !strings.Contains(res.Detail, "重启") {
		t.Fatalf("要告诉运维该做什么：%q", res.Detail)
	}
	if _, err := st.Reductions().Get(ctx, res.Reduction.ID); err != nil {
		t.Fatalf("发布成功就该落库：%v", err)
	}
}

func TestPublishDataDiskChecksTheMachineAndTheDisk(t *testing.T) {
	ctx := context.Background()
	st, _, terminal := seedPublishStore(t)
	pool := &fakePublishStorage{}
	svc := TerminalService{Store: st, Storage: pool, QuietWindow: time.Millisecond}

	// 不是超管机
	normal := terminal
	normal.IsSuper = false
	if err := st.Terminals().Update(ctx, normal); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-1", Name: "x"}); !errors.Is(err, ErrTerminalSuperRequired) {
		t.Fatalf("只有超管机能发布：%v", err)
	}
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	// 这块盘不是它分组里的
	if _, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-nope", Name: "x"}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("盘不属于这个分组要拒绝：%v", err)
	}

	// 名字和已有还原点重复
	if _, err := svc.PublishDataDisk(ctx, terminal.ID, PublishDataDiskRequest{DiskID: "gd-1", Name: "0"}); err == nil {
		t.Fatal("重名要拒绝")
	}
	if pool.called {
		t.Fatal("校验没过就不该动存储")
	}
}

// 数据盘列表给出每块盘的当前点、未发布改动量与能否发布；
// 未开过机的盘应标为不可发布，而不是报错或显示 0。
func TestSuperDisksTellsWhatEachDiskIsPublishingAndWhatIsPending(t *testing.T) {
	ctx := context.Background()
	st, group, terminal := seedPublishStore(t)
	now := time.Now().UTC()
	// 第二块盘：还没开过机，池里没有它的超管盘
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-doc", ImageID: "img-1", Name: "资料盘", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd-2", GroupID: group.ID, ConfigID: "cfg-doc", ImageID: "img-1", MountTarget: "E:"}); err != nil {
		t.Fatal(err)
	}
	// 游戏盘当前发布的点，取的是配置的当前应用点，显示运维起的名字
	cfg, err := st.Configs().Get(ctx, "cfg-games")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-games-1", ConfigID: "cfg-games", Name: "@9yue14", DisplayName: "9月14日更新", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	current := "red-games-1"
	cfg.DefaultReductionID = &current
	if err := st.Configs().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	pool := &fakePublishStorage{written: []int64{13 << 30}, noDisk: map[int]bool{2: true}}
	res, err := (TerminalService{Store: st, Storage: pool}).SuperDisks(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Fatalf("两块数据盘都要列出来：%#v", res)
	}
	games := res.Items[0]
	if games.MountTarget != "D:" || games.LUN != 1 || games.ConfigName != "游戏盘" {
		t.Fatalf("游戏盘那行 = %#v", games)
	}
	if games.CurrentName != "9月14日更新" || games.Written != 13<<30 || !games.Ready {
		t.Fatalf("要显示当前发布点和未发布改动：%#v", games)
	}
	doc := res.Items[1]
	if doc.MountTarget != "E:" || doc.Ready || doc.Written != 0 {
		t.Fatalf("没开过机的盘应当是「还不能发布」：%#v", doc)
	}

	// 系统盘不在这张表里：它只能关机保存
	for _, item := range res.Items {
		if item.LUN == 0 {
			t.Fatalf("系统盘不该出现在数据盘表里：%#v", item)
		}
	}

	// 不是超管机就没有这张表
	normal := terminal
	normal.IsSuper = false
	if err := st.Terminals().Update(ctx, normal); err != nil {
		t.Fatal(err)
	}
	if _, err := (TerminalService{Store: st, Storage: pool}).SuperDisks(ctx, terminal.ID); !errors.Is(err, ErrTerminalSuperRequired) {
		t.Fatalf("普通终端没有超管盘可列：%v", err)
	}
}
