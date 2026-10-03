package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestBackupServiceRunShipsWholeCatalogueWithDBCopy(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedBackupSource(t, st, "img-1", "cfg-1")
	backupStorage := &fakeBackupStorage{dbCopyDir: t.TempDir()}
	service := BackupService{Store: st, Storage: backupStorage, RunInline: true}

	if _, err := service.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "24h"}); err != nil {
		t.Fatal(err)
	}
	first, err := service.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.Tasks().Get(ctx, first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskStatusSuccess || task.Progress != 100 {
		t.Fatalf("task = %#v", task)
	}
	// 一轮只送一条整个目录的流，不是每个数据集一次。
	if len(backupStorage.calls) != 1 {
		t.Fatalf("calls = %#v", backupStorage.calls)
	}
	call := backupStorage.calls[0]
	if call.BackupPool != "backup" || !strings.HasPrefix(call.Snapshot, "ndbackup-") {
		t.Fatalf("call = %#v", call)
	}
	// 快照前先写 DB 副本，流里的行与数据集属于同一时刻。
	if _, err := os.Stat(filepath.Join(backupStorage.dbCopyDir, "ndiskless.db")); err != nil {
		t.Fatalf("db copy missing: %v", err)
	}

	second, err := service.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.TaskID == first.TaskID {
		t.Fatalf("duplicate task id %q", second.TaskID)
	}
	if len(backupStorage.calls) != 2 {
		t.Fatalf("calls = %#v", backupStorage.calls)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Node.BackupPool != "backup" || status.Node.LastBackupAt == nil {
		t.Fatalf("status = %#v", status.Node)
	}
	// 送的是写入者刚打的标记（复用），不是另打一个。
	last := backupStorage.calls[len(backupStorage.calls)-1]
	if !last.Reuse || last.Snapshot == "" {
		t.Fatalf("最后一次推的参数不对：%#v", last)
	}
}

func TestBackupServiceMarksTaskFailed(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedBackupSource(t, st, "img-1", "cfg-1")
	service := BackupService{Store: st, Storage: &fakeBackupStorage{err: errors.New("zfs failed")}, RunInline: true}

	if _, err := service.SaveConfig(ctx, BackupConfigRequest{}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.Tasks().Get(ctx, result.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskStatusFailed || task.Error == "" {
		t.Fatalf("task = %#v", task)
	}
}

// 计划按钟点触发：上午开启的夜间备份不应当场跑，要等到计划时刻。
func TestBackupServiceRunDue(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedBackupSource(t, st, "img-1", "cfg-1")
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	service := BackupService{Store: st, Storage: &fakeBackupStorage{dbCopyDir: t.TempDir()},
		RunInline: true, Now: func() time.Time { return now }}

	if _, err := service.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "daily@03:00"}); err != nil {
		t.Fatal(err)
	}
	// 刚开启，还没到点
	ran, err := service.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("开启的当下就跑了一轮，而计划写的是凌晨三点")
	}

	// 到了次日凌晨三点
	now = time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	ran, err = service.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("到点了却没跑")
	}

	// 同一个点不跑第二次
	ran, err = service.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("同一个计划点跑了两轮")
	}
}

func seedBackupSource(t *testing.T, st *store.SQLStore, imageID, configID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.Images().Create(context.Background(), domain.Image{ID: imageID, Name: imageID, OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(context.Background(), domain.Config{ID: configID, ImageID: imageID, Name: "default", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

type fakeBackupStorage struct {
	storage.StorageAgent
	mu           sync.Mutex // 调度器在自己的 goroutine 里调用
	calls        []storage.BackupReq
	err          error
	dbCopyDir    string
	marker       string
	copied       string
	noBackupPool bool
}

func (s *fakeBackupStorage) Backup(_ context.Context, req storage.BackupReq) (storage.BackupResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err == nil {
		s.copied = req.Snapshot
	}
	return storage.BackupResult{}, s.err
}

func (s *fakeBackupStorage) BackupPoolName(context.Context) (string, error) {
	if s.noBackupPool {
		return "", nil
	}
	return "backup", nil
}

func (s *fakeBackupStorage) LatestBackupMarker(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marker, nil
}

func (s *fakeBackupStorage) CopiedBackupMarker(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copied, nil
}

func (s *fakeBackupStorage) SnapshotCatalogue(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.marker = name
	return nil
}

func (s *fakeBackupStorage) EnsureDBCopyDir(context.Context) (string, error) {
	if s.dbCopyDir == "" {
		return "", errors.New("no db copy dir configured in this fake")
	}
	return s.dbCopyDir, nil
}

func (s *fakeBackupStorage) snapshot() []storage.BackupReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storage.BackupReq(nil), s.calls...)
}

// Trigger 由其它操作（如超管机保存）顺带调用，不能把调用方的成功变成失败：
// 备份未开启、已有备份在跑都应静默返回。
func TestBackupTriggerIsASideErrandNotAGate(t *testing.T) {
	ctx := context.Background()

	t.Run("未开启时什么也不做", func(t *testing.T) {
		st := newImageTestStore(t)
		seedBackupSource(t, st, "img-1", "cfg-1")
		agent := &fakeBackupStorage{}
		svc := BackupService{Store: st, Storage: agent, RunInline: true}
		if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: false, Schedule: "24h"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Trigger(ctx); err != nil {
			t.Fatalf("trigger = %v, want a silent no-op", err)
		}
		if len(agent.snapshot()) != 0 {
			t.Fatalf("it ran anyway: %#v", agent.calls)
		}
	})

	// 没有备份池也要打标记：标记在数据池上供全集群使用，拦下会让一台没加盘
	// 的机器使整个集群不再产生备份。
	t.Run("本机没有备份池，标记照打", func(t *testing.T) {
		st := newImageTestStore(t)
		seedBackupSource(t, st, "img-1", "cfg-1")
		agent := &fakeBackupStorage{dbCopyDir: t.TempDir(), noBackupPool: true}
		svc := BackupService{Store: st, Storage: agent, RunInline: true}
		if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "daily@03:00"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Trigger(ctx); err != nil {
			t.Fatalf("trigger = %v", err)
		}
		if agent.marker == "" {
			t.Fatal("标记没打")
		}
		if len(agent.snapshot()) != 0 {
			t.Fatalf("没有备份池却推了副本：%#v", agent.calls)
		}
	})

	t.Run("开启后真的跑一轮", func(t *testing.T) {
		st := newImageTestStore(t)
		seedBackupSource(t, st, "img-1", "cfg-1")
		agent := &fakeBackupStorage{dbCopyDir: t.TempDir()}
		svc := BackupService{Store: st, Storage: agent, RunInline: true}
		if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "24h"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Trigger(ctx); err != nil {
			t.Fatal(err)
		}
		if len(agent.snapshot()) == 0 {
			t.Fatal("nothing was backed up")
		}
	})

	// 两次保存挨得近时已有备份在跑是常态，第二次触发应静默。
	t.Run("已有备份在跑时不报错", func(t *testing.T) {
		st := newImageTestStore(t)
		seedBackupSource(t, st, "img-1", "cfg-1")
		agent := &fakeBackupStorage{}
		svc := BackupService{Store: st, Storage: agent, RunInline: true}
		if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "24h"}); err != nil {
			t.Fatal(err)
		}
		if err := st.Tasks().Create(ctx, domain.Task{
			ID: "task-backup_dataset-running", Type: domain.TaskTypeBackupDataset,
			Status: domain.TaskStatusRunning, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Trigger(ctx); err != nil {
			t.Fatalf("trigger = %v, want the in-flight run to be swallowed", err)
		}
		if len(agent.snapshot()) != 0 {
			t.Fatalf("a second backup started alongside the running one: %#v", agent.calls)
		}
	})
}

// 调度器应按时调用 RunDue，并随 context 取消而退出。
func TestBackupSchedulerRunsDueWorkAndStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := newImageTestStore(t)
	seedBackupSource(t, st, "img-1", "cfg-1")
	agent := &fakeBackupStorage{dbCopyDir: t.TempDir()}
	// 时钟停在计划点之后，RunDue 每次都判为该跑。
	svc := BackupService{Store: st, Storage: agent, RunInline: true,
		Now: func() time.Time { return time.Date(2026, 8, 25, 4, 0, 0, 0, time.UTC) }}
	if _, err := svc.SaveConfig(ctx, BackupConfigRequest{Enabled: true, Schedule: "daily@03:00"}); err != nil {
		t.Fatal(err)
	}
	// 预置 LastRunAt，跳过「从未运行只记起点」的分支。
	if err := st.BackupConfigs().Upsert(ctx, domain.BackupConfig{
		ID: domain.BackupConfigDefaultID, Enabled: true, Schedule: "daily@03:00",
		LastRunAt: ptrTime(time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { svc.RunScheduler(ctx, time.Millisecond); close(done) }()

	deadline := time.After(3 * time.Second)
	for len(agent.snapshot()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the scheduler never ran a due backup")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler outlived its context")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
