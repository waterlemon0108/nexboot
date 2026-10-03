package ops

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

// backupFakeAgent 只模拟备份路径用到的：备份池名、最新标记、送副本的结果。
type backupFakeAgent struct {
	storage.StorageAgent
	mu         sync.Mutex
	backupPool string
	marker     string
	copied     string // 备份池里已有的最新标记
	base       string // 增量基准；空表示没有基准
	fellBack   bool   // 存储层退回了全量
	snapshots  int
	backups    []storage.BackupReq
	dbDir      string
}

func (a *backupFakeAgent) BackupPoolName(context.Context) (string, error) {
	return a.backupPool, nil
}

func (a *backupFakeAgent) LatestBackupMarker(context.Context) (string, error) {
	return a.marker, nil
}

func (a *backupFakeAgent) CopiedBackupMarker(context.Context, string) (string, error) {
	return a.copied, nil
}

func (a *backupFakeAgent) SnapshotCatalogue(_ context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.snapshots++
	a.marker = name
	return nil
}

func (a *backupFakeAgent) Backup(_ context.Context, req storage.BackupReq) (storage.BackupResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.backups = append(a.backups, req)
	a.copied = req.Snapshot
	return storage.BackupResult{Base: a.base, FellBack: a.fellBack}, nil
}

func (a *backupFakeAgent) EnsureDBCopyDir(context.Context) (string, error) {
	if a.dbDir == "" {
		return "", nil
	}
	return a.dbDir, nil
}

// 写入者只打一次标记，随复制下发；它自己的副本和别台一样走 DrainLocal。
func TestSnapshotRoundTakesTheMarkerAndNothingElse(t *testing.T) {
	ctx := context.Background()
	ag := &backupFakeAgent{backupPool: "backup"}
	st := newImageTestStore(t)
	svc := BackupService{Store: st, Storage: ag, RunInline: true,
		Now: func() time.Time { return time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC) }}

	marker, err := svc.SnapshotRound(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if marker == "" {
		t.Fatal("没有打出标记")
	}
	if len(ag.backups) != 0 {
		t.Fatalf("打快照这一步不该顺手推副本：%#v", ag.backups)
	}
	if ag.snapshots != 1 {
		t.Fatalf("快照次数 = %d", ag.snapshots)
	}
}

// 每台节点复用写入者下发的标记，把本机副本送进本机备份池。
func TestDrainLocalShipsTheReplicatedMarker(t *testing.T) {
	ctx := context.Background()
	ag := &backupFakeAgent{backupPool: "backup", marker: "ndbackup-7"}
	st := newImageTestStore(t)
	svc := BackupService{Store: st, Storage: ag, RunInline: true, Now: time.Now}

	done, _, err := svc.DrainLocal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("有新标记却没推")
	}
	if len(ag.backups) != 1 {
		t.Fatalf("backups = %#v", ag.backups)
	}
	if got := ag.backups[0]; got.Snapshot != "ndbackup-7" || !got.Reuse || got.BackupPool != "backup" {
		t.Fatalf("推的参数不对：%#v", got)
	}

	// 同一个标记不重复送。
	ag.copied = "ndbackup-7"
	done, _, err = svc.DrainLocal(ctx)
	if err != nil || done {
		t.Fatalf("已经推过的标记又推了一次：done=%v err=%v", done, err)
	}
}

// 没有备份池不是错误，但也不能报成已备份。
func TestDrainLocalIsQuietWithoutABackupPool(t *testing.T) {
	ctx := context.Background()
	ag := &backupFakeAgent{backupPool: "", marker: "ndbackup-7"}
	svc := BackupService{Store: newImageTestStore(t), Storage: ag, RunInline: true, Now: time.Now}
	done, _, err := svc.DrainLocal(ctx)
	if err != nil {
		t.Fatalf("没有备份池应当安静跳过，而不是报错：%v", err)
	}
	if done || len(ag.backups) != 0 {
		t.Fatal("没有备份池却推了")
	}
}

// 备份状态从本机池读，不靠数据库：库随复制会被覆盖，标记名里已带时间。
func TestLocalStatusReadsThePoolsNotTheDatabase(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	ag := &backupFakeAgent{
		backupPool: "backup",
		marker:     "ndbackup-" + itoa(at.UnixNano()),
		copied:     "ndbackup-" + itoa(at.UnixNano()),
	}
	svc := BackupService{Store: newImageTestStore(t), Storage: ag, RunInline: true, Now: time.Now}

	got, err := svc.LocalStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupPool != "backup" {
		t.Fatalf("池 = %q", got.BackupPool)
	}
	if got.LastBackupAt == nil || !got.LastBackupAt.Equal(at) {
		t.Fatalf("上次备份时刻 = %v，想要 %v", got.LastBackupAt, at)
	}
	if got.Pending {
		t.Fatal("已经推完了却说还欠着")
	}

	// 写入者又打了一轮，本机还没送：Pending。
	ag.marker = "ndbackup-" + itoa(at.Add(time.Hour).UnixNano())
	got, err = svc.LocalStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pending {
		t.Fatal("有新标记没推，却说不欠")
	}
	if got.LastBackupAt == nil || !got.LastBackupAt.Equal(at) {
		t.Fatalf("欠着的时候，上次备份仍应是真的推过的那次：%v", got.LastBackupAt)
	}
}

// 没有备份池的节点要如实说「没有备份池」，不能画成正常。
func TestLocalStatusSaysWhenThereIsNowhereToBackUp(t *testing.T) {
	svc := BackupService{Store: newImageTestStore(t), Storage: &backupFakeAgent{}, Now: time.Now}
	got, err := svc.LocalStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupPool != "" || got.LastBackupAt != nil || got.Pending {
		t.Fatalf("status = %#v", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// 服务层计算下次运行要用本机时区（scheduleNow），不能用 .UTC() 的 now()。
func TestStatusReportsNextRunOnTheLocalWallClock(t *testing.T) {
	ctx := context.Background()
	zone := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 8, 23, 22, 41, 0, 0, zone)
	svc := BackupService{
		Store: newImageTestStore(t), Storage: &backupFakeAgent{}, RunInline: true,
		Now: func() time.Time { return now },
	}
	if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "daily@03:00"}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Config.NextRunAt == nil {
		t.Fatal("没有算出下一次")
	}
	got := st.Config.NextRunAt.In(zone)
	if got.Hour() != 3 || got.Minute() != 0 {
		t.Fatalf("下一次在本地 %02d:%02d，而计划写的是 03:00", got.Hour(), got.Minute())
	}
}

// NextRunText 由服务端按本机钟点格式化，不交给浏览器按其时区显示。
func TestStatusFormatsNextRunInTheServersOwnClock(t *testing.T) {
	ctx := context.Background()
	zone := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 8, 23, 22, 41, 0, 0, zone)
	svc := BackupService{
		Store: newImageTestStore(t), Storage: &backupFakeAgent{}, RunInline: true,
		Now: func() time.Time { return now },
	}
	if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "daily@03:00"}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Config.NextRunText != "2026-08-24 03:00" {
		t.Fatalf("下次 = %q，想要按服务器的钟排好的 2026-08-24 03:00", st.Config.NextRunText)
	}
}

// 任务结果要写明增量还是全量：反复全量说明增量链在断，需要排查。
func TestDrainLocalReportsWhetherItWasFullOrIncremental(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)

	// 有基准：增量。
	ag := &backupFakeAgent{backupPool: "backup", marker: "ndbackup-7", base: "ndbackup-6"}
	svc := BackupService{Store: st, Storage: ag, RunInline: true, Now: time.Now}
	done, kind, err := svc.DrainLocal(ctx)
	if err != nil || !done {
		t.Fatalf("有新标记就该推：done=%v err=%v", done, err)
	}
	if !strings.Contains(kind, "增量") {
		t.Fatalf("有基准时要报增量，实际 %q", kind)
	}

	// 没有基准或退回全量：全量。
	ag2 := &backupFakeAgent{backupPool: "backup", marker: "ndbackup-8", fellBack: true}
	svc2 := BackupService{Store: st, Storage: ag2, RunInline: true, Now: time.Now}
	done, kind, err = svc2.DrainLocal(ctx)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !strings.Contains(kind, "全量") {
		t.Fatalf("退回全量时要报全量，实际 %q", kind)
	}
}

// slowBackupAgent 停在送副本那一步，直到放行。
type slowBackupAgent struct {
	*backupFakeAgent
	entered chan struct{}
	release chan struct{}
}

func (a *slowBackupAgent) Backup(ctx context.Context, req storage.BackupReq) (storage.BackupResult, error) {
	a.entered <- struct{}{}
	<-a.release
	return a.backupFakeAgent.Backup(ctx, req)
}

// 备份任务和后台循环同时送副本时，同一时刻只送一份。
func TestDrainLocalShipsOneCopyAtATime(t *testing.T) {
	agent := &slowBackupAgent{
		backupFakeAgent: &backupFakeAgent{backupPool: "backup", marker: "ndbackup-2", copied: "ndbackup-1"},
		entered:         make(chan struct{}, 2), release: make(chan struct{}),
	}
	svc := BackupService{Storage: agent}
	first := make(chan error, 1)
	go func() { _, _, err := svc.DrainLocal(context.Background()); first <- err }()
	<-agent.entered

	shipped, _, err := svc.DrainLocal(context.Background())
	if err != nil || shipped {
		t.Fatalf("另一份正在送时应跳过: shipped=%v err=%v", shipped, err)
	}
	close(agent.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if len(agent.backups) != 1 {
		t.Fatalf("只该送一份: %v", agent.backups)
	}
}

// 后台循环正在送时，任务结果要说正在送，不能说成没有备份池。
func TestBackupTaskSaysACopyIsAlreadyOnItsWay(t *testing.T) {
	st := newImageTestStore(t)
	agent := &slowBackupAgent{
		backupFakeAgent: &backupFakeAgent{backupPool: "backup", marker: "ndbackup-1", copied: ""},
		entered:         make(chan struct{}, 2), release: make(chan struct{}),
	}
	svc := BackupService{Store: st, Storage: agent, RunInline: true}
	go func() { _, _, _ = svc.DrainLocal(context.Background()) }()
	<-agent.entered
	defer close(agent.release)

	res, err := svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	task, _ := st.Tasks().Get(context.Background(), res.TaskID)
	if strings.Contains(task.Result, "没有备份池") || !strings.Contains(task.Result, "正在送") {
		t.Fatalf("result = %q", task.Result)
	}
}

// 正在送副本时要说「备份中」，不能和「还没开始」一样显示待备份。
func TestLocalStatusSaysCopyingWhileACopyIsOnItsWay(t *testing.T) {
	agent := &slowBackupAgent{
		backupFakeAgent: &backupFakeAgent{backupPool: "backup", marker: "ndbackup-2", copied: "ndbackup-1"},
		entered:         make(chan struct{}, 1), release: make(chan struct{}),
	}
	svc := BackupService{Storage: agent}
	done := make(chan error, 1)
	go func() { _, _, err := svc.DrainLocal(context.Background()); done <- err }()
	<-agent.entered

	st, err := svc.LocalStatus(context.Background())
	if err != nil || !st.Copying || !st.Pending {
		t.Fatalf("while copying: %+v err=%v", st, err)
	}
	close(agent.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err = svc.LocalStatus(context.Background())
	if err != nil || st.Copying || st.Pending {
		t.Fatalf("after copying: %+v err=%v", st, err)
	}
}

// 备份标记与复制轮次一样，要等进行中的目录变更结束：库副本和快照落在变更中间，
// 恢复出来的是池与库对不上的目录。
func TestSnapshotRoundWaitsForAnInFlightCatalogueChange(t *testing.T) {
	ctx := context.Background()
	ag := &backupFakeAgent{backupPool: "backup", dbDir: t.TempDir()}
	svc := BackupService{Store: newImageTestStore(t), Storage: ag, RunInline: true, Now: time.Now}

	done := storage.ChangeCatalogue()
	finished := make(chan error, 1)
	go func() {
		_, err := svc.SnapshotRound(ctx)
		finished <- err
	}()
	select {
	case err := <-finished:
		done()
		t.Fatalf("目录变更进行中就打了备份标记：err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	done()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("变更结束后备份标记仍没打出来")
	}
	if ag.snapshots != 1 {
		t.Fatalf("快照次数 = %d", ag.snapshots)
	}
}
