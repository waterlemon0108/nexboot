package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tianwei/diskless/internal/api"
	"github.com/tianwei/diskless/internal/boot"
	"github.com/tianwei/diskless/internal/boot/tftpassets"
	"github.com/tianwei/diskless/internal/config"
	"github.com/tianwei/diskless/internal/control"
	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/network"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/control/platform"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
	"github.com/tianwei/diskless/internal/storage/local"
	"github.com/tianwei/diskless/internal/storage/remote"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
	"github.com/tianwei/diskless/internal/systemd"
	"github.com/tianwei/diskless/web"
)

// version 是构建版本号，由 `make build` 经 -ldflags 写入 `git describe`；
// "dev" 表示裸 `go build` 出来的，对不上任何提交。
var version = "dev"

// versionLine 是 --version 的输出，用来把磁盘上的二进制对应到构建。
func versionLine() string {
	return "ndiskless " + version
}

// versionRequested 在原始参数里识别版本查询。必须早于进程锁和配置解析：
// 服务已在运行时也要能用，而 config.Load 会拒绝不认识的参数。
func versionRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--version", "-version", "version":
		return true
	}
	return false
}

// managedUnits 是控制台可查看、可操作的 systemd 服务白名单。各发行版单元名不同，
// 都可用 env 覆盖。ndiskless 自身只读（仅看状态），免得界面切断自己的连接。
func managedUnits() []platform.ManagedUnit {
	unit := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return def
	}
	return []platform.ManagedUnit{
		// 先写能力再写单元：运维关心的是客户机能否拿到地址、连上磁盘，而不是单元的 ActiveState。
		{Key: "dnsmasq", Capability: "DHCP / PXE 引导", Label: "DNSMASQ · DHCP/TFTP/PXE",
			Unit: unit("NDISKLESS_UNIT_DNSMASQ", "dnsmasq.service"), Critical: true, ActiveOnly: true},
		// target.service 是 oneshot，开机加载 LIO 配置后即退出，它的 inactive 说明不了什么；
		// 要看内核侧在不在，由 Probe 回答。
		{Key: "iscsi", Capability: "客户机磁盘", Label: "iSCSI Target · LIO",
			Unit: unit("NDISKLESS_UNIT_ISCSI", "target.service"), Critical: true,
			OneShot: true, Probe: iscsi.Available},
		{Key: "zfs", Capability: "存储", Label: "ZFS · 存储",
			Unit: unit("NDISKLESS_UNIT_ZFS", "zfs-zed.service"), Critical: true},
		{Key: "ndiskless", Capability: "控制服务", Label: "ndiskless · 控制/引导",
			Unit: unit("NDISKLESS_UNIT_SELF", "ndiskless.service"), ReadOnly: true},
	}
}

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Println(versionLine())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-layout" {
		os.Exit(runMigrateLayout(os.Args[2:]))
	}
	cfg := config.Load(os.Args[1:])
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	releaseLock, err := ha.AcquireProcessLock(processLockPath())
	if err != nil {
		logger.Error("startup refused", "error", err)
		os.Exit(1)
	}
	defer releaseLock()
	// 单写入者闸门：单机进程全程打开；备机从一开始就关着。
	gate := ha.NewOpenGate()
	apiPort := ""
	if _, port, err := net.SplitHostPort(cfg.Addr); err == nil {
		apiPort = port
	}
	roleFile := ha.DefaultRoleFile(filepath.Dir(liveDBPath(cfg.DBDSN)))
	// 没追平就接任时留下的记录，接任后由告警读出。
	takeoverReport := filepath.Join(filepath.Dir(roleFile), "ha-takeover.json")
	// 只有本机确实在集群里（配了对端）才认持久化的角色。没有对端的节点不可能是备机，
	// 旧成员身份残留的 standby 标记不能把写入闸门永远关着。
	if rs, ok := ha.ReadRoleState(roleFile); ok && cfg.PeerURL != "" {
		// 切换过的角色靠标记跨重启保留；配置只决定从未切换过的节点的初始角色。
		cfg.Role = rs.Role
		// 标记只记得「我死时是主机」，信它之前先问集群：否则对端正在服务时，崩溃后回来的
		// 旧主会打开写入闸门、起自己的 dnsmasq，直到自愈循环把它降级，正是要防的双写。
		// 只凭确凿证据降级；对端不可达多半是机房刚来电，此时退位会让没人服务。
		if cfg.PeerURL != "" && cfg.ClusterToken != "" {
			// 问写入者而不是固定对端：writerURL 有 VIP 时就是 VIP。3 台以上集群里 NDISKLESS_PEER_URL
			// 只是某台备机，会漏掉落在别处的接任。
			peer := ha.Peer{BaseURL: writerURL(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, apiPort), Token: cfg.ClusterToken}
			role := ha.StartingRole(context.Background(), rs,
				cfg.PortalAddr != "" && holdsAddr(cfg.PortalAddr),
				[]ha.PeerStatusClient{peer})
			if role != cfg.Role {
				logger.Warn("角色标记说本机是主机，但对端正在服务且本机没有虚 IP：以备机身份启动",
					"peer", cfg.PeerURL)
				cfg.Role = role
				// 记下降级时刻：本机仍持有 VIP 的话，自愈循环下一轮会再试着激活，探不到对端时
				// 就会成功，然后又在启动核对里被降回来，每轮一次服务重启。
				demotedAt := time.Now().UTC()
				if err := ha.WriteRoleState(roleFile, ha.RoleState{
					Role: role, Epoch: rs.Epoch, SeenEpoch: rs.SeenEpoch, DemotedAt: &demotedAt,
				}); err != nil {
					logger.Warn("启动角色未能落盘，本轮仍按备机运行", "error", err)
				}
			}
		}
	}
	if cfg.Role == "standby" {
		port := ""
		if _, p, err := net.SplitHostPort(cfg.Addr); err == nil {
			port = p
		}
		gate.Close("本节点为备机", writerHint(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, port))
	}

	st, err := store.Open(context.Background(), cfg.DBDSN)
	if err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	if cfg.MigrateOnly {
		logger.Info("database migrations applied", "db_dsn", cfg.DBDSN)
		return
	}
	if err := tasks.FailInterruptedTasks(context.Background(), st, time.Now); err != nil {
		logger.Error("task recovery failed", "error", err)
		os.Exit(1)
	}

	zfsClient := zfs.New(cfg.Pool, nil, logger)
	// 池名以本机自己的记录为准（见 bindDataPool）：运维在界面建池后重启要回到那个池，
	// 而不是 env 里的 tank。之后所有用 cfg.Pool 的地方（replRoot、AdoptDataPool、nodeHealth）都跟着走。
	poolExists := func(name string) bool {
		names, err := zfsClient.ListPoolNames(context.Background())
		if err != nil {
			return false
		}
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	persisted := ""
	if ss, serr := st.SystemSettings().Get(context.Background(), domain.SystemSettingsDefaultID); serr == nil {
		persisted = ss.DataPool
	}
	dbPath := liveDBPath(cfg.DBDSN)
	bound, bindErr := bindDataPool(readDataPool(dbPath), persisted, cfg.Pool, poolExists,
		func(p string) error { return writeDataPool(dbPath, p) })
	if bindErr != nil {
		// 绑定是对的，只是没记下来；不记的话下次重启又要猜，猜错会把复制收进不存在的池。
		logger.Warn("could not record this node's data pool", "pool", bound, "error", bindErr)
	}
	if bound != cfg.Pool {
		cfg.Pool = bound
		zfsClient.SetPool(bound)
		logger.Info("using this node's own data pool", "pool", cfg.Pool)
	}
	// 必须早于任何读写数据库副本的地方：旧版本建的池还挂在 /<池名>，先搬到 /ndiskless 下。
	if moved, err := zfsClient.MigrateMountpoints(context.Background()); err != nil {
		logger.Warn("some pools are still mounted at their old paths", "moved", moved, "error", err)
	} else if len(moved) > 0 {
		logger.Info("moved pools under "+zfs.MountRoot, "pools", moved)
	}
	if err := zfsClient.EnsureLayout(context.Background()); err != nil {
		if zfs.IsNotExist(err) {
			// 还没有数据池：服务照常起，运维在界面建池时由 CreatePool 建好容器。
			logger.Warn("data pool not found; layout check deferred until a pool exists", "pool", cfg.Pool)
		} else {
			logger.Error("storage layout check failed", "error", err)
			os.Exit(1)
		}
	}
	// agent 只带节点身份；客户机连哪个地址（portal）由控制面决定，注入 BootService。
	agent := local.New(cfg.NodeID, zfsClient, iscsi.New(logger))
	// 一个密钥驱动 iSCSI CHAP 两端：导出侧给 target 上锁，启动脚本把对应登录交给客户机。
	// 为空即演示模式；设 NDISKLESS_CHAP_SECRET（各节点同值）开启，关闭时在日志里告警一次。
	chapSecret := strings.TrimSpace(os.Getenv("NDISKLESS_CHAP_SECRET"))
	agent.SetCHAPSecret(chapSecret)
	// 上次进程崩溃留下的体检、导出、适配回读临时克隆和 LVM 映射会占住数据集，挡住后续合并与删除。
	if swept, err := agent.SweepScratch(context.Background()); err != nil {
		logger.Warn("startup scratch sweep incomplete; leftovers may block merges until removed by hand", "swept", swept, "error", err)
	} else if !swept.Empty() {
		logger.Info("removed scratch leftovers from a previous run",
			"unmounted", swept.Unmounted, "mappings", swept.Mappings, "clones", swept.Clones, "snapshots", swept.Snapshots)
	}
	if !iscsi.CHAPEnabled(chapSecret) {
		logger.Warn("iSCSI CHAP off (demo mode): any host on the client network that guesses a target IQN can attach a client's disk; set NDISKLESS_CHAP_SECRET and keep clients on an isolated segment")
	}
	authService := platform.AuthService{Store: st, Secret: []byte(cfg.JWTSecret)}
	if cfg.JWTSecret == "" {
		logger.Error("jwt secret is required", "env", "NDISKLESS_JWT_SECRET")
		os.Exit(1)
	}
	if err := authService.EnsureInitialAdmin(context.Background(), cfg.BootstrapUser, cfg.BootstrapPass); err != nil {
		logger.Error("initial admin setup failed", "error", err)
		os.Exit(1)
	}
	placeRouter := &place.Router{Store: st, NodeID: cfg.NodeID, Local: agent, LocalPortal: cfg.PortalAddr, Token: cfg.ClusterToken, Logger: logger}
	bootService := control.BootService{PortalAddr: cfg.PortalAddr, Store: st, Storage: agent, Builder: boot.Builder{CHAPSecret: chapSecret}, Logger: logger, APIPort: apiPort, Place: placeRouter}
	adaptationService := adapt.AdaptationService{Store: st}
	// driverService/terminalService 在下面接线，自动适配依赖也在那里设置。
	settingsService := platform.SystemSettingsService{Store: st, FallbackImportDir: cfg.ImportDir}
	// 运维从自己机器上传镜像：分片落到导入目录，之后走原来的导入。
	// 会话编号带本机节点标识，主机切换后新主能认出「不是我的会话」并给出准确提示。
	uploadService := &assets.UploadService{
		ImportDir: settingsService.ImportDir,
		NodeID:    cfg.NodeID,
		FreeSpace: freeBytesAt,
		Now:       func() time.Time { return time.Now().UTC() },
	}
	fullSends := &ha.FullSends{}
	imageService := assets.ImageService{Store: st, Storage: agent, Async: true, ImportDir: cfg.ImportDir, ImportDirResolver: settingsService.ImportDir,
		NodeAddr: nodeSelfAddr(cfg), Replicating: fullSends.Sending,
		MountScriptBake: func(os domain.OSType) ([]byte, string) {
			if os == domain.OSTypeLinux {
				return adapt.RenderLinuxMountScript(apiPort), adapt.LinuxMountScriptVersion(apiPort)
			}
			return adapt.RenderMountScript(apiPort), adapt.MountScriptVersion(apiPort)
		}}
	configService := assets.ConfigService{Store: st, Storage: agent, Async: true, Replicating: fullSends.Sending}
	reductionService := assets.ReductionService{Store: st, Storage: agent, Async: true}
	// 每条任务记下执行节点，任务列表的「服务器」列读它。
	tasks.SetNode(hostIPForNode(cfg))
	nodeRow := domain.Server{
		ID: cfg.NodeID, Name: cfg.NodeID, IP: hostIPForNode(cfg),
		PortalIP: cfg.PortalAddr, APIURL: apiURLForNode(cfg),
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}
	poolService := ops.PoolService{Store: st, Storage: agent, Logger: logger, Async: true, Node: nodeRow,
		// 建池后不重启就绑定：持久化池名，并让运行期 zfs 客户端立刻改用新池，无池装机的节点
		// 建池后即可导入镜像、开机；/healthz 在检查时问池名，也随之跟上。
		// 已在跑的 replicator 仍持旧池名，下次重启（通常是加入集群那步）才对齐。
		Reload: func(rctx context.Context, pool string) error {
			// 记在本机而不是会复制出去的目录里：池名是每台机器自己的事。
			if err := writeDataPool(liveDBPath(cfg.DBDSN), pool); err != nil {
				return err
			}
			// 目录里那份保留：单机界面读它显示「当前数据池」，也让旧版本升上来、还没有本机记录的节点认得自己的池。
			if err := settingsService.SaveDataPool(rctx, pool); err != nil {
				return err
			}
			zfsClient.SetPool(pool)
			logger.Info("bound to the newly created pool", "pool", pool)
			return nil
		},
		// 稍等再退出，让建池任务结果先落库、日志先写完；systemd（Restart=always）会拉起。
		Restart: func() {
			go func() {
				time.Sleep(2 * time.Second)
				logger.Info("restarting to use the new data pool")
				os.Exit(0)
			}()
		}}
	if err := ops.EnsureLocalServer(context.Background(), st, nodeRow); err != nil {
		// 故意不致命：这行只是池归属和集群花名册的账，单机功能都不依赖它。
		// 退出会把记账问题变成 systemd 最终放弃的崩溃循环（如 machine-id 重置后的
		// "UNIQUE constraint failed: servers.ip"）。报错、继续服务，让运维修这行。
		logger.Error("节点身份登记失败：集群功能（放置、复制、节点列表）可能异常，"+
			"单机功能不受影响。请检查 servers 表中是否有占用本机地址的过期记录",
			"error", err, "node_id", cfg.NodeID, "ip", nodeRow.IP)
	}
	// 产品安装前就建好的池也是本机的池：要登记，否则一边用它服务镜像一边报没有存储。
	if err := poolService.AdoptDataPool(context.Background()); err != nil {
		logger.Warn("could not register the data pool", "pool", cfg.Pool, "error", err)
	}
	userService := platform.UserService{Store: st}
	driverService := adapt.DriverService{Store: st, Dir: cfg.DriverDir}
	backupService := ops.BackupService{Store: st, Storage: agent}
	serverService := platform.ServiceService{Client: systemd.Client{}, Units: managedUnits(), Version: version}
	auditService := platform.AuditService{Store: st}
	reconcileService := ops.ReconcileService{Store: st, Storage: agent, Logger: logger}
	// 池比数据库活得久（重装、丢库文件），里面的数据仍属本机。能从池里描述的就重建；
	// 描述不了的（数据集已不在的行）保持原样，由下面的一致性告警报告。
	if report, err := reconcileService.RecoverCatalogue(context.Background()); err != nil {
		logger.Warn("could not rebuild the catalogue from the pool", "error", err)
	} else if report.Images > 0 || report.Configs > 0 || report.Reductions > 0 {
		logger.Warn("recovered catalogue entries from the pool", "recovered", report.String())
	}
	// 要先于 dnsmasq 接线声明：告警规则经它读取客户机网络，指针在下面才填上。
	var networkService network.Service
	replRoot := cfg.Pool + "/" + storage.CatalogueRoot
	replicator := &ops.Replicator{
		Store: st, ZFS: zfsClient, Gate: gate, Root: replRoot,
		DBSnapshot: st.Snapshot, DBRevision: st.Revision,
		Now: time.Now, Logger: logger, LiveDBPath: liveDBPath(cfg.DBDSN), SelfURL: selfAPIURL(cfg)}
	if cfg.PortalAddr != "" && cfg.PortalAddr != nodeSelfAddr(cfg) {
		// 复制源是 VIP：VIP 在本机时向它拉就是向自己拉。
		replicator.HoldsVIP = func() bool { return holdsAddr(cfg.PortalAddr) }
	}
	replPeer, replSource := replicationPeer(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, apiPort, cfg.ClusterToken, selfAPIURL(cfg))
	if replSource != "" {
		replicator.Peer = replPeer
		replicator.PeerURL = replSource
	}
	// 池记录随目录复制，每台都列出全部池，但容量和健康只有导入池的那台读得到；
	// 只有问各节点自己的池，控制台才能给别台的存储显示真实数字而不是「未知」。
	nodePools := func(ctx context.Context) (ops.PoolListResult, error) {
		return poolService.List(ctx, cfg.NodeID)
	}
	clusterPools := platform.ClusterPools{
		Store: st, NodeID: cfg.NodeID, Token: cfg.ClusterToken,
		Now: func() time.Time { return time.Now().UTC() },
		Local: func(ctx context.Context) ([]ops.PoolItem, error) {
			res, err := nodePools(ctx)
			return res.Items, err
		},
	}
	// 被保留的目录副本在保住它的那台上，告警只在写入者评估，所以由写入者挨个问。
	clusterPreserved := platform.ClusterPreserved{
		Store: st, NodeID: cfg.NodeID, Token: cfg.ClusterToken,
		Now:   func() time.Time { return time.Now().UTC() },
		Local: agent.PreservedCopies,
	}
	alarmService := platform.AlarmService{Store: st, Logger: logger, Rules: platform.DefaultAlarmRules(poolService, serverService, backupService, reconcileService, st, platform.AlarmThresholds{}, platform.NetworkAlarmInputs{
		Client: func(ctx context.Context) (assets.ClientNetwork, error) { return networkService.Client(ctx) },
		Reachable: func(ctx context.Context, ip string) bool {
			res, err := networkService.Probe(ctx, network.ProbeRequest{IP: ip})
			return err == nil && res.Reachable
		},
	})}
	if cfg.PeerURL != "" {
		alarmService.Rules = append(alarmService.Rules, platform.ReplicationAlarmRule(replicator.Status, 5*time.Minute))
	}
	// 备机的池由备机自己回答。必须在 sweeper 启动前追加：RunSweeper 绑定的是副本。
	alarmService.Rules = append(alarmService.Rules, platform.PeerPoolAlarmRule(clusterPools.List, st, platform.AlarmThresholds{}))
	alarmService.Rules = append(alarmService.Rules, platform.PreservedCopyAlarmRule(clusterPreserved.List))
	if cfg.PeerURL != "" {
		alarmService.Rules = append(alarmService.Rules, platform.TakeoverAlarmRule(
			func() (ha.TakeoverReport, bool) { return ha.ReadTakeoverReport(takeoverReport) },
			func() time.Time { return time.Now().UTC() }))
	}
	const tftpRoot = "/srv/tftp"
	if err := tftpassets.WriteTo(tftpRoot); err != nil {
		logger.Error("tftp boot file setup failed", "error", err)
		os.Exit(1)
	}
	// dnsmasq 基础配置（监听方式）和分组文件放一起管理：00- 排在最前，先于分组被读取。
	dnsmasqBaseConf := filepath.Join(filepath.Dir(cfg.DnsmasqConf), "00-ndiskless-base.conf")
	dhcpManager := dhcp.Manager{
		Path:        cfg.DnsmasqConf,
		BasePath:    dnsmasqBaseConf,
		Logger:      logger,
		RestartGate: gate.Allow,
		Renderer:    dhcp.Renderer{TFTPRoot: tftpRoot, BootFile: "undionly.kpxe", EFIBootFile: "snponly.efi", BootURL: bootURLWithHints(cfg.BootURL)},
	}
	networkService = network.Service{Store: st, BootHost: storageServerFromBootURL(cfg.BootURL), BaseConfPath: dnsmasqBaseConf, Base: dhcpManager, Logger: logger}
	groupService := assets.GroupService{Store: st, DHCP: dhcpManager, Logger: logger, LocalAddrs: localIPv4Addrs, Network: networkService.Client}
	groupDiskService := assets.GroupDiskService{Store: st}
	terminalService := assets.TerminalService{Store: st, DHCP: dhcpManager, Storage: agent, Logger: logger, BackupTrigger: backupService.Trigger, Async: true, Place: placeRouter}
	// 驱动注入：离线把驱动包和开机安装脚本放进超管克隆，运维随后用 StopSuper 保存结果。
	adaptationService.Storage = agent
	adaptationService.Place = placeRouter
	adaptationService.Logger = logger
	adaptationService.APIPort = apiPort
	adaptationService.BundleZip = func(ctx context.Context, bundleID string) ([]byte, error) {
		_, data, err := driverService.BundleArchive(ctx, bundleID)
		return data, err
	}
	if err := networkService.SyncBaseAtStartup(context.Background()); err != nil {
		logger.Error("dnsmasq base config sync failed; dnsmasq keeps listening the way it was configured", "error", err)
	}
	syncDHCPAtStartup(context.Background(), groupService.SyncDHCP, logger)
	// 克隆带着镜像的 LVM 卷组，宿主机不能激活它们。
	if changed, err := local.EnsureZvolLVMFilter("/etc/lvm/lvm.conf"); err != nil {
		logger.Warn("lvm.conf not given the zvol filter; add global_filter = [ \"r|^/dev/zd.*|\" ] by hand", "error", err)
	} else if changed {
		logger.Info("lvm.conf: added global_filter excluding zvols")
	}
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	// 在线状态以实时 iSCSI 会话为准（无需客户机内代理），心跳接口只是辅助信号。
	// 读会话列表只是扫 configfs，间隔短也没成本；对账器对空读去抖（offlineConfirmations），5s 周期不会抖动。
	// 这些 sweeper 会改数据，备机上由闸门让它们闲置；gatedLoop 每个周期复查，闸门打开后一个周期内生效。
	gatedLoop := func(interval time.Duration, run func(context.Context, time.Duration)) {
		go func() {
			for {
				if sweepCtx.Err() != nil {
					return
				}
				if gate.Allow() == nil {
					run(sweepCtx, interval)
					return
				}
				select {
				case <-sweepCtx.Done():
					return
				case <-time.After(interval):
				}
			}
		}()
	}
	gatedLoop(5*time.Second, terminalService.RunOnlineReconciler)
	// 放置计数只增不减（关机、掉线、回收都不经过放置路径），不定期拉回真实值会越来越偏；
	// 用在线对账已采集的会话数校准。
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				actual, err := terminalService.ClientsPerNode(sweepCtx)
				if err != nil {
					continue
				}
				placeRouter.ReconcileLoad(actual)
			}
		}
	}()
	// 否则临时克隆要等它的客户机下次开机才清，轮换使用的机房每台机器积一个死数据集。
	// 回收比对账慢得多：不急（宽限期远大于周期），每次回收要拆 configfs 再 zfs destroy。
	gatedLoop(time.Minute, terminalService.RunCloneReclaimer)
	// 只有写入者决定何时备份一轮并打标记，标记随复制到达各节点。
	gatedLoop(time.Minute, backupService.RunScheduler)
	// 送副本则是每个节点自己的事，写入者也一样，所以不过闸门：备机有完整目录副本和
	// 自己的备份池，能维持自己不断的备份链，切换时备份无需迁移或重来。
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if shipped, _, err := backupService.DrainLocal(sweepCtx); err != nil {
					logger.Warn("backup copy not shipped", "error", err)
				} else if shipped {
					logger.Info("backup copy updated")
				}
			}
		}
	}()
	gatedLoop(30*time.Second, alarmService.RunSweeper)
	// 两个复制循环都常驻，由闸门决定哪个生效。单机无对端时备机循环空转，
	// 主机循环只在有对端可用时才打标记，不产生多余快照。
	if replSource != "" {
		go replicator.RunPrimary(sweepCtx, cfg.ReplicationInterval)
		go replicator.RunStandby(sweepCtx, cfg.ReplicationInterval)
	}
	// 进程已就绪：带它来到这里的切换已结束，「正在以新角色重启」标记要清掉，
	// 否则之后的崩溃会被过期的标记掩盖。
	const switchFile = "/run/ndiskless.switching"
	ha.ClearSwitching(switchFile)

	health := nodeHealth(st, zfsClient, cfg.Role, func() string { return readDataPool(liveDBPath(cfg.DBDSN)) })
	haController := &ha.Controller{
		NodeID:       cfg.NodeID,
		RoleFile:     roleFile,
		DefaultRole:  cfg.Role,
		StepDownFile: "/run/ndiskless.step-down",
		SwitchFile:   switchFile,
		// 放 /run：让出过的记忆只该活到这次开机为止。
		YieldFile:  "/run/ndiskless.yielded",
		ReportFile: takeoverReport,
		Gate:       gate,
		Exit: func(code int) {
			// systemd 会重启服务，由标记决定新角色。
			logger.Info("restarting to switch roles")
			cancelSweep()
			st.Close()
			os.Exit(code)
		},
		Logger: logger,
		// 接任前要比两样：本机目录收到了哪一轮（没收完的续传不算），以及本机是否健康。
		CatalogueMark: func(ctx context.Context) string {
			if _, token, err := zfsClient.ResumeToken(ctx, replRoot); err == nil && token != "" {
				return ""
			}
			inv, err := zfsClient.ListGUIDs(ctx, replRoot)
			if err != nil {
				return ""
			}
			// 写入者的递归快照是原子的，根上最新一轮即完整；收来的副本则不然，
			// 递归流失败时根最先落地，配置可能还停在上一轮。
			if gate.Allow() == nil {
				return ops.WriterMarker(inv, replRoot)
			}
			return ops.CompleteMarker(inv, replRoot)
		},
		Healthy: func(ctx context.Context) bool { return health(ctx) == nil },
		CatalogueIncomplete: func(ctx context.Context) []string {
			copyDir, err := zfsClient.EnsureDBCopyDataset(ctx)
			if err != nil {
				return nil
			}
			return dbCopyMissing(ctx, filepath.Join(copyDir, "ndiskless.db"), agent, logger)
		},
		// 用复制来的库副本覆盖在用库，只在备机激活时执行。从没收到副本就拒绝，
		// 好过在装满镜像的池旁边以空目录起服务。
		PrepareActiveDB: func(ctx context.Context) error {
			copyDir, err := zfsClient.EnsureDBCopyDataset(ctx)
			if err != nil {
				return fmt.Errorf("%w（副本数据集不可用: %v）", ha.ErrNoDBCopy, err)
			}
			src := filepath.Join(copyDir, "ndiskless.db")
			data, err := os.ReadFile(src)
			if err != nil {
				return ha.ErrNoDBCopy
			}
			return ha.ReplaceLiveDB(liveDBPath(cfg.DBDSN), data)
		},
		// 计划切换前先排空：打一轮标记，等备机拉走（从 primary-serve 行观察）。
		SyncBeforeSwitch: func(ctx context.Context) error {
			if cfg.PeerURL == "" {
				return fmt.Errorf("未配置对端（NDISKLESS_PEER_URL），无法计划切换")
			}
			// 自己打一轮作目标并暂停后台打标记，否则取样后、退位前冒出的新一轮谁也收不到。
			// 退位会重启进程，暂停随之消失；排空失败则立即恢复。
			latest, err := replicator.MarkForSwitch(ctx, 5*time.Minute)
			if err != nil {
				replicator.ResumeMarking()
				return err
			}
			drained := false
			defer func() {
				if !drained {
					replicator.ResumeMarking()
				}
			}()
			// 等所有存活备机都追上，而不是第一个：VIP 交给谁由 VRRP 优先级决定，不在本进程控制内，
			// 任何候选者持旧副本接任都会丢掉它漏的轮次。几分钟没拉取的备机不等，它怎样都接不干净。
			alive := 3 * cfg.ReplicationInterval
			if alive < 3*time.Minute {
				alive = 3 * time.Minute
			}
			deadline := time.Now().Add(3 * time.Minute)
			var lagging []string
			for time.Now().Before(deadline) {
				rows, err := st.ReplicationStates().List(ctx)
				if err != nil {
					return err
				}
				// 是「追到或越过」而非「正好等于」：主机每轮都打新标记，备机拉的是当时最新的，很容易越过采样目标。
				lagging = ops.LaggingPullers(rows, latest, time.Now().UTC(), alive, selfAPIURL(cfg))
				if len(lagging) == 0 {
					drained = true
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(3 * time.Second):
				}
			}
			if len(lagging) == 1 && lagging[0] == "" {
				return fmt.Errorf("没有正在拉取目录的备机，无人可接管（最新一轮 %s）", latest)
			}
			return fmt.Errorf("备机 3 分钟内未拉取最新目录（%s）：%s，请检查它们的复制状态", latest, strings.Join(lagging, "、"))
		},
	}
	if cfg.PeerURL != "" && cfg.ClusterToken != "" {
		haController.Peer = ha.Peer{BaseURL: cfg.PeerURL, Token: cfg.ClusterToken}
	}
	if replSource != "" {
		// 接任前追平直接找对端自己的地址拉：平时复制源是 VIP，而此刻 VIP 就在本机，问它等于问自己。
		haController.CatchUp = func(ctx context.Context, from ha.PeerStatusClient, target string) error {
			peer, ok := from.(ha.Peer)
			if !ok {
				return fmt.Errorf("对端不支持直接拉取目录")
			}
			peer.Self = selfAPIURL(cfg)
			return catchUpTo(ctx, target,
				func(ctx context.Context) error { return replicator.PullFrom(ctx, peer, peer.BaseURL) },
				haController.CatalogueMark, 5*time.Second)
		}
		// 交接中「交出去」的一半：关写入之后打最后一轮。
		haController.StampFinalRound = replicator.MarkForSwitch
		haController.ResumeRounds = replicator.ResumeMarking
	}
	// HoldsVIP 让对端的激活栅栏区分「真在服务」和「角色文件还没改过来」。
	// 下面的 ClusterPeers 是激活守卫的花名册：其它每个节点按其自身地址拨（不用 VIP，切换时
	// VIP 正是争议所在），两项以上即按多数裁决；各节点心跳上报 epoch，对端死掉也知道栅栏抬到多高。
	haController.HoldsVIP = func() bool { return holdsAddr(cfg.PortalAddr) }
	serverService.Role = func() string { return haController.State().Role }
	clusterServices := platform.ClusterServices{
		Store: st, Local: serverService.ListServices, NodeID: cfg.NodeID,
		SelfRole: func() string { return haController.State().Role },
		Token:    cfg.ClusterToken, Now: func() time.Time { return time.Now().UTC() },
	}
	// 每个节点在自己池上留备份副本，所以「最近一次备份」每台机器各有答案。
	clusterBackups := platform.ClusterBackups{
		Store: st, Local: backupService.LocalStatus, NodeID: cfg.NodeID,
		Token: cfg.ClusterToken, Now: func() time.Time { return time.Now().UTC() },
	}
	// 从控制台重启别台的服务，比 SSH 上去强，而出问题时恰恰最难 SSH。
	clusterOps := platform.ClusterServiceOps{
		Store: st, NodeID: cfg.NodeID, Token: cfg.ClusterToken,
		Now:         func() time.Time { return time.Now().UTC() },
		LocalAction: serverService.ServiceAction,
		LocalLogs:   serverService.ServiceLogs,
	}
	// 池写入在磁盘所在节点执行，转发过去控制台才能操作任意节点的存储。
	clusterDisks := platform.ClusterDisks{
		Store: st, NodeID: cfg.NodeID, Token: cfg.ClusterToken, Local: poolService.ListDisks,
	}
	clusterProxy := platform.ClusterProxy{
		Store: st, NodeID: cfg.NodeID, Token: cfg.ClusterToken, SelfURL: selfAPIURL(cfg),
	}
	haController.RecordEpoch = func(ctx context.Context, epoch int64) {
		srv, err := st.Servers().Get(ctx, cfg.NodeID)
		if err != nil {
			return
		}
		srv.Epoch = epoch
		srv.HAState = "active"
		if err := st.Servers().Update(ctx, srv); err != nil {
			logger.Warn("epoch 未能立即落库，将由下一次心跳补上", "error", err)
		}
	}
	haController.KnownEpoch = func(ctx context.Context) int64 {
		servers, err := st.Servers().List(ctx)
		if err != nil {
			return 0
		}
		var highest int64
		for _, s := range servers {
			if s.Epoch > highest {
				highest = s.Epoch
			}
		}
		return highest
	}
	haController.ClusterPeers = func(ctx context.Context) []ha.PeerStatusClient {
		servers, err := st.Servers().List(ctx)
		if err != nil {
			return nil
		}
		var out []ha.PeerStatusClient
		for _, s := range servers {
			if s.ID == cfg.NodeID || s.APIURL == "" {
				continue
			}
			out = append(out, ha.Peer{BaseURL: s.APIURL, Token: cfg.ClusterToken})
		}
		return out
	}
	clusterRegistry := ops.ClusterRegistry{Store: st, Logger: logger, SelfNodeID: cfg.NodeID,
		Now: func() time.Time { return time.Now().UTC() }, LivePools: clusterPools.NodePoolNames}
	// 注册与花名册同步走 VIP：它是唯一永远指向当前写入者的地址。指向固定机器时，那台一停
	// 或变成备机，本节点就从花名册消失，客户机随之被判离线、重新放置（见 registrationTarget）。
	clusterPeer := ha.Peer{
		BaseURL: writerURL(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, apiPort),
		Token:   cfg.ClusterToken,
	}
	// 没人续传的分片会占满导入目录所在的盘，而那块盘正是运行中的克隆要用的；一天没动静就收走。
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if n := uploadService.Reap(sweepCtx, 24*time.Hour); n > 0 {
					logger.Info("清理了没人续传的上传分片", "count", n)
				}
			}
		}
	}()

	// 以备机身份持有 VIP 是自相矛盾的，别处不会解决：keepalived 只在边沿通知一次，一次激活
	// 失败就留下持有 VIP、只读的节点，集群没有写入者直到有人发现。在这里对账。
	if cfg.PeerURL != "" && cfg.PortalAddr != "" {
		go func() {
			reconcile := func() {
				if err := haController.ReconcileRole(sweepCtx, holdsAddr(cfg.PortalAddr)); err != nil {
					logger.Warn("自愈激活失败，下一轮重试", "error", err)
				}
			}
			// 启动时先探一次对端，只为记住集群到过的 epoch，不做角色决定。等满 15 秒才学的话，
			// 比 tick 更快的连续切换会让新主拿过期下限激活，两任主机共用同一 epoch，栅栏判不出真假。
			// 此刻也不能裁决角色：keepalived 还没配上 VIP，刚起来的主机会据「没 VIP、对端在服务」把自己退掉。
			haController.ObservePeers(sweepCtx)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-sweepCtx.Done():
					return
				case <-ticker.C:
					reconcile()
				}
			}
		}()
	}

	// 心跳带上本机有哪些池：池记录写在建池那台的库里，备机的库留不住，由写入者按上报对齐目录。
	// 读不全就不报，免得按残缺清单删记录。
	poolReport := func(ctx context.Context) *ha.PoolReport {
		names, err := zfsClient.ProductPoolNames(ctx)
		if err != nil {
			return nil
		}
		report, err := poolService.PoolReport(ctx, names)
		if err != nil {
			return nil
		}
		return &report
	}
	go func() {
		// 本节点的脉搏。写入者：本地刷新自己的行（portal 用 VIP，客户机拨的就是它）。
		// 其余节点：把同样的注册发给 VIP（加入与心跳是同一个调用），并拉回花名册供日后激活用。
		// 有对端时用 10s：节点靠它得知别台的 epoch，一个周期内发生的接任会读到过期上限，栅栏方向就反了。
		interval := 30 * time.Second
		if cfg.PeerURL != "" {
			interval = 10 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				rs := haController.State()
				pools := poolReport(sweepCtx)
				if gate.Allow() == nil {
					self := ha.NodeInfo{NodeID: cfg.NodeID, APIURL: selfAPIURL(cfg), PortalIP: cfg.PortalAddr, Role: rs.Role, Epoch: rs.Epoch, Pools: pools}
					if err := clusterRegistry.Register(sweepCtx, self); err != nil {
						logger.Warn("server heartbeat write failed", "error", err)
					}
					continue
				}
				if cfg.PeerURL == "" || cfg.ClusterToken == "" {
					continue
				}
				self := ha.NodeInfo{NodeID: cfg.NodeID, APIURL: selfAPIURL(cfg), PortalIP: nodeSelfAddr(cfg), Role: rs.Role, Epoch: rs.Epoch, Pools: pools}
				hctx, hcancel := context.WithTimeout(sweepCtx, 10*time.Second)
				if err := clusterPeer.RegisterSelf(hctx, self); err != nil {
					logger.Warn("cluster register failed", "error", err)
				} else if roster, err := clusterPeer.ClusterPeers(hctx); err == nil {
					if err := ops.SyncPeersIntoStore(hctx, st, cfg.NodeID, roster, time.Now().UTC()); err != nil {
						logger.Warn("peer roster sync failed", "error", err)
					}
				}
				hcancel()
			}
		}
	}()

	if cfg.Role == "standby" {
		// 备机不服务磁盘也不提供 DHCP：第二个权威 dnsmasq 会 NAK 主机的客户机。
		// RestartGate 只阻止本进程重启它；停掉单元是本节点自己的责任，notify 脚本只是触发者。
		if out, err := (systemd.ExecRunner{}).Run(context.Background(), "systemctl", "stop", "dnsmasq"); err != nil {
			logger.Warn("standby quiesce: dnsmasq stop failed", "error", err, "output", string(out))
		}
		// 备机不服务磁盘：之前角色（或崩溃）留下的导出和克隆现在清掉，赶在任何连接之前。
		// 包括超管克隆，原因见 quiesceStandby。
		quiesceStandby(context.Background(), iscsi.New(logger).TeardownAll, agent, logger.Warn)
	} else if cfg.PeerURL != "" {
		// 与备机停 dnsmasq 对称的另一半。HA 下该单元是 disabled 的，防止备机自己起第二个权威 DHCP，
		// 也就意味着开机没人启动它；不在这里拉起，重启后的主机 API 正常而客户机全都拿不到 DHCP。
		// 本节点若不是写入者，内部闸门会让它什么都不做。
		if err := dhcpManager.EnsureRunning(context.Background()); err != nil {
			logger.Warn("active startup: dnsmasq start failed", "error", err)
		}
	}
	if cfg.PeerURL != "" && cfg.ClusterToken != "" {
		// keepalived 的单播 peer 列表跟随花名册：增删节点几轮内在每个成员上生效，无需手改，
		// reload 也不丢已持有的 VIP。否则主机不会向没告知过的节点发通告，那个节点会选自己当主。
		reconciler := ha.PeerReconciler{
			ConfPath: keepalivedConfPath, Self: nodeSelfAddr(cfg), Logger: logger,
			Roster: func(ctx context.Context) ([]string, error) {
				roster, err := clusterRegistry.Peers(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]string, 0, len(roster))
				for _, n := range roster {
					out = append(out, n.Addr())
				}
				return out, nil
			},
			Reload: func(ctx context.Context) error {
				// 正在加入的节点有文件但还没起守护进程（见 JoinFinisher），启动时会读到新文件。
				sc := systemd.Client{}
				if st, err := sc.Show(ctx, "keepalived"); err == nil && st.Active != "active" {
					return nil
				}
				return sc.Action(ctx, "keepalived", "reload")
			},
		}
		go reconciler.Run(sweepCtx, 15*time.Second)
		// 界面加入会写好 keepalived 配置但保持停止，直到写入者把本节点列为 VRRP peer；标记就表示这个待定状态。
		finisher := ha.JoinFinisher{
			Marker: keepalivedPendingMarker, Self: nodeSelfAddr(cfg), Logger: logger,
			Writer: func(ctx context.Context) (ha.PeerStatus, error) {
				return ha.Peer{BaseURL: writerURL(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, apiPort), Token: cfg.ClusterToken}.Status(ctx)
			},
			Enable: func(ctx context.Context) error {
				out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "keepalived").CombinedOutput()
				if err != nil {
					return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
				}
				return nil
			},
		}
		go finisher.Run(sweepCtx, 5*time.Second)
	}
	clusterJoin := ha.JoinService{
		AlreadyClustered: func() bool { return strings.TrimSpace(cfg.PeerURL) != "" },
		HasPool: func(ctx context.Context) bool {
			names, err := zfsClient.ListPoolNames(ctx)
			if err != nil {
				return false
			}
			want := zfsClient.PoolName()
			for _, n := range names {
				if n == want {
					return true
				}
			}
			return false
		},
		LocalPool: zfsClient.PoolName,
		APIPort:   apiPort,
		FetchConfig: func(ctx context.Context, vipBase, token string) (ha.JoinConfig, error) {
			return ha.Peer{BaseURL: vipBase, Token: token}.JoinConfig(ctx)
		},
		ResolveSelfAddr: resolveSelfAddr,
		Reconfigure:     reconfigureForCluster(logger),
	}
	hasLocalPool := clusterJoin.HasPool
	clustered := clusterJoin.AlreadyClustered
	// 出厂态：没有数据池且不在任何集群里。两条都成立时本机按定义没有数据，
	// 集群可以直接纳管它，运维不必去它的界面上转述 VIP 和令牌。
	factoryState := func(ctx context.Context) bool { return !clustered() && !hasLocalPool(ctx) }
	clusterCreate := ha.CreateService{
		AlreadyClustered: clustered,
		LocalPool:        zfsClient.PoolName,
		OnLocalSubnet:    localSubnetHas,
		ResolveSelfAddr:  resolveSelfAddr,
		AddressInUse:     func(ctx context.Context, ip string) []string { return assets.AddressUsers(ctx, st, ip) },
		NewToken:         randomHex32,
		Reconfigure:      reconfigureForCluster(logger),
	}
	adoptOff := envBool("NDISKLESS_ADOPT_OFF")
	adoptHandler := ha.AdoptHandler{
		NodeID: cfg.NodeID, Version: version, Disabled: adoptOff,
		Factory: factoryState,
		Adopt:   clusterJoin.Adopt,
	}
	discovery := ha.Discovery{
		VIP: cfg.PortalAddr, Token: cfg.ClusterToken,
		Candidates: func(context.Context) []string {
			return ha.SubnetCandidates(localSubnetCIDR(nodeSelfAddr(cfg)), apiPort,
				map[string]bool{nodeSelfAddr(cfg): true, cfg.PortalAddr: true})
		},
		InCluster: func(nodeID string) bool {
			peers, err := clusterRegistry.Peers(context.Background())
			if err != nil {
				return false
			}
			for _, n := range peers {
				if n.NodeID == nodeID {
					return true
				}
			}
			return false
		},
	}
	router := api.NewRouter(logger, web.Dist(), api.Services{Gate: gate, Health: health,
		HAHandler:      ha.ControlHandler{Controller: haController, NodeID: cfg.NodeID, Token: cfg.ClusterToken, VRRPPeers: liveVRRPPeers},
		ClusterHandler: ha.ClusterHandler{Registry: clusterRegistry, Token: cfg.ClusterToken, JWTSecret: cfg.JWTSecret, CHAPSecret: chapSecret, PoolName: zfsClient.PoolName},
		ClusterJoin:    clusterJoin,
		BootReadProxy:  forwardBootReadToWriter(cfg, apiPort, logger),
		ClusterCreate:  clusterCreate,
		AdoptHandler:   adoptHandler,
		ClusterDiscover: func(ctx context.Context) ([]ha.Candidate, error) {
			return discovery.Scan(ctx)
		},
		ClusterAdoptNode: func(ctx context.Context, addr string) error {
			return discovery.AdoptNode(ctx, addr)
		},
		ClusterNodes: clusterRegistry.Nodes,
		ForgetNode:   clusterRegistry.Forget,
		ClusterToken: cfg.ClusterToken,
		NodeServices: serverService.ListServices,
		NodePools:    nodePools,
		NodeDisks:    poolService.ListDisks,
		ClusterDisks: clusterDisks.List,
		// 同一对接口的对端一侧：只在调用方点名本机时才执行。
		NodeServiceAction: func(ctx context.Context, want, key, action string) error {
			if err := platform.AssertSelf(cfg.NodeID, want); err != nil {
				return err
			}
			return serverService.ServiceAction(ctx, key, action)
		},
		NodeServiceLogs: func(ctx context.Context, want, key string, lines int) (string, error) {
			if err := platform.AssertSelf(cfg.NodeID, want); err != nil {
				return "", err
			}
			return serverService.ServiceLogs(ctx, key, lines)
		},
		NodePoolWrites: true,
		NodeID:         cfg.NodeID,
		ClusterProxy: func(ctx context.Context, node, method, suffix, query string, body io.Reader) (api.ProxyResponse, error) {
			res, err := clusterProxy.Forward(ctx, node, method, suffix, query, body)
			return api.ProxyResponse{Status: res.Status, Body: res.Body, ContentType: res.ContentType}, err
		},
		ClusterServiceAction: clusterOps.Action,
		ClusterServiceLogs:   clusterOps.Logs,
		ClusterPools: func(ctx context.Context) (platform.ClusterPoolsResult, error) {
			return clusterPools.List(ctx)
		},
		ClusterServices: func(ctx context.Context) (platform.ClusterServicesResult, error) {
			return clusterServices.List(ctx)
		},
		ClusterBackups: func(ctx context.Context) (platform.ClusterBackupsResult, error) {
			return clusterBackups.List(ctx)
		},
		NodeBackup:     backupService.LocalStatus,
		NodePreserved:  agent.PreservedCopies,
		NodeTasks:      func(ctx context.Context) ([]domain.Task, error) { return platform.NodePoolTasks(ctx, st) },
		StorageHandler: remote.Handler{Agent: agent, Token: cfg.ClusterToken},
		HA: ha.StatusService{Controller: haController, NodeID: cfg.NodeID, PeerURL: cfg.PeerURL, Peer: haController.Peer,
			Rate: func(ctx context.Context) (int, string, error) {
				return settingsService.ReplicationRate(ctx, cfg.ReplicationRateMBPerSec)
			},
			SaveRate: func(ctx context.Context, mbps int) error {
				return settingsService.SaveReplicationRate(ctx, mbps)
			}},
		ReplicationHandler: ha.ReplicationHandler{Source: zfsClient, Root: replRoot, Token: cfg.ClusterToken, FullSends: fullSends,
			// 备机据此判断是否跟随：本机库还记着、池里已没有的数据集，跟随过去会从全集群一并删掉。直接复用体检结果。
			Incomplete: func(ctx context.Context) []string {
				report, err := reconcileService.Check(ctx)
				if err != nil {
					return nil // 算不出来就不声张，别把读不到说成不一致
				}
				var missing []string
				for _, issue := range report.Issues {
					if issue.Kind == ops.IssueMissingDataset {
						missing = append(missing, issue.Ref)
					}
				}
				return missing
			},
			// 每次开流时读取，运维在运行期调整（维护窗口「解除限速」）无需重启即生效。
			RateMBPerSec: func() int {
				mbps, _, err := settingsService.ReplicationRate(context.Background(), cfg.ReplicationRateMBPerSec)
				if err != nil {
					return cfg.ReplicationRateMBPerSec
				}
				return mbps
			},
			// 每次服务拉取都是备机存活且在跟随的证据，本节点的滞后告警读它。
			// 按拉取方每台备机一行：多台备机时只有一行就只能说明「有人追上了」，计划切换会排空错的那台。
			OnServed: func(from, to string) {
				now := time.Now().UTC()
				target := from
				if target == "" {
					target = cfg.PeerURL
				}
				row := domain.ReplicationState{
					Target: target, Kind: "primary-serve", Root: replRoot,
					LastSnapshot: to, LastOKAt: &now, UpdatedAt: now,
				}
				if to == "" { // 清单轮询：只算心跳
					if rows, err := st.ReplicationStates().List(context.Background()); err == nil {
						for _, prev := range rows {
							if prev.Target == target && prev.Kind == "primary-serve" {
								row.LastSnapshot = prev.LastSnapshot
							}
						}
					}
				}
				_ = st.ReplicationStates().Upsert(context.Background(), row)
			}},
		Replication: replicator,
		Boot:        bootService, BootPeerCheckOff: envBool("NDISKLESS_BOOT_PEER_CHECK_OFF"), Auth: authService, Images: imageService, Uploads: uploadService, Configs: configService, Reductions: reductionService, Groups: groupService, GroupDisks: groupDiskService, Terminals: terminalService, Pools: poolService, Users: userService, Backups: backupService, Consistency: reconcileService, Settings: settingsService, Network: networkService, Tasks: platform.ClusterTasks{Store: st, Local: imageService, NodeID: cfg.NodeID, Token: cfg.ClusterToken}, Drivers: driverService, Adaptations: adaptationService, Server: serverService, Audit: auditService, Alarms: alarmService, Runtime: api.RuntimeConfig{ImportDir: cfg.ImportDir}})
	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
	}

	logger.Info("ndiskless starting",
		"addr", cfg.Addr,
		"db_dsn", cfg.DBDSN,
		"pool", cfg.Pool,
		"fallback_import_dir", cfg.ImportDir,
		"effective_import_dir", effectiveImportDir(context.Background(), settingsService, cfg.ImportDir),
		"dnsmasq_conf", cfg.DnsmasqConf,
		"workers", cfg.Workers,
	)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server stopped", "error", err)
		os.Exit(1)
	}
}

// bootURLWithHints 在 iPXE chainload URL 上追加 L1 硬件画像参数，运维已自定义过则不动。
// 不会展开变量的老 iPXE 只会发空值，启动不受这些参数影响。
func bootURLWithHints(raw string) string {
	if raw == "" || strings.Contains(raw, "platform=") {
		return raw
	}
	return raw + "&platform=${platform}&busid=${net0/busid}&manufacturer=${manufacturer:uristring}&product=${product:uristring}"
}

func storageServerFromBootURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func effectiveImportDir(ctx context.Context, settings platform.SystemSettingsService, fallback string) string {
	dir, err := settings.ImportDir(ctx)
	if err != nil || strings.TrimSpace(dir) == "" {
		return fallback
	}
	return dir
}

// holdsAddr 判断该地址当前是否配在本机上，节点靠它区分「keepalived 把 VIP 给了我」和「VIP 在对端」。
func holdsAddr(want string) bool {
	target, err := netip.ParseAddr(want)
	if err != nil {
		return false
	}
	addrs, err := localIPv4Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a == target {
			return true
		}
	}
	return false
}

func localIPv4Addrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err != nil {
			continue
		}
		out = append(out, prefix.Addr())
	}
	return out, nil
}

// syncDHCPAtStartup 按数据库写出 dnsmasq 配置。失败只报错不退出：出错的是库里某一行，
// 退出就没有界面和 API 去改它了。iSCSI 启动不经过 dnsmasq，已登记的机器照旧按旧配置启动，
// 下次编辑分组或客户机时会重新同步。
func syncDHCPAtStartup(ctx context.Context, sync func(context.Context) error, logger *slog.Logger) {
	if err := sync(ctx); err != nil {
		logger.Error("initial dnsmasq sync failed; dnsmasq keeps its previous config until a group or terminal is edited",
			"error", err)
	}
}

// runMigrateLayout 是离线的 layout-v2 迁移：把顶层散落的数据集改名到 nd/ 或 run/ 下并给池打标。
// 仍有客户机持有 iSCSI 会话时拒绝：给已导出 LUN 的数据集改名会让导出指向空处。
func runMigrateLayout(args []string) int {
	cfg := config.Load(args)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	sessions, err := iscsi.New(logger).ActiveSessions(ctx)
	if err == nil && len(sessions) > 0 {
		fmt.Fprintf(os.Stderr, "迁移前请先关闭正在使用的客户机: %s\n(然后停止 ndiskless 服务再重试)\n", strings.Join(sessions, ", "))
		return 1
	}

	moved, err := zfs.New(cfg.Pool, nil, logger).MigrateLayout(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败(已迁 %d 个数据集,重跑本命令可从断点继续): %v\n", moved, err)
		return 1
	}
	fmt.Printf("布局迁移完成: %d 个数据集移入容器,存储池 %s 已标记为 v2\n", moved, cfg.Pool)
	return 0
}

// nodeSelfAddr 是本节点自己的可达地址：优先 NodeAddr，否则 PortalAddr（单机时二者相同）。
func nodeSelfAddr(cfg config.Config) string {
	if cfg.NodeAddr != "" {
		return cfg.NodeAddr
	}
	return cfg.PortalAddr
}

// keepalivedConfPath 是安装器写 VRRP 配置的位置，peer 对账器只改其中的 unicast_peer 块。
// keepalivedPendingMarker 由 `install-go.sh --defer-keepalived`（界面加入）放下，
// JoinFinisher 在 keepalived 可以安全启动后删除。
const (
	keepalivedConfPath      = "/etc/keepalived/keepalived.conf"
	keepalivedPendingMarker = "/etc/ndiskless/keepalived.pending"
)

// liveVRRPPeers 读取 keepalived 当前的单播 peer 列表，加入中的节点启动自己的 VRRP 前要看它。
// 本机没有 keepalived 配置时返回 nil。
func liveVRRPPeers() []string {
	data, err := os.ReadFile(keepalivedConfPath)
	if err != nil {
		return nil
	}
	return ha.UnicastPeers(string(data))
}

// randomHex32 生成集群令牌。它是机器之间的共享密钥，运维不需要看到或自己编。
func randomHex32() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// localNetworks 返回本机所有 IPv4 地址所在网段（CIDR）。
func localNetworks() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
			out = append(out, ipnet.String())
		}
	}
	return out
}

// subnetHas 判断 addr 是否落在 nets 之一。keepalived 要把 VIP 挂到某个网卡上，不在任何本地网段的地址
// 无处可挂；不在这里拦，失败只会出现在分离运行的配置过程里，运维看不到。
func subnetHas(nets []string, addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if _, network, err := net.ParseCIDR(n); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func localSubnetHas(addr string) bool { return subnetHas(localNetworks(), addr) }

// subnetCIDROf 返回地址所在的 /24，即值得扫描未配置节点的机房网段。故意不用网卡真实掩码：
// /16 就是六万五千次探测，那是扫网，不是「找这个机柜里的空机器」。
func subnetCIDROf(addr string) string {
	ip := net.ParseIP(strings.TrimSpace(addr)).To4()
	if ip == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], ip[2])
}

func localSubnetCIDR(addr string) string { return subnetCIDROf(addr) }

// forwardBootReadToWriter 代客户机向当前写入者询问只读的 /boot 接口。
// 备机服务放在它上面的客户机磁盘，客户机里的脚本会向给它磁盘的节点要网关和盘符；但备机的
// 在用目录不跟随写入者（副本只在激活时换入），根本不认识这台客户机，只能转发。转发必须带上
// 客户机自己的地址：写入者「只回答本机」的检查按它判断，而不是按转发节点。
func forwardBootReadToWriter(cfg config.Config, apiPort string, logger *slog.Logger) func(context.Context, string, string, string, string) (int, []byte, string, error) {
	return func(ctx context.Context, path, rawQuery, clientIP, authorization string) (int, []byte, string, error) {
		base := writerURL(cfg.PortalAddr, nodeSelfAddr(cfg), cfg.PeerURL, apiPort)
		if base == "" || cfg.ClusterToken == "" {
			return 0, nil, "", fmt.Errorf("本机不在集群里，无处转发")
		}
		target := strings.TrimSuffix(base, "/") + path
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
		if err != nil {
			return 0, nil, "", err
		}
		// 原始调用方的身份原样带过去；本机作为转发的集群节点，用自己的头证明身份。
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		req.Header.Set("X-ND-Cluster", cfg.ClusterToken)
		req.Header.Set("X-ND-Client", clientIP)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return 0, nil, "", err
		}
		logger.Info("relayed a client's boot query to the writer",
			"path", path, "client", clientIP, "writer", base, "status", resp.StatusCode)
		return resp.StatusCode, body, resp.Header.Get("Content-Type"), nil
	}
}

// resolveSelfAddr 返回本节点通往 VIP 的源地址，正是其他节点和被放置的客户机回拨要用的地址。
// UDP「连接」不发包，只让内核选路并绑定本地地址。
func resolveSelfAddr(vip string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(vip, "9"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}

// selfAPIURL 是其他节点拨本节点的地址：总是本机自己的地址，从不用 VIP。
func selfAPIURL(cfg config.Config) string {
	port := strings.TrimPrefix(cfg.Addr, ":")
	if _, p, err := net.SplitHostPort(cfg.Addr); err == nil && p != "" {
		port = p
	}
	addr := nodeSelfAddr(cfg)
	if addr == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(addr, port)
}

func hostIPForNode(cfg config.Config) string {
	// 必须是本机自己的地址。PortalAddr 在 HA 下是各节点都上报的 VIP，用它会让多个节点
	// 争同一个 servers.ip、后注册的被 UNIQUE 约束拒绝。PortalAddr 只在单机（二者相同）时替补。
	if cfg.NodeAddr != "" {
		return cfg.NodeAddr
	}
	if cfg.PortalAddr != "" {
		return cfg.PortalAddr
	}
	host, _ := os.Hostname()
	return host
}

// apiURLForNode 是其他节点拨本节点的地址，必须是本机自己的地址：指向 VIP 会让对端列表里
// 「另一个节点」解析成当前 VIP 持有者（自己或正被替换的主机），备机探到更高 epoch 的主机后拒绝接任。
// 与 selfAPIURL 保持一致，写这一行的两条路径不能各说各的。
func apiURLForNode(cfg config.Config) string { return selfAPIURL(cfg) }

// reconfigureForCluster 经 systemd-run 分离启动安装器，把本节点加入集群：第一台作为声明 VIP 的主机，
// 之后的都是备机。必须分离：重启会杀掉本进程，工作要比发起它的请求活得久。
// 安装器沿用本机已有的管理员密码（在 env 里），这里两条路径都不需要密码。
func reconfigureForCluster(logger *slog.Logger) func(context.Context, ha.JoinParams) error {
	return func(ctx context.Context, p ha.JoinParams) error {
		bin, err := exec.LookPath("ndiskless-configure")
		if err != nil {
			bin = "/usr/sbin/ndiskless-configure"
		}
		role := p.Role
		if role == "" {
			role = "standby"
		}
		// VRRP peer 是其他成员的真实地址，从不用 VIP（VIP 只是当前的主，不是一台机器）。
		// 第一台还没有 peer，空列表没问题：它是唯一候选，peer 对账器会随节点加入补齐。
		peers := strings.Join(p.Peers, ",")
		args := []string{
			"--no-block", "--collect", "--unit", "ndiskless-join",
			bin,
			"--peer", peers, "--vip", p.VIP, "--cluster-token", p.Token,
			"--ha-role", role, "--node-addr", p.NodeAddr, "--pool", p.Pool,
		}
		if p.JWTSecret != "" {
			args = append(args, "--jwt-secret", p.JWTSecret)
		}
		if p.CHAPSecret != "" {
			args = append(args, "--chap-secret", p.CHAPSecret)
		}
		// 加入者在写入者列出它之前不能起 VRRP，否则收不到通告会选自己当主；第一台无需等待。
		if !p.StartsKeepalivedNow() {
			args = append(args, "--defer-keepalived")
		}
		out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
		if err != nil {
			logger.Error("cluster reconfigure launch failed", "role", role, "error", err, "output", string(out))
			return fmt.Errorf("启动集群配置进程失败：%v", err)
		}
		logger.Info("cluster reconfigure launched", "role", role, "vip", p.VIP, "pool", p.Pool, "peers", peers)
		return nil
	}
}

// standbyMissingDBCopy 说明本节点为何不适合持有 VIP：它是备机，且接任所需的库副本从未到达。
// 分组、客户机、用户、设置只在 SQLite 里，备机的在用库不跟随写入者，激活时才换入副本，没有副本就拒绝激活。
// 拒绝接任的节点也不该参与 VIP 竞选，否则会出现有 VIP 无写入者的集群（nopreempt 让真主机抢不回）。
// 快路径只 stat 一次；文件不在才问 zfs：副本经 `recv -u` 到达，可能存在但未挂载，读起来是空目录。
func standbyMissingDBCopy(ctx context.Context, role, copyPath string, remount func(context.Context) (string, error)) error {
	if role != "standby" {
		return nil // 主机服务的是自己的活库，本来就不需要这份副本
	}
	if copyPath != "" && fileExists(copyPath) {
		return nil
	}
	if remount != nil {
		if dir, err := remount(ctx); err == nil && fileExists(filepath.Join(dir, "ndiskless.db")) {
			return nil
		}
	}
	return fmt.Errorf("本节点是备机且尚未收到主机的数据库副本，接管会丢失全部分组与客户机配置，" +
		"因此不参与虚 IP 竞选。请确认主机在服务、且本机能拉到目录")
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// dataPoolFile 是节点记录「自己的数据存在哪个池」的文件，故意放在本机（在用库旁边，不复制）。
// 目录里不存绝对路径（总是 <本机池>/nd/<id>），池记录也按 (node, name) 区分，各节点池名无需一致；
// 放在会复制的 system_settings.data_pool 里会让别台的池名传染过来，切换时去找别台的池。
func dataPoolFile(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "data-pool")
}

func readDataPool(dbPath string) string {
	b, err := os.ReadFile(dataPoolFile(dbPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeDataPool(dbPath, pool string) error {
	return os.WriteFile(dataPoolFile(dbPath), []byte(strings.TrimSpace(pool)+"\n"), 0o644)
}

// resolveDataPool 按权威顺序选本进程绑定的池：
//
//	local      本机上次绑定的池，总是优先
//	persisted  目录里的值，仅当本机确有这个池时才认：旧版本升上来的单机照常工作，
//	           属于别台的名字（集群场景）则被忽略
//	envDefault 安装器的 --pool，即本节点配置时给的值
func resolveDataPool(local, persisted, envDefault string, exists func(string) bool) string {
	if local = strings.TrimSpace(local); local != "" {
		return local
	}
	if persisted = strings.TrimSpace(persisted); persisted != "" && exists != nil && exists(persisted) {
		return persisted
	}
	return envDefault
}

// bindDataPool 选出池并把结果记下来，下次启动读事实而不是重新推断。
// 每次重推会回落到会复制的 system_settings.data_pool 或各台同名的 NDISKLESS_POOL，
// 让池叫 data1 的节点绑到 tank、反复向不存在的池收复制。
// 只记本机确实看得到的池名：在还没有池的机器上钉住回落值，会盖过运维随后以别的名字建的池。
func bindDataPool(local, persisted, envDefault string, exists func(string) bool, persist func(string) error) (string, error) {
	pool := resolveDataPool(local, persisted, envDefault, exists)
	if strings.TrimSpace(local) != "" {
		return pool, nil // 本机记录已经是最高权威，无需重写
	}
	if exists == nil || !exists(pool) || persist == nil {
		return pool, nil
	}
	if err := persist(pool); err != nil {
		return pool, fmt.Errorf("记下本机数据池名失败（下次重启会重新推断）: %w", err)
	}
	return pool, nil
}

// unprovisionedNonStandby 判断本节点只是还没配置，而不是坏了或不适合接任。
// 从没建过池的机器没东西可服务也没东西可丢；它若是写入者或独立节点仍应持有 VIP，
// 运维才能从 VIP 打开控制台统一配置整个集群。边界是「不是备机」而不是「集群里只有它」：
// 写入者还没池、运维刚加入第二台，是这个流程的正常中间态，不能因此丢掉 VIP。
// 无池的备机仍不健康（没有库副本，接不了任）；配了池却读不了的节点是坏了，dataPool 非空，不归这里。
func unprovisionedNonStandby(dataPool, role string) bool {
	return strings.TrimSpace(dataPool) == "" && role != "standby"
}

// nodeHealth 是 /healthz 的报告，也是 keepalived track_script 读的结果：库可读写、数据池可读且健康、
// LIO configfs 存在、运维未打维护标记。每条原因都写给看到 VIP 迁走的运维。
// localDataPool 在检查时读取而非接线时捕获：无池装机的节点可能几分钟后才在界面建池。
func nodeHealth(st *store.SQLStore, zfsClient *zfs.Client, role string, localDataPool func() string) func(context.Context) error {
	return func(ctx context.Context) error {
		// 池名在检查时取：无池装机后在界面建池会不重启地 SetPool；/healthz 决定 VIP 归属，
		// 不跟上的话节点会对着旧默认名（tank）永远 503，新建的池明明 ONLINE 也拿不到 VIP。
		pool := zfsClient.PoolName()
		dbCopyPath := filepath.Join(zfs.DBCopyDir(pool), "ndiskless.db")
		if _, err := os.Stat("/run/ndiskless.maintenance"); err == nil {
			return fmt.Errorf("维护模式已开启（/run/ndiskless.maintenance 存在），本节点主动让出服务")
		}
		if ha.StepDownActive("/run/ndiskless.step-down", time.Minute) {
			// 刚降级过：暂不参选，留够时间让对端拿走 VIP；标记会自行过期。
			return fmt.Errorf("本节点刚退位为备机，暂不参选（最多一分钟后自动恢复）")
		}
		if err := st.Ping(ctx); err != nil {
			return fmt.Errorf("数据库无响应: %w", err)
		}
		// 光能读不够：磁盘满或只读重挂时读都正常、写全失败，而本节点是唯一允许写的。
		if err := st.CheckWritable(ctx); err != nil {
			return fmt.Errorf("数据库不可写（磁盘满或文件只读）: %w", err)
		}
		status, err := zfsClient.PoolStatus(ctx, pool)
		if err != nil {
			// 还没建池的独立节点是「还没配」不是「坏了」，让它健康，VIP 才落得下来，运维才能从 VIP 进来建池。
			if unprovisionedNonStandby(localDataPool(), role) {
				return nil
			}
			return fmt.Errorf("存储池 %s 不可读: %w", pool, err)
		}
		if status.Health != "" && status.Health != "ONLINE" {
			return fmt.Errorf("存储池 %s 状态为 %s，请检查磁盘", pool, status.Health)
		}
		if !iscsi.Available() {
			return fmt.Errorf("LIO 内核模块未加载（configfs 缺失），客户机无法连接磁盘")
		}
		// 最后问「就算 VIP 给我，我顶得上吗」：前面查的是本机好不好，这项查的是能不能当主机，keepalived 要的是后者。
		if err := standbyMissingDBCopy(ctx, role, dbCopyPath, zfsClient.EnsureDBCopyDataset); err != nil {
			return err
		}
		return nil
	}
}

// processLockPath 是单实例 flock 的位置：/run 可写时（常规 systemd 部署）用 /run，否则用临时目录。
func processLockPath() string {
	if p := os.Getenv("NDISKLESS_LOCK_PATH"); p != "" {
		return p // 同机跑第二个实例（双节点演练）需要独立的锁
	}
	if f, err := os.OpenFile("/run/ndiskless.lock", os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		_ = f.Close()
		return "/run/ndiskless.lock"
	}
	return filepath.Join(os.TempDir(), "ndiskless.lock")
}

// liveDBPath 从 DSN 取出 SQLite 文件路径；内存或空 DSN 回落到 "ndiskless.db"（HA 切换需要可替换的库文件）。
func liveDBPath(dsn string) string {
	p := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == ":memory:" || p == "" {
		return "ndiskless.db"
	}
	return p
}

// freeBytesAt 返回路径所在文件系统的可用字节数。上传开始前用：传完 39G 再拒绝 40G 的文件代价最大。
func freeBytesAt(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// writerURL 是接受写入的节点地址：有 VIP 时为 VIP，否则为配置的对端。
// 注册用它，免得节点把自己登记进即将被复制覆盖的目录；备机拒绝写入时用它告诉运维该去哪。
// 固定对端在三节点集群里会让两台备机互相指着，而写入的是第三台。
// envBool 从环境变量读开关。只有明确的肯定值才打开，空值或拼错保持默认（各开关的默认都是安全侧）。
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func writerURL(portalAddr, nodeAddr, peerURL, port string) string {
	// 前面有 VIP 时 portalAddr 才与 nodeAddr 不同。
	if portalAddr != "" && portalAddr != nodeAddr {
		return "http://" + net.JoinHostPort(portalAddr, port)
	}
	return peerURL
}

// replicationPeer 决定本节点从哪拉目录：和注册同一个问题、同一个答案，即当前持有 VIP 的写入者。
// 固定对端在两节点时侥幸可用，有第三台时两台备机会互相拉，静默停在上次切换时的目录，
// 计划切回也因备机永远追不上而一直被拒。
// 返回空表示不复制：既无 VIP 也无对端（单机），或没有集群令牌。
func replicationPeer(portalAddr, nodeAddr, peerURL, port, token, self string) (ha.Peer, string) {
	src := writerURL(portalAddr, nodeAddr, peerURL, port)
	if src == "" || token == "" {
		return ha.Peer{}, ""
	}
	return ha.Peer{BaseURL: src, Token: token, Self: self}, src
}

// writerHint 就是 writerURL，按另一个调用方的叫法命名。
func writerHint(portalAddr, nodeAddr, peerURL, port string) string {
	return writerURL(portalAddr, nodeAddr, peerURL, port)
}
