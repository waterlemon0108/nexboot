package ops

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

type fakeInventoryStorage struct {
	storage.StorageAgent
	inv    storage.PoolInventory
	err    error
	osType domain.OSType
	// onInventory 在读池时改库，模拟检查读池与读库之间导入刚好写完。
	onInventory func(context.Context)
}

func (s *fakeInventoryStorage) DetectOSType(context.Context, string) (domain.OSType, bool) {
	return s.osType, s.osType != ""
}

func (s *fakeInventoryStorage) Inventory(ctx context.Context) (storage.PoolInventory, error) {
	if s.onInventory != nil {
		s.onInventory(ctx)
	}
	return s.inv, s.err
}

func seedReconcileStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "img", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "img_default", ImageID: "img", Name: "default", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, red := range []domain.Reduction{
		{ID: "img_0", ConfigID: "img_default", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady},
		{ID: "img_default_r1", ConfigID: "img_default", Name: "@r1", CreatedAt: now, Status: domain.ReductionStatusReady},
	} {
		if err := st.Reductions().Create(ctx, red); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// 一致时不报任何差异。
func TestReconcileReportsNothingWhenStoreAndPoolAgree(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default", "imports"},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1"},
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || len(report.Issues) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

// 库里有而池里没有的行要报出来：快照已丢的还原点在界面上仍显示可开机。
func TestReconcileFindsRowsThePoolCannotBack(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default"},
		Snapshots: []string{"img@0", "img_default@0"}, // @r1 vanished
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Issues) != 1 {
		t.Fatalf("issues = %#v", report.Issues)
	}
	issue := report.Issues[0]
	if issue.Kind != IssueMissingSnapshot || issue.Ref != "img_default_r1" {
		t.Fatalf("issue = %#v", issue)
	}
	if !strings.Contains(issue.Detail, "@r1") {
		t.Fatalf("detail should name the snapshot: %q", issue.Detail)
	}
}

func TestReconcileFindsConfigsAndImagesWithoutDatasets(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"imports"}, // both the image and its config are gone
		Snapshots: nil,
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, i := range report.Issues {
		kinds[i.Kind]++
	}
	if kinds[IssueMissingDataset] != 2 { // the image and the config
		t.Fatalf("issues = %#v", report.Issues)
	}
}

// 无主数据集要报（占空间、挡操作），但客户机克隆归离线回收管，不报。
func TestReconcileFindsOrphanDatasetsButIgnoresClientClones(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets: []string{
			"img", "img_default", "imports",
			"CLIENT-AABBCCDDEEFF",      // a running machine
			"SCLIENT-AABBCCDDEEFF",     // a super machine
			"INSPECT-IMG-1785",         // a health check in flight
			"img_default_before_super", // left by an interrupted save
			"leftover",                 // nobody's
		},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1"},
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var orphans []string
	for _, i := range report.Issues {
		if i.Kind == IssueOrphanDataset {
			orphans = append(orphans, i.Ref)
		}
	}
	want := map[string]bool{"img_default_before_super": true, "leftover": true}
	if len(orphans) != len(want) {
		t.Fatalf("orphans = %#v, want %#v", orphans, want)
	}
	for _, o := range orphans {
		if !want[o] {
			t.Fatalf("unexpected orphan %q (client clones and health checks must be ignored)", o)
		}
	}
}

// 读不到池时返回错误，不能报成所有记录都坏了。
func TestReconcileFailsLoudlyWhenThePoolCannotBeRead(t *testing.T) {
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{err: context.DeadlineExceeded}}
	if _, err := svc.Check(context.Background()); err == nil {
		t.Fatal("expected the error to surface")
	}
}

// 无主快照要报：占空间，还可能挡住删除或与合并的 promote 冲突（导入流会带进
// 「@--head--」这类快照）。
func TestReconcileFindsSnapshotsNoRowClaims(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets: []string{"img", "img_default", "imports", "CLIENT-AABBCCDDEEFF"},
		Snapshots: []string{
			"img@0",                           // the image's baseline — claimed by the image
			"img_default@0", "img_default@r1", // claimed by reduction rows
			"img@--head--",             // rode in on the import stream
			"CLIENT-AABBCCDDEEFF@boot", // the reclaimer's business, not this check's
		},
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var orphans []string
	for _, i := range report.Issues {
		if i.Kind == IssueOrphanSnapshot {
			orphans = append(orphans, i.Ref)
		}
	}
	if len(orphans) != 1 || orphans[0] != "img@--head--" {
		t.Fatalf("orphan snapshots = %#v, want just img@--head--", orphans)
	}
}

// 无主数据集上的快照不重复报。
func TestReconcileReportsAnOrphanDatasetOnceNotAlsoItsSnapshots(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default", "imports", "leftover"},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1", "leftover@0", "leftover@1"},
	}}}

	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 1 || report.Issues[0].Kind != IssueOrphanDataset || report.Issues[0].Ref != "leftover" {
		t.Fatalf("issues = %#v", report.Issues)
	}
}

// 丢库后可从池按数据集、快照和血缘重建镜像、配置和还原点。恢复只增不删：没导入
// 的池与数据被删的池看起来一样，删行会丢掉即将回来的数据的目录。
func TestRecoverCatalogueRebuildsWhatThePoolStillHolds(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakeInventoryStorage{
		inv: storage.PoolInventory{
			Datasets: []string{"win11", "win11_default", "win11_work_1785", "imports", "CLIENT-AABBCCDDEEFF"},
			Snapshots: []string{
				"win11@0",
				"win11_default@0", "win11_default@office",
				"win11_work_1785@0",
			},
			Origins: map[string]string{
				"win11_default":       "win11@0",
				"win11_work_1785":     "win11_default@0", // a config forked from a config
				"CLIENT-AABBCCDDEEFF": "win11_default@office",
			},
		},
		osType: domain.OSTypeWindows,
	}
	svc := ReconcileService{Store: st, Storage: agent}

	report, err := svc.RecoverCatalogue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Images != 1 || report.Configs != 2 || report.Reductions != 3 {
		t.Fatalf("recovered %#v", report)
	}

	images, err := st.Images().List(ctx)
	if err != nil || len(images) != 1 || images[0].ID != "win11" {
		t.Fatalf("images = %#v err=%v", images, err)
	}
	// 系统类型从盘本身判断，猜错会让该镜像所有客户机拿到错误的启动脚本。
	if images[0].OSType != domain.OSTypeWindows {
		t.Fatalf("os type = %q", images[0].OSType)
	}
	configs, err := st.Configs().ListByImage(ctx, "win11")
	if err != nil || len(configs) != 2 {
		t.Fatalf("configs = %#v err=%v", configs, err)
	}
	// fork 出的配置与来源配置属于同一镜像。
	for _, cfg := range configs {
		if cfg.ImageID != "win11" {
			t.Fatalf("config %s belongs to %q", cfg.ID, cfg.ImageID)
		}
	}
	reds, err := st.Reductions().ListByConfig(ctx, "win11_default")
	if err != nil || len(reds) != 2 {
		t.Fatalf("reductions = %#v err=%v", reds, err)
	}
	var names []string
	for _, r := range reds {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"@0", "@office"}) {
		t.Fatalf("reduction names = %v", names)
	}
	// 每个配置都要有可开机的默认还原点，否则分组无法引用。
	cfg, err := st.Configs().Get(ctx, "win11_default")
	if err != nil || cfg.DefaultReductionID == nil {
		t.Fatalf("config = %#v err=%v", cfg, err)
	}
}

// 每次启动都会跑，再跑一次不能有任何变化。
func TestRecoverCatalogueIsIdempotentAndNeverTouchesExistingRows(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t) // already has img / img_default / @0 / @r1
	agent := &fakeInventoryStorage{
		inv: storage.PoolInventory{
			Datasets:  []string{"img", "img_default"},
			Snapshots: []string{"img@0", "img_default@0", "img_default@r1"},
			Origins:   map[string]string{"img_default": "img@0"},
		},
		osType: domain.OSTypeLinux, // would be wrong for the existing row
	}
	svc := ReconcileService{Store: st, Storage: agent}

	for i := 0; i < 3; i++ {
		report, err := svc.RecoverCatalogue(ctx)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if report.Images != 0 || report.Configs != 0 || report.Reductions != 0 {
			t.Fatalf("run %d recovered %#v, want nothing", i, report)
		}
	}
	img, err := st.Images().Get(ctx, "img")
	if err != nil {
		t.Fatal(err)
	}
	// 已有行保留自己的名字和系统类型：恢复只增不改。
	if img.Name != "win" || img.OSType != domain.OSTypeWindows {
		t.Fatalf("existing image was rewritten: %#v", img)
	}
}

// 数据集不在的行保留不删：手工删了数据集或池还没导入，都不能让操作者丢掉目录。
func TestRecoverCatalogueLeavesRowsWithoutDatasetsAlone(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{
		inv: storage.PoolInventory{Datasets: []string{"imports"}}, // the pool says nothing
	}}

	if _, err := svc.RecoverCatalogue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Images().Get(ctx, "img"); err != nil {
		t.Fatalf("the image row was removed: %v", err)
	}
	if configs, err := st.Configs().ListByImage(ctx, "img"); err != nil || len(configs) != 1 {
		t.Fatalf("configs = %#v err=%v", configs, err)
	}
}

// 接任前的完整性检查只看「库里有、池里没有」的镜像和配置，并用名字说出来。
func TestMissingDatasetsNamesImagesAndConfigs(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets: []string{"img", "orphan"},
	}}}
	missing, err := svc.MissingDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"配置「default」"}) {
		t.Fatalf("missing = %v", missing)
	}
}

// 导入进行中池上已有数据集而库里还没登记，是正常中间状态，不报残留。
func TestReconcileSkipsAnImageBeingImported(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	if err := st.Tasks().Create(ctx, domain.Task{ID: "t-imp", Type: domain.TaskTypeImportImage, TargetRef: "ubuntu-0",
		Status: domain.TaskStatusRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default", "ubuntu-0", "ubuntu-0_default"},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1", "ubuntu-0@0"},
	}}}
	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("导入中的镜像不该报残留：%#v", report.Issues)
	}
}

// 数据集总是先于库行建出，先读库再读池，导入恰好写完时才不会误报「数据集不存在」。
func TestReconcileReadsTheStoreBeforeThePool(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{
		inv: storage.PoolInventory{
			Datasets:  []string{"img", "img_default"},
			Snapshots: []string{"img@0", "img_default@0", "img_default@r1"},
		},
		onInventory: func(ctx context.Context) {
			_ = st.Configs().Create(ctx, domain.Config{ID: "img_work", ImageID: "img", Name: "work", CreatedAt: time.Now().UTC()})
		},
	}}
	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("读池之后才写上的配置不该报不一致：%#v", report.Issues)
	}
}

// 接任时池里可能有打 @0 之前就复制过来的半导入盘，不能收成镜像（没有配置、开不了机）。
func TestRecoverCatalogueSkipsAnImageStillBeingImported(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	agent := &fakeInventoryStorage{
		inv: storage.PoolInventory{
			Datasets:  []string{"win11", "win11_default", "ubuntu-lvm"},
			Snapshots: []string{"win11@0", "win11_default@0", "ubuntu-lvm@rep-1790968321"},
			Origins:   map[string]string{"win11_default": "win11@0"},
		},
		osType: domain.OSTypeLinux,
	}
	report, err := ReconcileService{Store: st, Storage: agent}.RecoverCatalogue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Images != 1 {
		t.Fatalf("recovered %#v", report)
	}
	if _, err := st.Images().Get(ctx, "ubuntu-lvm"); err == nil {
		t.Fatal("a half-imported disk was adopted as an image")
	}
}

// 接任检查读的是库副本，里面写入者的任务永远「执行中」；不能据此跳过，否则会
// 放缺数据的备机去接任。
func TestMissingDatasetsIgnoresTasksInTheDatabaseCopy(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	if err := st.Tasks().Create(ctx, domain.Task{ID: "t-imp", Type: domain.TaskTypeImportImage, TargetRef: "img",
		Status: domain.TaskStatusRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{}}}
	missing, err := svc.MissingDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Fatal("a standby missing the image passed the takeover check")
	}
}

// 检查要等目录锁：删除、合并、超管保存在锁内先改池后改库，中途读会误报。
func TestReconcileWaitsForCatalogueChangesInProgress(t *testing.T) {
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default"},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1"},
	}}}
	done := storage.ChangeCatalogue()
	finished := make(chan struct{})
	go func() {
		_, _ = svc.Check(context.Background())
		close(finished)
	}()
	select {
	case <-finished:
		done()
		t.Fatal("check read the catalogue while a change was half done")
	case <-time.After(150 * time.Millisecond):
	}
	done()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("check never ran after the change finished")
	}
}

// 只跳过导入、另存为镜像任务自己的 id 和 id_default，名字相近的镜像和其它任务目标照常检查。
func TestReconcileSkipsOnlyWhatAnImportLaysDown(t *testing.T) {
	ctx := context.Background()
	st := seedReconcileStore(t)
	for _, task := range []domain.Task{
		{ID: "t-imp", Type: domain.TaskTypeImportImage, TargetRef: "win10"},
		{ID: "t-pool", Type: domain.TaskTypeCreatePool, TargetRef: "data"},
	} {
		task.Status, task.CreatedAt = domain.TaskStatusRunning, time.Now().UTC()
		if err := st.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{
		Datasets:  []string{"img", "img_default", "win10", "win10_default", "win10_old", "data"},
		Snapshots: []string{"img@0", "img_default@0", "img_default@r1", "win10@0", "win10_default@0"},
	}}}
	report, err := svc.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, issue := range report.Issues {
		refs = append(refs, issue.Ref)
	}
	sort.Strings(refs)
	if strings.Join(refs, ",") != "data,win10_old" {
		t.Fatalf("issues = %v", refs)
	}
}

// 目录锁一直被占时在期限内放弃，且等待中的检查不能挡住后续目录变更。
func TestReconcileGivesUpWhenTheCatalogueStaysBusy(t *testing.T) {
	st := seedReconcileStore(t)
	svc := ReconcileService{Store: st, Storage: &fakeInventoryStorage{inv: storage.PoolInventory{}}}
	done := storage.ChangeCatalogue()
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := svc.Check(ctx); err == nil {
		t.Fatal("check ran against a half-done change")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("waited %s past its deadline", took)
	}
	// 等待中的检查不能挡住下一次变更。
	other := make(chan struct{})
	go func() { storage.ChangeCatalogue()(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("a waiting check blocked a new catalogue change")
	}
}
