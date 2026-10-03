package ops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrBackupPoolRequired    = errs.Invalid("必须选择备份池")
	ErrBackupScheduleInvalid = errs.Invalid("备份计划无效")
	ErrBackupRunning         = errs.Conflict("备份正在进行中")
)

type BackupService struct {
	Store     store.Store
	Storage   storage.StorageAgent
	Now       func() time.Time
	RunInline bool
}

// BackupConfigRequest 不带备份池：每台节点备份到自己的备份池，由本机决定，
// 在这里指定只会多一个填错的机会（集群里还会填成别台的池）。
type BackupConfigRequest struct {
	Enabled  bool   `json:"enabled"`
	Schedule string `json:"schedule"`
}

type BackupRunResult struct {
	TaskID string `json:"task_id"`
}

type BackupStatus struct {
	Config BackupConfigStatus `json:"config"`
	// Node 是本机的备份情况；集群视图向每台节点各取一份并排展示。
	Node NodeBackupStatus `json:"node"`
	Task *domain.Task     `json:"task,omitempty"`
}

type BackupConfigStatus struct {
	Enabled  bool   `json:"enabled"`
	Schedule string `json:"schedule"`
	// ScheduleText 是界面展示用的计划文字（如「每天 03:00」）。
	ScheduleText string     `json:"schedule_text"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	// NextRunText 按服务器本地时区格式化 NextRunAt：备份按本机钟点执行，
	// 交给前端按浏览器时区格式化会和操作者填的时间对不上。
	NextRunText string     `json:"next_run_text,omitempty"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	LastTaskID  string     `json:"last_task_id,omitempty"`
}

type BackupItemStatus struct {
	Source       string `json:"source"`
	Target       string `json:"target"`
	Mode         string `json:"mode"`
	LastSnapshot string `json:"last_snapshot"`
	Error        string `json:"error"`
}

func (s BackupService) GetConfig(ctx context.Context) (domain.BackupConfig, error) {
	cfg, err := s.Store.BackupConfigs().Get(ctx, domain.BackupConfigDefaultID)
	if errs.IsNotFound(err) {
		return domain.BackupConfig{ID: domain.BackupConfigDefaultID}, nil
	}
	return cfg, err
}

func (s BackupService) SaveConfig(ctx context.Context, req BackupConfigRequest) (domain.BackupConfig, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return domain.BackupConfig{}, err
	}
	sched, err := ParseBackupSchedule(req.Schedule)
	if err != nil {
		return domain.BackupConfig{}, errs.Invalid(err.Error())
	}
	if req.Enabled && sched.Empty {
		return domain.BackupConfig{}, errs.Invalid("开启定时备份要先选好什么时候跑")
	}
	cfg.ID = domain.BackupConfigDefaultID
	cfg.Enabled = req.Enabled
	cfg.Schedule = sched.String()
	if err := s.Store.BackupConfigs().Upsert(ctx, cfg); err != nil {
		return domain.BackupConfig{}, err
	}
	return cfg, nil
}

// Run 立即开始一轮备份。写入者打标记并送本机副本；其余节点随复制收到标记后
// 由各自的后台循环送副本，本机不跨网络推流。
func (s BackupService) Run(ctx context.Context) (BackupRunResult, error) {
	if running, err := s.hasRunningBackup(ctx); err != nil {
		return BackupRunResult{}, err
	} else if running {
		return BackupRunResult{}, ErrBackupRunning
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return BackupRunResult{}, err
	}
	task, err := s.runner().Create(ctx, domain.TaskTypeBackupDataset, "")
	if err != nil {
		return BackupRunResult{}, err
	}
	now := s.now()
	cfg.ID = domain.BackupConfigDefaultID
	cfg.LastRunAt = &now
	if err := s.Store.BackupConfigs().Upsert(ctx, cfg); err != nil {
		_ = s.runner().Fail(ctx, task, err)
		return BackupRunResult{}, err
	}
	run := func() { s.executeBackup(context.Background(), task) }
	if s.RunInline {
		run()
	} else {
		go run()
	}
	return BackupRunResult{TaskID: task.ID}, nil
}

func (s BackupService) Trigger(ctx context.Context) error {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	_, err = s.Run(ctx)
	if errors.Is(err, ErrBackupRunning) {
		return nil
	}
	return err
}

// RunDue 在上次运行后又越过了计划时刻时开始一轮。按钟点而不是按间隔判断：
// 从上次运行起算间隔会逐轮漂移，最终落到上课时间。
func (s BackupService) RunDue(ctx context.Context) (bool, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return false, err
	}
	if !cfg.Enabled {
		return false, nil
	}
	sched, err := ParseBackupSchedule(cfg.Schedule)
	if err != nil {
		return false, ErrBackupScheduleInvalid
	}
	if sched.Empty {
		return false, nil
	}
	if running, err := s.hasRunningBackup(ctx); err != nil {
		return false, err
	} else if running {
		return false, nil
	}
	now := s.scheduleNow()
	// 从未运行过：等下一个计划时刻，不要在开启的那一刻就跑。
	if cfg.LastRunAt == nil {
		first := now
		cfg.LastRunAt = &first
		return false, s.Store.BackupConfigs().Upsert(ctx, cfg)
	}
	if now.Before(sched.Next(cfg.LastRunAt.In(now.Location()))) {
		return false, nil
	}
	_, err = s.Run(ctx)
	return err == nil, err
}

func (s BackupService) RunScheduler(ctx context.Context, checkEvery time.Duration) {
	if checkEvery <= 0 {
		checkEvery = time.Minute
	}
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.RunDue(ctx)
		}
	}
}

func (s BackupService) Status(ctx context.Context) (BackupStatus, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return BackupStatus{}, err
	}
	node, err := s.LocalStatus(ctx)
	if err != nil {
		return BackupStatus{}, err
	}
	latest, _ := s.latestBackupTask(ctx)
	status := BackupStatus{Config: backupConfigStatus(cfg, latest, s.scheduleNow()), Node: node}
	if latest.ID != "" {
		status.Task = &latest
	}
	return status, nil
}

func (s BackupService) executeBackup(ctx context.Context, task domain.Task) {
	marker, err := s.SnapshotRound(ctx)
	if err != nil {
		_ = s.runner().Fail(ctx, task, err)
		return
	}
	task.Progress = 40
	task.Result = "已打还原标记 " + marker
	if err := s.Store.Tasks().Update(ctx, task); err != nil {
		_ = s.runner().Fail(ctx, task, err)
		return
	}
	shipped, kind, err := s.DrainLocal(ctx)
	if err != nil {
		_ = s.runner().Fail(ctx, task, err)
		return
	}
	switch {
	case !shipped && kind == drainBusy:
		task.Result = "已打还原标记；本机正在送上一份副本，送完后由后台循环接着送这一轮"
	case shipped:
		// 带上这一轮的性质：反复走全量说明增量链在断，那是要查的信号。
		task.Result = "本机副本已更新（" + kind + "）；其余节点会在收到复制后各自更新"
	default:
		// 没有备份池不算失败：标记已打，副本由有备份池的节点保存。
		task.Result = "已打还原标记；本机没有备份池，副本由有备份池的节点保存"
	}
	_ = s.runner().Finish(ctx, s.Store, task, task.Result)
}

// shipping 保证每个进程（即每台节点）同时只有一个 DrainLocal。
var shipping sync.Mutex

// copying 在本机向备份池送副本期间为 true。
var copying atomic.Bool

// drainBusy 是已有副本在送时 DrainLocal 返回的 kind。
const drainBusy = "busy"

// backupStateKey 是 backup_states 唯一的一行：整个目录作为一条流送出，只有一份状态。
const backupStateKey = "nd"

func (s BackupService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now}
}

func (s BackupService) hasRunningBackup(ctx context.Context) (bool, error) {
	tasks, err := s.Store.Tasks().List(ctx)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.Type == domain.TaskTypeBackupDataset && (task.Status == domain.TaskStatusPending || task.Status == domain.TaskStatusRunning) {
			return true, nil
		}
	}
	return false, nil
}

func (s BackupService) latestBackupTask(ctx context.Context) (domain.Task, error) {
	tasks, err := s.Store.Tasks().List(ctx)
	if err != nil {
		return domain.Task{}, err
	}
	var latest domain.Task
	for _, task := range tasks {
		if task.Type != domain.TaskTypeBackupDataset {
			continue
		}
		if latest.ID == "" || task.CreatedAt.After(latest.CreatedAt) {
			latest = task
		}
	}
	if latest.ID == "" {
		return domain.Task{}, fmt.Errorf("backup task: %w", errs.ErrNotFound)
	}
	return latest, nil
}

func backupConfigStatus(cfg domain.BackupConfig, latest domain.Task, now time.Time) BackupConfigStatus {
	status := BackupConfigStatus{
		Enabled:   cfg.Enabled,
		Schedule:  cfg.Schedule,
		LastRunAt: cfg.LastRunAt,
	}
	if sched, err := ParseBackupSchedule(cfg.Schedule); err == nil {
		status.ScheduleText = sched.Describe()
		if cfg.Enabled && !sched.Empty {
			from := now
			if cfg.LastRunAt != nil && cfg.LastRunAt.After(from) {
				from = cfg.LastRunAt.In(now.Location())
			}
			next := sched.Next(from)
			status.NextRunAt = &next
			status.NextRunText = next.Format("2006-01-02 15:04")
		}
	}
	if latest.ID != "" {
		status.LastTaskID = latest.ID
	}
	return status
}

func (s BackupService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// scheduleNow 与 now() 同一时刻，但保留本机时区：计划时刻指服务器所在地的
// 钟点，按 UTC 算会在 UTC+8 机房把凌晨三点的备份跑到上午 11 点。
func (s BackupService) scheduleNow() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var _ interface {
	GetConfig(context.Context) (domain.BackupConfig, error)
	SaveConfig(context.Context, BackupConfigRequest) (domain.BackupConfig, error)
	Run(context.Context) (BackupRunResult, error)
	Status(context.Context) (BackupStatus, error)
} = BackupService{}

// SnapshotRound 在目录根上打本轮备份标记，只由写入者执行：备机自己打的标记
// 会在下次接收时被 -F 删掉。
// 先写 DB 副本再递归快照，让数据库行与数据集冻结在同一时刻；分组、客户机、
// 用户无法由 RecoverCatalogue 从池里重建。
func (s BackupService) SnapshotRound(ctx context.Context) (string, error) {
	dbDir, err := s.Storage.EnsureDBCopyDir(ctx)
	if err != nil {
		return "", err
	}
	// 与复制轮次同理（见 Replicator.mark）：库副本和快照之间不能落下目录变更，也不能有变更做到一半。
	defer storage.HoldCatalogue()()
	if dbDir != "" {
		if err := s.Store.Snapshot(ctx, filepath.Join(dbDir, "ndiskless.db")); err != nil {
			return "", err
		}
	}
	marker := fmt.Sprintf("%s%d", storage.BackupSnapshotPrefix, s.now().UnixNano())
	if err := s.Storage.SnapshotCatalogue(ctx, marker); err != nil {
		return "", err
	}
	return marker, nil
}

// DrainLocal 把本机持有的最新标记送进本机备份池，返回是否送了以及走的是
// 增量还是全量。每台节点（含写入者）都各自执行，切换不影响备份放在哪。
// 返回 kind 是因为反复全量说明增量链在断，需要排查。
func (s BackupService) DrainLocal(ctx context.Context) (bool, string, error) {
	// 备份任务和后台循环都会调用；两条流同时写一份副本会让副本从头重建。
	if !shipping.TryLock() {
		return false, drainBusy, nil
	}
	defer shipping.Unlock()
	copying.Store(true)
	defer copying.Store(false)
	pool, err := s.Storage.BackupPoolName(ctx)
	if err != nil {
		return false, "", err
	}
	// 没有备份池不算故障，节点照常服务客户机。
	if strings.TrimSpace(pool) == "" {
		return false, "", nil
	}
	marker, err := s.Storage.LatestBackupMarker(ctx)
	if err != nil || strings.TrimSpace(marker) == "" {
		return false, "", err
	}
	copied, err := s.Storage.CopiedBackupMarker(ctx, pool)
	if err != nil {
		return false, "", err
	}
	if copied == marker {
		return false, "", nil // 已经推过这一轮
	}
	res, err := s.Storage.Backup(ctx, storage.BackupReq{
		BackupPool: pool, Snapshot: marker, Reuse: true,
	})
	if err != nil {
		return false, "", err
	}
	kind := "全量"
	if res.Base != "" && !res.FellBack {
		kind = "增量（基准 " + res.Base + "）"
	} else if res.FellBack {
		kind = "全量（增量基准不可用，已自动退回）"
	}
	return true, kind, nil
}

// NodeBackupStatus 是单台节点的备份情况，从本机池读取而不是从数据库行读：
// 数据库随复制覆盖，切换后行里的值可能是别台的；标记名本身带有时间。
type NodeBackupStatus struct {
	BackupPool   string     `json:"backup_pool"`
	LastBackupAt *time.Time `json:"last_backup_at,omitempty"`
	// Pending 表示写入者已打了更新的标记，本机尚未送进备份池。
	Pending bool `json:"pending"`
	// Copying 表示本机正在送副本。
	Copying bool `json:"copying"`
}

func (s BackupService) LocalStatus(ctx context.Context) (NodeBackupStatus, error) {
	pool, err := s.Storage.BackupPoolName(ctx)
	if err != nil {
		return NodeBackupStatus{}, err
	}
	if strings.TrimSpace(pool) == "" {
		return NodeBackupStatus{}, nil
	}
	out := NodeBackupStatus{BackupPool: pool}
	copied, err := s.Storage.CopiedBackupMarker(ctx, pool)
	if err != nil {
		return NodeBackupStatus{}, err
	}
	if at, ok := markerTime(copied); ok {
		out.LastBackupAt = &at
	}
	marker, err := s.Storage.LatestBackupMarker(ctx)
	if err != nil {
		return NodeBackupStatus{}, err
	}
	out.Pending = marker != "" && marker != copied
	out.Copying = copying.Load()
	return out, nil
}

// markerTime 从「ndbackup-<unix 纳秒>」解析时间；旧版本的标记格式相同。
// 解析不了的视为无时间，不算错误。
func markerTime(marker string) (time.Time, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(marker, storage.BackupSnapshotPrefix), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n).UTC(), true
}
