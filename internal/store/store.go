package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/tianwei/diskless/internal/domain"
)

type Repository[T any] interface {
	Create(context.Context, T) error
	Get(context.Context, string) (T, error)
	List(context.Context) ([]T, error)
	Update(context.Context, T) error
	Delete(context.Context, string) error
}

type ImageRepo interface {
	Repository[domain.Image]
	GetByName(context.Context, string) (domain.Image, error)
}
type ConfigRepo interface {
	Repository[domain.Config]
	ListByImage(context.Context, string) ([]domain.Config, error)
}
type ReductionRepo interface {
	Repository[domain.Reduction]
	ListByConfig(context.Context, string) ([]domain.Reduction, error)
	DeleteByConfig(context.Context, string) error
}
type GroupRepo interface{ Repository[domain.Group] }
type GroupDiskRepo interface{ Repository[domain.GroupDisk] }
type TerminalRepo interface {
	Repository[domain.Terminal]
	GetByMAC(context.Context, string) (domain.Terminal, error)
}
type ServerRepo interface{ Repository[domain.Server] }
type PoolRepo interface{ Repository[domain.Pool] }
type PoolDiskRepo interface{ Repository[domain.PoolDisk] }
type ClientCloneRepo interface{ Repository[domain.ClientClone] }
type TaskRepo interface {
	Repository[domain.Task]
	ListActive(context.Context) ([]domain.Task, error)
	ListPaged(ctx context.Context, status string, limit, offset int) ([]domain.Task, int, error)
}
type UserRepo interface {
	Repository[domain.User]
	GetByUsername(context.Context, string) (domain.User, error)
}
type ImageHealthReportRepo interface {
	Repository[domain.ImageHealthReport]
	GetByImage(context.Context, string) (domain.ImageHealthReport, error)
}
type DriverPackRepo interface{ Repository[domain.DriverPack] }
type DriverBundleRepo interface {
	Repository[domain.DriverBundle]
}
type DriverBundlePackRepo interface {
	Repository[domain.DriverBundlePack]
}
type BackupConfigRepo interface {
	Repository[domain.BackupConfig]
	Upsert(context.Context, domain.BackupConfig) error
}
type SystemSettingsRepo interface {
	Repository[domain.SystemSettings]
	Upsert(context.Context, domain.SystemSettings) error
}
type BackupStateRepo interface {
	Repository[domain.BackupState]
	Upsert(context.Context, domain.BackupState) error
}
type BootFailureRepo interface {
	List(context.Context) ([]domain.BootFailure, error)
	Upsert(context.Context, domain.BootFailure) error
}
type ReplicationStateRepo interface {
	List(context.Context) ([]domain.ReplicationState, error)
	Upsert(context.Context, domain.ReplicationState) error
}
type AuditLogRepo interface {
	Repository[domain.AuditLog]
	ListPaged(ctx context.Context, f AuditFilter, limit, offset int) ([]domain.AuditLog, int, error)
}
type AlarmRepo interface {
	Repository[domain.Alarm]
	ListPaged(ctx context.Context, f AlarmFilter, limit, offset int) ([]domain.Alarm, int, error)
	ListOpen(ctx context.Context) ([]domain.Alarm, error)
	Summary(ctx context.Context) (AlarmSummary, error)
}

type Store interface {
	Images() ImageRepo
	Configs() ConfigRepo
	Reductions() ReductionRepo
	Groups() GroupRepo
	GroupDisks() GroupDiskRepo
	Terminals() TerminalRepo
	Servers() ServerRepo
	Pools() PoolRepo
	PoolDisks() PoolDiskRepo
	ClientClones() ClientCloneRepo
	Tasks() TaskRepo
	Users() UserRepo
	DriverPacks() DriverPackRepo
	DriverBundles() DriverBundleRepo
	DriverBundlePacks() DriverBundlePackRepo
	ImageHealthReports() ImageHealthReportRepo
	BackupConfigs() BackupConfigRepo
	SystemSettings() SystemSettingsRepo
	BackupStates() BackupStateRepo
	ReplicationStates() ReplicationStateRepo
	BootFailures() BootFailureRepo
	AuditLogs() AuditLogRepo
	Alarms() AlarmRepo
	Tx(context.Context, func(Store) error) error
	// Snapshot 把在用库的一致副本写到 path，替换那里的旧副本。分组、终端、用户和设置只在这些行里，
	// 这是备份中池恢复不了的部分。
	Snapshot(ctx context.Context, path string) error
}

type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type SQLStore struct {
	db      *sql.DB
	q       queryer
	dialect string
	// rev 统计真正改了目录的写入次数。与事务派生出的 store 共享，事务内的写入同样计数。
	rev *atomic.Uint64
}

// counting 包装 queryer，经仓储执行的写入会递增版本号；读操作和复制状态的写入不计（见 Revision）。
type counting struct {
	queryer
	rev *atomic.Uint64
}

func (c counting) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := c.queryer.ExecContext(ctx, query, args...)
	if err == nil && !strings.HasPrefix(query, "INSERT INTO replication_state ") {
		c.rev.Add(1)
	}
	return res, err
}

// uncounted 返回不计数的底层 queryer，只给记账写入用。
func (s *SQLStore) uncounted() queryer {
	if c, ok := s.q.(counting); ok {
		return c.queryer
	}
	return s.q
}

// serverRepository 让只刷新存活信息的更新不计数：心跳每个周期都写，计入的话空闲集群每轮都要复制。
type serverRepository struct {
	repository[domain.Server]
	quiet repository[domain.Server]
}

func (r serverRepository) Update(ctx context.Context, v domain.Server) error {
	if old, err := r.Get(ctx, v.ID); err == nil && livenessOnly(old, v) {
		return r.quiet.Update(ctx, v)
	}
	return r.repository.Update(ctx, v)
}

// livenessOnly 判断两行只差心跳时间和在线状态。epoch、角色、地址的变化要复制：接任时靠名册取 epoch 下限。
func livenessOnly(a, b domain.Server) bool {
	a.Status, a.Heartbeat, a.LastSeenAt = b.Status, b.Heartbeat, b.LastSeenAt
	return a == b
}

// poolRepository 让只刷新容量的更新不计数：池列表每次读都会把实时用量写回，用量又随每轮复制变化。
type poolRepository struct {
	repository[domain.Pool]
	quiet repository[domain.Pool]
}

func (r poolRepository) Update(ctx context.Context, v domain.Pool) error {
	if old, err := r.Get(ctx, v.ID); err == nil && usageOnly(old, v) {
		return r.quiet.Update(ctx, v)
	}
	return r.repository.Update(ctx, v)
}

func usageOnly(a, b domain.Pool) bool {
	return a.ServerID == b.ServerID && a.Name == b.Name && a.Layout == b.Layout && a.GroupWidth == b.GroupWidth &&
		slices.Equal(a.Disks, b.Disks) && slices.Equal(a.ReadCacheDisks, b.ReadCacheDisks) &&
		slices.Equal(a.WriteCacheDisks, b.WriteCacheDisks)
}

func New(db *sql.DB, dialect string) *SQLStore {
	rev := &atomic.Uint64{}
	return &SQLStore{db: db, q: counting{queryer: db, rev: rev}, dialect: dialect, rev: rev}
}

// Revision 返回本进程启动以来的目录写入次数，复制器用它而不是 stat 库文件。
// 文件时间戳答不了这个问题：健康探针每 2 秒写一行再回滚，回滚的写也会弄脏 -wal，空闲集群会不停复制。
// 只统计经仓储的写入，探针走裸事务、故意不计；复制状态和只刷新心跳的节点更新也不计。
// 只在进程内：本节点是自己在用库的唯一写者，重启也会重置复制器记住的指纹。
func (s *SQLStore) Revision() uint64 {
	if s.rev == nil {
		return 0
	}
	return s.rev.Load()
}

func (s *SQLStore) Close() error {
	return s.db.Close()
}

// Ping 判断库能否回答一个简单查询，供健康检查的库探针使用。只在具体类型上提供，业务代码不 ping。
func (s *SQLStore) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// CheckWritable 确认库还能写。Ping 只读，而磁盘满、文件只读、重挂为 ro 时读照样成功；
// 持有 VIP 的节点是唯一允许写的，可写才是要紧的属性。写的是立即回滚的空事务，只花一次日志往返，不改任何东西。
func (s *SQLStore) CheckWritable(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS ndiskless_write_probe (id INTEGER PRIMARY KEY)"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO ndiskless_write_probe (id) VALUES (1) ON CONFLICT(id) DO UPDATE SET id = 1"); err != nil {
		return err
	}
	return nil
}

func (s *SQLStore) Tx(ctx context.Context, fn func(Store) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txStore := &SQLStore{db: s.db, q: counting{queryer: tx, rev: s.rev}, dialect: s.dialect, rev: s.rev}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) Images() ImageRepo {
	return imageRepository{repository: newRepository(s.q, s.dialect, imageCodec)}
}
func (s *SQLStore) Configs() ConfigRepo {
	return configRepository{repository: newRepository(s.q, s.dialect, configCodec)}
}
func (s *SQLStore) Reductions() ReductionRepo {
	return reductionRepository{repository: newRepository(s.q, s.dialect, reductionCodec)}
}
func (s *SQLStore) Groups() GroupRepo         { return newRepository(s.q, s.dialect, groupCodec) }
func (s *SQLStore) GroupDisks() GroupDiskRepo { return newRepository(s.q, s.dialect, groupDiskCodec) }
func (s *SQLStore) Terminals() TerminalRepo {
	return terminalRepository{repository: newRepository(s.q, s.dialect, terminalCodec)}
}
func (s *SQLStore) Servers() ServerRepo {
	return serverRepository{
		repository: newRepository(s.q, s.dialect, serverCodec),
		quiet:      newRepository(s.uncounted(), s.dialect, serverCodec),
	}
}
func (s *SQLStore) Pools() PoolRepo {
	return poolRepository{
		repository: newRepository(s.q, s.dialect, poolCodec),
		quiet:      newRepository(s.uncounted(), s.dialect, poolCodec),
	}
}
func (s *SQLStore) PoolDisks() PoolDiskRepo { return newRepository(s.q, s.dialect, poolDiskCodec) }
func (s *SQLStore) ClientClones() ClientCloneRepo {
	return newRepository(s.q, s.dialect, clientCloneCodec)
}
func (s *SQLStore) ImageHealthReports() ImageHealthReportRepo {
	return imageHealthReportRepository{repository: newRepository(s.q, s.dialect, imageHealthReportCodec)}
}
func (s *SQLStore) DriverPacks() DriverPackRepo {
	return newRepository(s.q, s.dialect, driverPackCodec)
}
func (s *SQLStore) DriverBundles() DriverBundleRepo {
	return newRepository(s.q, s.dialect, driverBundleCodec)
}
func (s *SQLStore) DriverBundlePacks() DriverBundlePackRepo {
	return newRepository(s.q, s.dialect, driverBundlePackCodec)
}
func (s *SQLStore) BackupConfigs() BackupConfigRepo {
	return backupConfigRepository{repository: newRepository(s.q, s.dialect, backupConfigCodec)}
}
func (s *SQLStore) SystemSettings() SystemSettingsRepo {
	return systemSettingsRepository{repository: newRepository(s.q, s.dialect, systemSettingsCodec)}
}
func (s *SQLStore) AuditLogs() AuditLogRepo {
	return auditLogRepository{repository: newRepository(s.q, s.dialect, auditLogCodec)}
}
func (s *SQLStore) Alarms() AlarmRepo {
	return alarmRepository{
		repository: newRepository(s.q, s.dialect, alarmCodec),
		quiet:      newRepository(s.uncounted(), s.dialect, alarmCodec),
	}
}
func (s *SQLStore) BackupStates() BackupStateRepo {
	return backupStateRepository{repository: newRepository(s.q, s.dialect, backupStateCodec)}
}
func (s *SQLStore) BootFailures() BootFailureRepo {
	return bootFailureRepository{repository: newRepository(s.q, s.dialect, bootFailureCodec)}
}
func (s *SQLStore) ReplicationStates() ReplicationStateRepo {
	return replicationStateRepository{repository: newRepository(s.q, s.dialect, replicationStateCodec)}
}
func (s *SQLStore) Tasks() TaskRepo {
	return taskRepository{repository: newRepository(s.q, s.dialect, taskCodec)}
}
func (s *SQLStore) Users() UserRepo {
	return userRepository{repository: newRepository(s.q, s.dialect, userCodec)}
}
