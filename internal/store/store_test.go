package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func TestMigrationsApply(t *testing.T) {
	st := newTestStore(t)

	var hasNameColumn int
	if err := st.db.QueryRow(`SELECT 1 FROM pragma_table_info('terminals') WHERE name = 'name'`).Scan(&hasNameColumn); err != nil {
		t.Fatalf("terminal name column missing: %v", err)
	}
	if hasNameColumn != 1 {
		t.Fatal("terminal name column was not added")
	}

	var n int
	if err := st.db.QueryRow("SELECT count(*) FROM images").Scan(&n); err != nil {
		t.Fatalf("images table missing: %v", err)
	}
	if err := st.db.QueryRow("SELECT count(*) FROM client_clones").Scan(&n); err != nil {
		t.Fatalf("client_clones table missing: %v", err)
	}
}

// 池布局是池身份的一部分，决定「加盘」「移除盘」能做什么：池记录保存布局，盘记录保存所在 vdev 组
// 以及 special、spare 两种角色。
func TestPoolLayoutPersists(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "server-a", Name: "a", IP: "10.0.0.1", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	pool := domain.Pool{ID: "pool-t", ServerID: "server-a", Name: "t", Disks: []string{"/dev/sdb", "/dev/sdc"}, Layout: domain.PoolLayoutMirror, GroupWidth: 2}
	if err := st.Pools().Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got, err := st.Pools().Get(ctx, "pool-t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Layout != domain.PoolLayoutMirror || got.GroupWidth != 2 {
		t.Fatalf("layout = %q/%d", got.Layout, got.GroupWidth)
	}
	// 没有布局的旧记录（或未设布局的代码写的）按条带处理：产品建过的池都是条带。
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-old", ServerID: "server-a", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	if got, err = st.Pools().Get(ctx, "pool-old"); err != nil || got.Layout != domain.PoolLayoutStripe || got.GroupWidth != 1 {
		t.Fatalf("legacy pool = %#v, %v", got, err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO pools (id,server_id,name,layout) VALUES ('bad','server-a','bad','raid10')`); err == nil {
		t.Fatal("unknown layout accepted")
	}
	for _, d := range []domain.PoolDisk{
		{ID: "d1", PoolID: "pool-t", Path: "/dev/sdb", Role: domain.PoolDiskRoleData, Vdev: "mirror-0"},
		{ID: "d2", PoolID: "pool-t", Path: "/dev/nvme0n1", Role: domain.PoolDiskRoleSpecial, Vdev: "mirror-1"},
		{ID: "d3", PoolID: "pool-t", Path: "/dev/sdz", Role: domain.PoolDiskRoleSpare, Vdev: ""},
	} {
		if err := st.PoolDisks().Create(ctx, d); err != nil {
			t.Fatalf("create pool disk %s: %v", d.Path, err)
		}
	}
	disks, err := st.PoolDisks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]domain.PoolDisk{}
	for _, d := range disks {
		byPath[d.Path] = d
	}
	if byPath["/dev/sdb"].Vdev != "mirror-0" || byPath["/dev/nvme0n1"].Role != domain.PoolDiskRoleSpecial || byPath["/dev/sdz"].Role != domain.PoolDiskRoleSpare {
		t.Fatalf("pool disks = %#v", disks)
	}
}

// 镜像记录用途（系统盘或数据盘）和来源。旧记录都是导入的系统镜像，那是当时唯一的类型。
func TestImagePurposeAndOriginPersist(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "games", Name: "games", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeData, Origin: domain.ImageOriginBlank, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Images().Get(ctx, "games")
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != domain.ImagePurposeData || got.Origin != domain.ImageOriginBlank {
		t.Fatalf("image = %#v", got)
	}
	if err := st.Images().Create(ctx, domain.Image{ID: "old", Name: "old", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got, err = st.Images().Get(ctx, "old"); err != nil || got.Purpose != domain.ImagePurposeSystem || got.Origin != domain.ImageOriginImported {
		t.Fatalf("legacy image = %#v, %v", got, err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO images (id,name,os_type,state,purpose,created_at) VALUES ('bad','bad','windows','normal','cache',?)`, timeValue(now)); err == nil {
		t.Fatal("unknown purpose accepted")
	}
}

func TestConstraintsAndCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))

	img := domain.Image{ID: "img-1", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := st.Images().Create(ctx, domain.Image{ID: "img-2", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err == nil {
		t.Fatal("duplicate image name accepted")
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO images (id,name,os_type,state,created_at) VALUES ('bad','bad','plan9','normal',?)`, timeValue(now)); err == nil {
		t.Fatal("invalid enum accepted")
	}

	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}
	red := domain.Reduction{ID: "red-1", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	if err := st.Reductions().Create(ctx, red); err != nil {
		t.Fatalf("create reduction: %v", err)
	}
	group := domain.Group{
		ID:                "grp-1",
		Name:              "default",
		IsDefault:         true,
		StartIP:           "192.168.1.10",
		ClientMax:         10,
		Gateway:           "192.168.1.1",
		Netmask:           "255.255.255.0",
		SystemImageID:     img.ID,
		SystemConfigID:    cfg.ID,
		SystemReductionID: red.ID,
	}
	if err := st.Groups().Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	terminal := domain.Terminal{
		ID:      "term-1",
		Name:    "desk-1",
		MAC:     "AABBCCDDEEFF",
		IP:      "192.168.1.10",
		GroupID: group.ID,
		State:   domain.TerminalStateUnknown,
	}
	if err := st.Terminals().Create(ctx, terminal); err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-2", MAC: terminal.MAC, IP: "192.168.1.11", GroupID: group.ID, State: domain.TerminalStateUnknown}); err == nil {
		t.Fatal("duplicate terminal mac accepted")
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-3", MAC: "FFEEDDCCBBAA", IP: terminal.IP, GroupID: group.ID, State: domain.TerminalStateUnknown}); err == nil {
		t.Fatal("duplicate terminal ip accepted")
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-4", MAC: "001122334455", IP: "192.168.1.12", GroupID: "missing", State: domain.TerminalStateUnknown}); err == nil {
		t.Fatal("missing group foreign key accepted")
	}
	gotTerminal, err := st.Terminals().Get(ctx, terminal.ID)
	if err != nil {
		t.Fatalf("get terminal: %v", err)
	}
	if gotTerminal.Name != terminal.Name {
		t.Fatalf("terminal name = %q, want %q", gotTerminal.Name, terminal.Name)
	}

	got, err := st.Images().Get(ctx, img.ID)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at location = %v, want UTC", got.CreatedAt.Location())
	}
	got.Remark = "updated"
	if err := st.Images().Update(ctx, got); err != nil {
		t.Fatalf("update image: %v", err)
	}
	got, _ = st.Images().Get(ctx, img.ID)
	if got.Remark != "updated" {
		t.Fatalf("remark = %q", got.Remark)
	}
}

func TestTxRollback(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	errRollback := errors.New("rollback")

	err := st.Tx(ctx, func(tx Store) error {
		return errors.Join(
			tx.Users().Create(ctx, domain.User{ID: "user-1", Username: "admin", PasswordHash: "hash", CreatedAt: time.Now()}),
			errRollback,
		)
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Tx error = %v", err)
	}
	if _, err := st.Users().Get(ctx, "user-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled back user lookup err = %v", err)
	}
}

func TestSystemSettingsRepoUpsert(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty settings err = %v", err)
	}
	if err := st.SystemSettings().Upsert(ctx, domain.SystemSettings{ID: domain.SystemSettingsDefaultID, ImportDir: "/tank/imports"}); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if err := st.SystemSettings().Upsert(ctx, domain.SystemSettings{ID: domain.SystemSettingsDefaultID, ImportDir: "/data/imports"}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	got, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != "/data/imports" {
		t.Fatalf("settings = %#v", got)
	}
}

func TestPostgresDSNUsesPGX(t *testing.T) {
	driver, dialect, err := driverForDSN("postgres://user:pass@localhost/db")
	if err != nil {
		t.Fatal(err)
	}
	if driver != "pgx" || dialect != "postgres" {
		t.Fatalf("driver=%q dialect=%q", driver, dialect)
	}
}

func TestPostgresMigrationsApplyWhenDSNProvided(t *testing.T) {
	dsn := os.Getenv("NDISKLESS_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("set NDISKLESS_PG_TEST_DSN to run optional PostgreSQL migration test")
	}
	st, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
}

func TestTaskRepoListPaged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	mk := func(id string, status domain.TaskStatus, offsetMin int) domain.Task {
		return domain.Task{ID: id, Type: domain.TaskTypeImportImage, Status: status, CreatedAt: base.Add(time.Duration(offsetMin) * time.Minute)}
	}
	for _, task := range []domain.Task{
		mk("t1", domain.TaskStatusSuccess, 0),
		mk("t2", domain.TaskStatusRunning, 1),
		mk("t3", domain.TaskStatusFailed, 2),
		mk("t4", domain.TaskStatusSuccess, 3),
	} {
		if err := st.Tasks().Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}

	// 按新到旧排序并分页。
	page1, total, err := st.Tasks().ListPaged(ctx, "", 2, 0)
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if len(page1) != 2 || page1[0].ID != "t4" || page1[1].ID != "t3" {
		t.Fatalf("page1 = %#v", page1)
	}
	page2, _, err := st.Tasks().ListPaged(ctx, "", 2, 2)
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != "t2" || page2[1].ID != "t1" {
		t.Fatalf("page2 = %#v", page2)
	}

	// 按状态过滤，总数单独计算。
	success, successTotal, err := st.Tasks().ListPaged(ctx, string(domain.TaskStatusSuccess), 10, 0)
	if err != nil {
		t.Fatalf("list success: %v", err)
	}
	if successTotal != 2 || len(success) != 2 || success[0].ID != "t4" || success[1].ID != "t1" {
		t.Fatalf("success = %#v total=%d", success, successTotal)
	}
}

// 迁移 0040 之前的设置行 data_pool 为 NULL，扫进普通 string 会报
// "converting NULL to string is unsupported"，让所有设置读取（含镜像导入路径）都 500。Get 必须把 NULL 当作 ""。
func TestSystemSettingsGetToleratesNullDataPool(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO system_settings (id, import_dir, client_iface, allow_cross_subnet, replication_rate_mbps, data_pool)
		 VALUES (?, ?, ?, ?, NULL, NULL)`,
		domain.SystemSettingsDefaultID, "/tank/imports", "", 0); err != nil {
		t.Fatalf("seed null-data_pool row: %v", err)
	}
	got, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DataPool != "" {
		t.Fatalf("DataPool = %q, want empty", got.DataPool)
	}
	if got.ImportDir != "/tank/imports" {
		t.Fatalf("ImportDir = %q, want /tank/imports", got.ImportDir)
	}
}

func newTestStore(t *testing.T) *SQLStore {
	t.Helper()
	st, err := Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Snapshot 把在用库的一致副本写到文件，这是 RecoverCatalogue 无法从池重建的部分（分组、终端、用户）。
// 会覆盖旧副本：VACUUM INTO 拒绝已存在的文件，实现得自己腾位置。
func TestSnapshotWritesAReopenableCopy(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Users().Create(ctx, domain.User{ID: "u1", Username: "admin", PasswordHash: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "copy.db")
	if err := st.Snapshot(ctx, dest); err != nil {
		t.Fatal(err)
	}
	// 跑两次：第二次必须替换，而不是因文件已存在而失败。
	if err := st.Snapshot(ctx, dest); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	copyStore, err := Open(ctx, "file:"+dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	users, err := copyStore.Users().List(ctx)
	if err != nil || len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("users in copy = %#v err=%v", users, err)
	}
}

// WAL 和 busy timeout 让读者（健康探针、状态页）能与写者共存，而不是当场 SQLITE_BUSY。
func TestOpenTurnsOnWALAndBusyTimeout(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, "file:"+filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var mode string
	if err := st.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := st.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout < 5000 {
		t.Fatalf("busy_timeout = %d, want >= 5000", timeout)
	}
}

// 复制状态行是运维和滞后告警读的：哪个对端、最后落地的快照、时间、上次的错误。
func TestReplicationStateUpsertAndList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC)
	row := domain.ReplicationState{
		Target: "http://192.168.50.10:8080", Kind: "standby-pull", Root: "tank/nd",
		LastSnapshot: "rep-1", LastOKAt: &now, LastError: "", UpdatedAt: now,
	}
	if err := st.ReplicationStates().Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.LastSnapshot, row.LastError = "rep-2", "short read"
	if err := st.ReplicationStates().Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ReplicationStates().List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v err=%v", rows, err)
	}
	if rows[0].LastSnapshot != "rep-2" || rows[0].LastError != "short read" || rows[0].LastOKAt == nil {
		t.Fatalf("row = %#v", rows[0])
	}
}

// 复制限速在运行期设置：NULL（未设置）与 0（明确不限速）不同，前者回落到部署默认值。
func TestSystemSettingsReplicationRateThreeStates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SystemSettings().Upsert(ctx, domain.SystemSettings{ID: domain.SystemSettingsDefaultID, ImportDir: "/x"}); err != nil {
		t.Fatal(err)
	}
	row, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil || row.ReplicationRateMBPS != nil {
		t.Fatalf("unset must read as nil: %#v err=%v", row.ReplicationRateMBPS, err)
	}
	zero := 0
	row.ReplicationRateMBPS = &zero
	if err := st.SystemSettings().Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	row, _ = st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if row.ReplicationRateMBPS == nil || *row.ReplicationRateMBPS != 0 {
		t.Fatalf("explicit zero must survive: %#v", row.ReplicationRateMBPS)
	}
}

// Ping 只读，而库不可写（磁盘满、文件只读、文件系统重挂为 ro）时读照样成功，探针会一直报健康。
// 持有 VIP 的节点是唯一允许写的，可写才是真正要紧的属性。
func TestCheckWritableFailsOnAReadOnlyDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "rw.db")
	st, err := Open(ctx, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckWritable(ctx); err != nil {
		t.Fatalf("a healthy database must pass: %v", err)
	}
	st.Close()

	// 以只读方式重新打开：读仍可用，写不行。
	ro, err := Open(ctx, "file:"+path+"?mode=ro")
	if err != nil {
		t.Skipf("read-only open unsupported here: %v", err)
	}
	defer ro.Close()
	if err := ro.Ping(ctx); err != nil {
		t.Fatalf("reads must still work — that is the whole problem: %v", err)
	}
	if err := ro.CheckWritable(ctx); err == nil {
		t.Fatal("a read-only database must not pass the writability check")
	}
}

// 老库里池记录 ID 只由池名派生，多台机器的 tank 是同一行。升级后每台都要能认领自己那一行；
// 迁移只能改已存在的那一行，其余节点启动时各自登记。这里守的是已存在的那一行没被漏掉。
func TestPoolIdentityMigrationScopesExistingRowToItsNode(t *testing.T) {
	st := newTestStore(t)
	// server_id 有外键，先把节点放进花名册。
	if _, err := st.db.Exec(
		`INSERT INTO servers (id, name, ip, role, status) VALUES ('2f454fa237eb','n1','192.168.10.3','all','up')`,
	); err != nil {
		t.Fatal(err)
	}
	// 模拟升级前的一行：ID 不含节点，但 server_id 记着主人。
	if _, err := st.db.Exec(
		`INSERT INTO pools (id, server_id, name, capacity, used) VALUES ('pool-tank','2f454fa237eb','tank',0,0)`,
	); err != nil {
		t.Fatal(err)
	}
	// 子表有外键指向 pools(id)，不插这一行，迁移改 ID 时外键根本不会触发，单测就会放过让服务起不来的迁移。
	if _, err := st.db.Exec(
		`INSERT INTO pool_disks (id, pool_id, path, role, capacity, used)
		 VALUES ('pd-1','pool-tank','/dev/sdb','data',0,0)`,
	); err != nil {
		t.Fatal(err)
	}

	// 跑迁移文件本身而不是手抄一份，否则手抄版与文件走偏时测试会替坏迁移背书。goose 把整个文件放在一个事务里执行，这里照做。
	raw, err := os.ReadFile("migrations/0039_pool_identity_per_node.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range strings.Split(string(raw), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") && !strings.Contains(stmt, "\n") {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("迁移语句执行失败：%v\n%s", err, stmt)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交时外键校验没过——父子两表没能一起改完：%v", err)
	}

	var childID string
	if err := st.db.QueryRow(`SELECT pool_id FROM pool_disks WHERE id = 'pd-1'`).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if childID != "pool-2f454fa237eb--tank" {
		t.Fatalf("盘的归属没跟着池走：%s", childID)
	}
	var id string
	if err := st.db.QueryRow(`SELECT id FROM pools WHERE name = 'tank'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "pool-2f454fa237eb--tank" {
		t.Fatalf("迁移后的 ID 仍未带上节点：%s", id)
	}
}

// 覆盖旧副本必须是原子的：先删后写，中间一断就什么都没了。
// 这份副本是备机能否接管的前提（激活时用它换掉在用库，没有就拒绝激活），而写它的时刻恰恰最容易被打断：
// 角色切换本身就是一次进程重启。丢了副本，集群就会有 VIP 却没有主机。
func TestSnapshotKeepsTheOldCopyWhenTheNewOneFails(t *testing.T) {
	st := newTestStore(t)
	path := filepath.Join(t.TempDir(), "ndiskless.db")
	if err := os.WriteFile(path, []byte("上一轮的副本"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 关掉库让这次写必然失败，等价于写到一半进程没了。
	_ = st.Close()

	if err := st.Snapshot(context.Background(), path); err == nil {
		t.Fatal("写不出新副本时必须报错，不能装作成功")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("旧副本被删了，备机从此无法接管：%v", err)
	}
	if string(b) != "上一轮的副本" {
		t.Fatalf("旧副本被改坏了：%q", string(b))
	}
}

// 复制器靠「目录变没变」决定是否打一轮。若把健康探针的写入也算作变化，空闲集群会不停空转
// （keepalived 每 2 秒问 /healthz，探针每次弄脏 -wal）。变更计数只数真实的目录写入，探针走裸事务，不计入。
func TestWriteProbeDoesNotLookLikeACatalogueChange(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	before := st.Revision()
	for i := 0; i < 5; i++ {
		if err := st.CheckWritable(ctx); err != nil {
			t.Fatalf("健康探针本身要能通过：%v", err)
		}
	}
	if st.Revision() != before {
		t.Fatalf("健康探针把变更计数推高了 %d→%d：空闲集群会因此每轮空转",
			before, st.Revision())
	}

	// 真改了目录，计数必须变，否则改动永远送不到备机。
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-1", Name: "node-1", Role: "all", Status: "up"}); err != nil {
		t.Fatal(err)
	}
	if st.Revision() == before {
		t.Fatal("真实写入必须推高变更计数，否则复制器看不见这次改动")
	}
}

// 任务记下执行节点。写入者的库在切换时会被接过去，历史任务不一定是当前这台跑的，事后无法推断，只能创建时记住。
func TestTaskRecordsTheNodeThatRanIt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	task := domain.Task{ID: "task-1", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusPending, Node: "192.168.10.5", CreatedAt: time.Now().UTC()}
	if err := st.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := st.Tasks().Get(ctx, "task-1")
	if err != nil || got.Node != "192.168.10.5" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestBootFailureKeepsTheLatestPerMachine(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	at := time.Date(2026, 9, 30, 7, 26, 0, 0, time.UTC)
	first := domain.BootFailure{MAC: "0050562EF64E", Stage: domain.BootStageSanboot, Code: "0x3f122003", Platform: "efi", ImageID: "ubuntu", At: at}
	if err := st.BootFailures().Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Stage, second.Code, second.At = domain.BootStageSanhook, "", at.Add(time.Minute)
	if err := st.BootFailures().Upsert(ctx, second); err != nil {
		t.Fatal(err)
	}
	rows, err := st.BootFailures().List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v err=%v", rows, err)
	}
	if rows[0] != second {
		t.Fatalf("row = %#v, want %#v", rows[0], second)
	}
}

func TestImageHealthReportKeepsBootModes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Images().Create(ctx, domain.Image{ID: "ubuntu", Name: "ubuntu", OSType: domain.OSTypeLinux, State: domain.ImageStateNormal, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	report := domain.ImageHealthReport{ID: "health-ubuntu", ImageID: "ubuntu", Level: domain.HealthOK, BootModes: []string{domain.BootModeBIOS}, CreatedAt: time.Now().UTC()}
	if err := st.ImageHealthReports().Create(ctx, report); err != nil {
		t.Fatal(err)
	}
	got, err := st.ImageHealthReports().GetByImage(ctx, "ubuntu")
	if err != nil || len(got.BootModes) != 1 || got.BootModes[0] != domain.BootModeBIOS {
		t.Fatalf("boot modes = %#v err=%v", got.BootModes, err)
	}
}

func TestImageHealthReportKeepsNICPCIIDs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Images().Create(ctx, domain.Image{ID: "win11", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	report := domain.ImageHealthReport{ID: "health-win11", ImageID: "win11", Level: domain.HealthOK, NICPCIIDs: []string{"10EC:8125", "8086:15B8"}, CreatedAt: time.Now().UTC()}
	if err := st.ImageHealthReports().Create(ctx, report); err != nil {
		t.Fatal(err)
	}
	got, err := st.ImageHealthReports().GetByImage(ctx, "win11")
	if err != nil || strings.Join(got.NICPCIIDs, ",") != "10EC:8125,8086:15B8" {
		t.Fatalf("nic pci ids = %#v err=%v", got.NICPCIIDs, err)
	}
}

func TestCopyImageTaskIsAccepted(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	task := domain.Task{ID: "task-copy", Type: domain.TaskTypeCopyImage, Status: domain.TaskStatusPending, Node: "n1", CreatedAt: time.Now().UTC()}
	if err := st.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Tasks().Get(ctx, "task-copy"); err != nil || got.Node != "n1" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// 复制和备份都往同一目录写库副本：固定的临时文件名会让一方删掉另一方写到一半的文件。
func TestSnapshotLeavesAnotherWritersTempFileAlone(t *testing.T) {
	st := newTestStore(t)
	path := filepath.Join(t.TempDir(), "ndiskless.db")
	other := path + ".tmp"
	if err := os.WriteFile(other, []byte("另一方写到一半"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "另一方写到一半" {
		t.Fatalf("另一方的临时文件被删或被改：%q err=%v", b, err)
	}
	if _, err := Open(context.Background(), "file:"+path); err != nil {
		t.Fatalf("副本打不开：%v", err)
	}
}

// 心跳和复制状态每个周期都写；若计入变更计数，复制指纹每轮都变，空闲集群每轮都打一次复制。
// 节点增删、epoch 和角色变化仍要计数：接任时靠库副本里的名册取 epoch 下限。
func TestBookkeepingWritesDoNotLookLikeACatalogueChange(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seen := time.Now().UTC()
	srv := domain.Server{ID: "node-1", Name: "node-1", Role: "all", Status: "up", Epoch: 1, HAState: "active", LastSeenAt: &seen}
	if err := st.Servers().Create(ctx, srv); err != nil {
		t.Fatal(err)
	}

	before := st.Revision()
	for i := 0; i < 3; i++ {
		later := seen.Add(time.Duration(i+1) * time.Second)
		srv.LastSeenAt, srv.Heartbeat, srv.Status = &later, &later, "up"
		if err := st.Servers().Update(ctx, srv); err != nil {
			t.Fatal(err)
		}
		if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{
			Target: "http://a:8080", Kind: "pull", Root: "tank/nd", LastSnapshot: "rep-1", UpdatedAt: later,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Tx(ctx, func(tx Store) error {
		srv.Status = "down"
		return tx.Servers().Update(ctx, srv)
	}); err != nil {
		t.Fatal(err)
	}
	if st.Revision() != before {
		t.Fatalf("心跳与复制状态推高了变更计数 %d→%d：空闲集群会每轮复制", before, st.Revision())
	}

	srv.Epoch = 2
	if err := st.Servers().Update(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if st.Revision() == before {
		t.Fatal("epoch 变化必须计数，否则接任方的名册拿到陈旧的 epoch 下限")
	}
	before = st.Revision()
	if err := st.Users().Create(ctx, domain.User{ID: "u1", Username: "admin", PasswordHash: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if st.Revision() == before {
		t.Fatal("业务数据写入必须计数")
	}
}

// 池容量和告警数值每轮都会刷新，计入变更的话空闲集群每轮都要复制；它们随下一次真实写入一起送到备机即可。
// 盘、布局、告警级别和状态的变化仍要计入。
func TestStatusRefreshDoesNotLookLikeACatalogueChange(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-1", Name: "node-1", Role: "all", Status: "up"}); err != nil {
		t.Fatal(err)
	}
	pool := domain.Pool{ID: "pool-1", ServerID: "node-1", Name: "tank", Disks: []string{"/dev/sdb"}, Capacity: 100, Used: 10}
	if err := st.Pools().Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	pool, _ = st.Pools().Get(ctx, "pool-1") // 调用方都是读出来再改
	now := time.Now().UTC()
	alarm := domain.Alarm{ID: "al-1", AlarmKey: "k", Severity: "warn", Type: "replication_lag", Source: "service",
		Resource: "node-2", Value: "60", Status: "active", Message: "60 秒未同步", CreatedAt: now, UpdatedAt: now}
	if err := st.Alarms().Create(ctx, alarm); err != nil {
		t.Fatal(err)
	}

	before := st.Revision()
	pool.Used, pool.Capacity = 11, 101
	if err := st.Pools().Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	alarm.Value, alarm.Message, alarm.UpdatedAt = "90", "90 秒未同步", now.Add(time.Minute)
	if err := st.Alarms().Update(ctx, alarm); err != nil {
		t.Fatal(err)
	}
	if st.Revision() != before {
		t.Fatalf("只刷新容量和告警数值推高了变更计数 %d→%d", before, st.Revision())
	}
	got, err := st.Pools().Get(ctx, "pool-1")
	if err != nil || got.Used != 11 {
		t.Fatalf("不计数也要真的写进库：%+v %v", got, err)
	}

	pool.Disks = []string{"/dev/sdb", "/dev/sdc"}
	if err := st.Pools().Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if st.Revision() == before {
		t.Fatal("池的盘变了必须计入")
	}
	before = st.Revision()
	alarm.Severity = "error"
	if err := st.Alarms().Update(ctx, alarm); err != nil {
		t.Fatal(err)
	}
	if st.Revision() == before {
		t.Fatal("告警级别变了必须计入")
	}
}
