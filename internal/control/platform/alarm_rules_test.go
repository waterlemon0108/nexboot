package platform

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

func TestPoolAlarmConditions(t *testing.T) {
	th := AlarmThresholds{}.withDefaults()
	items := []ops.PoolItem{
		{Name: "tank", Health: "ONLINE", Capacity: 100, Used: 50},   // 健康，无告警
		{Name: "cold", Health: "DEGRADED", Capacity: 100, Used: 10}, // 降级 error
		{Name: "warm", Health: "online", Capacity: 100, Used: 90},   // 容量 warn (85-95)
		{Name: "full", Health: "PENDING", Capacity: 100, Used: 97},  // 容量 error (≥95)
		{Name: "zero", Health: "", Capacity: 0, Used: 0},            // 无容量信息，跳过
	}
	conds := poolAlarmConditions(items, th)
	got := map[string]string{}
	for _, c := range conds {
		got[c.Key] = c.Severity
	}
	want := map[string]string{
		"pool-health:cold": "error",
		"pool-cap:warm":    "warn",
		"pool-cap:full":    "error",
	}
	if len(got) != len(want) {
		t.Fatalf("conditions = %#v, want keys %#v", conds, want)
	}
	for k, sev := range want {
		if got[k] != sev {
			t.Fatalf("%s severity = %q, want %q (all: %#v)", k, got[k], sev, got)
		}
	}
}

// 镜像组掉成员时再坏一块就丢池；告警要点名是哪个组、哪块盘。
func TestPoolAlarmConditionsNameTheMirrorGroupThatIsDown(t *testing.T) {
	th := AlarmThresholds{}.withDefaults()
	items := []ops.PoolItem{{Name: "tank", Health: "DEGRADED", Capacity: 100, Used: 10, Layout: "mirror", GroupWidth: 2, Groups: []ops.PoolGroupItem{
		{Name: "mirror-0", Kind: "mirror", Role: "data", Status: "ONLINE", Disks: []ops.PoolDiskItem{{Path: "/dev/sda", Status: "ONLINE"}, {Path: "/dev/sdb", Status: "ONLINE"}}},
		{Name: "mirror-1", Kind: "mirror", Role: "data", Status: "DEGRADED", Disks: []ops.PoolDiskItem{{Path: "/dev/sdc", Status: "ONLINE"}, {Path: "/dev/sdd", Status: "OFFLINE"}}},
		{Name: "mirror-2", Kind: "mirror", Role: "data", Status: "DEGRADED", Disks: []ops.PoolDiskItem{{Path: "/dev/sde", Status: "ONLINE"}, {Path: "/dev/sdf", Status: "ONLINE"}, {Path: "/dev/sdg", Status: "UNAVAIL"}}},
		{Name: "mirror-3", Kind: "mirror", Role: "log", Status: "DEGRADED", Disks: []ops.PoolDiskItem{{Path: "/dev/nvme0", Status: "ONLINE"}, {Path: "/dev/nvme1", Status: "OFFLINE"}}},
	}}}
	conds := poolAlarmConditions(items, th)
	byKey := map[string]AlarmCondition{}
	for _, c := range conds {
		byKey[c.Key] = c
	}
	one, ok := byKey["pool-mirror:tank:mirror-1"]
	if !ok || one.Severity != "error" || !strings.Contains(one.Message, "mirror-1") || !strings.Contains(one.Message, "/dev/sdd") || !strings.Contains(one.Message, "仅剩 1 路") {
		t.Fatalf("mirror-1 condition = %#v (all: %#v)", one, conds)
	}
	two, ok := byKey["pool-mirror:tank:mirror-2"]
	if !ok || two.Severity != "warn" || !strings.Contains(two.Message, "/dev/sdg") {
		t.Fatalf("mirror-2 condition = %#v", two)
	}
	if _, ok := byKey["pool-mirror:tank:mirror-0"]; ok {
		t.Fatal("healthy group alarmed")
	}
	if _, ok := byKey["pool-mirror:tank:mirror-3"]; ok {
		t.Fatal("log mirror is not a data group; the pool alarm covers it")
	}
}

func TestPoolAlarmConditionsCustomThresholds(t *testing.T) {
	th := AlarmThresholds{PoolCapWarnPct: 50, PoolCapErrorPct: 70}
	conds := poolAlarmConditions([]ops.PoolItem{{Name: "t", Health: "ONLINE", Capacity: 100, Used: 60}}, th.withDefaults())
	if len(conds) != 1 || conds[0].Severity != "warn" || conds[0].Threshold != "≥50%" {
		t.Fatalf("conds = %#v, want single warn ≥50%%", conds)
	}
}

func TestServiceAlarmConditions(t *testing.T) {
	items := []ServiceItem{
		{Key: "dnsmasq", Label: "DNSMASQ", Unit: "dnsmasq.service", Critical: true, Status: "running", OK: true}, // 正常
		{Key: "iscsi", Label: "iSCSI", Unit: "target.service", Critical: true, Status: "stopped"},                // error
		{Key: "zfs", Label: "ZFS", Unit: "zfs-zed.service", Critical: true, Status: "not-installed"},             // warn
		{Key: "self", Label: "ndiskless", Unit: "ndiskless.service", Critical: false, Status: "stopped"},         // 非关键，跳过
		{Key: "nfs", Label: "NFS", Unit: "nfs.service", Critical: true, Status: "unknown"},                       // unknown 跳过
	}
	conds := serviceAlarmConditions(items)
	if len(conds) != 2 {
		t.Fatalf("conds = %#v, want 2", conds)
	}
	if conds[0].Key != "service-down:iscsi" || conds[0].Severity != "error" {
		t.Fatalf("first = %#v", conds[0])
	}
	if conds[1].Key != "service-down:zfs" || conds[1].Severity != "warn" {
		t.Fatalf("second = %#v", conds[1])
	}
}

// 备份悄悄失效的两种情况：上一轮失败，或开了定时备份却没有备份池。
func TestBackupAlarmConditions(t *testing.T) {
	ok := ops.BackupStatus{
		Config: ops.BackupConfigStatus{Enabled: true},
		Node:   ops.NodeBackupStatus{BackupPool: "backup"},
	}
	if conds := backupAlarmConditions(ok); len(conds) != 0 {
		t.Fatalf("正常状态报了告警：%#v", conds)
	}

	failed := ok
	failed.Task = &domain.Task{ID: "t1", Status: domain.TaskStatusFailed, Error: "zfs send failed"}
	conds := backupAlarmConditions(failed)
	if len(conds) != 1 || conds[0].Key != "backup-error" || conds[0].Severity != "warn" {
		t.Fatalf("conds = %#v", conds)
	}

	noPool := ops.BackupStatus{Config: ops.BackupConfigStatus{Enabled: true}}
	conds = backupAlarmConditions(noPool)
	if len(conds) != 1 || conds[0].Key != "backup-no-pool" {
		t.Fatalf("没有备份池却不报：%#v", conds)
	}

	// 没开定时备份时不因「没有备份池」告警
	off := ops.BackupStatus{Config: ops.BackupConfigStatus{Enabled: false}}
	if conds := backupAlarmConditions(off); len(conds) != 0 {
		t.Fatalf("未启用却报了告警：%#v", conds)
	}
}

func TestImageAlarmConditions(t *testing.T) {
	conds := imageAlarmConditions([]domain.ImageHealthReport{
		{ImageID: "ok", Level: domain.HealthOK},
		{ImageID: "warned", Level: domain.HealthWarn},
		{ImageID: "blocked", Level: domain.HealthBlock},
	}, nil)
	if len(conds) != 2 {
		t.Fatalf("conds = %#v, want 2", conds)
	}
	if conds[0].Key != "image-health:warned" || conds[0].Severity != "warn" {
		t.Fatalf("first = %#v", conds[0])
	}
	if conds[1].Key != "image-health:blocked" || conds[1].Severity != "error" {
		t.Fatalf("second = %#v", conds[1])
	}
}

func TestImageAlarmNamesTheImageAndWhatIsWrong(t *testing.T) {
	conds := imageAlarmConditions([]domain.ImageHealthReport{{
		ImageID: "ubuntu", Level: domain.HealthWarn,
		Items: []domain.HealthCheckItem{
			{Name: "partition_style", Level: domain.HealthOK, Detail: "GPT"},
			{Name: "iscsi_boot", Level: domain.HealthWarn, Detail: "内核参数没有 iscsi_auto"},
			{Name: "bcd", Level: domain.HealthWarn, Detail: "未找到 BCD"},
		},
	}}, map[string]string{"ubuntu": "Ubuntu 22"})
	if len(conds) != 1 {
		t.Fatalf("conds = %#v", conds)
	}
	c := conds[0]
	if c.Resource != "Ubuntu 22" || c.Message != "镜像 Ubuntu 22 体检有 2 项需要处理：内核参数没有 iscsi_auto；未找到 BCD" {
		t.Fatalf("cond = %#v", c)
	}
	if strings.Contains(c.Message, "GPT") {
		t.Fatal("passing items do not belong in the alarm")
	}
}

// 各数据源读失败必须返回错误而不是空条件：扫描器把空条件当作全部恢复，
// 吞掉读错误会在数据库抖动时关掉所有告警。
// service 源例外：systemctl 不可达时单元标为 unknown 且不告警，见 serviceAlarmConditions。
func TestDefaultAlarmRulesCoverEverySourceAndReportReadFailures(t *testing.T) {
	ctx := context.Background()
	// 关闭数据库让所有数据源同时读失败。
	st := newAlarmStore(t)
	closer, ok := st.(interface{ Close() error })
	if !ok {
		t.Fatalf("store %T cannot be closed", st)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	rules := DefaultAlarmRules(
		ops.PoolService{Store: st},
		ServiceService{},
		ops.BackupService{Store: st},
		ops.ReconcileService{Store: st},
		st,
		AlarmThresholds{},
		NetworkAlarmInputs{},
	)

	sources := map[string]bool{}
	for _, rule := range rules {
		sources[rule.Source] = true
		conds, err := rule.Eval(ctx)
		if rule.Source == "service" {
			if err != nil || len(conds) != 0 {
				t.Fatalf("service rule = %v / %d conditions, want a quiet empty listing", err, len(conds))
			}
			continue
		}
		if err == nil {
			t.Fatalf("source %q reported %d conditions instead of the read failure", rule.Source, len(conds))
		}
	}
	for _, want := range []string{"pool", "service", "backup", "image", "consistency"} {
		if !sources[want] {
			t.Fatalf("no rule for %q; sources = %v", want, sources)
		}
	}
}

// 阈值可调但不能为零，否则池里有一个字节就会告警。
func TestDefaultAlarmRulesFallBackToUsableThresholds(t *testing.T) {
	th := AlarmThresholds{}.withDefaults()
	if th.PoolCapWarnPct != 85 || th.PoolCapErrorPct != 95 {
		t.Fatalf("defaults = %#v", th)
	}
	custom := AlarmThresholds{PoolCapWarnPct: 70, PoolCapErrorPct: 90}.withDefaults()
	if custom.PoolCapWarnPct != 70 || custom.PoolCapErrorPct != 90 {
		t.Fatalf("custom thresholds were overwritten: %#v", custom)
	}
	// warn 线必须低于 error 线，否则更紧急的那条永远到不了。
	if th.PoolCapWarnPct >= th.PoolCapErrorPct {
		t.Fatalf("warn %v is not below error %v", th.PoolCapWarnPct, th.PoolCapErrorPct)
	}
}

// 库与池不一致（如手工删了快照）要通过告警展示出来，否则开机时才会暴露。
func TestConsistencyAlarmsSpeakInOperatorTerms(t *testing.T) {
	conds := consistencyAlarmConditions(ops.ConsistencyReport{OK: false, Issues: []ops.ConsistencyIssue{
		{Kind: ops.IssueMissingSnapshot, Ref: "cfg_r1", Detail: "配置「教学一班」的还原点 @r1 快照不存在，用它开机会失败"},
		{Kind: ops.IssueMissingDataset, Ref: "win11", Detail: "镜像「win11」的数据集不存在，该镜像已无法使用"},
		{Kind: ops.IssueOrphanDataset, Ref: "leftover", Detail: "数据集 leftover 不属于任何镜像或配置"},
		{Kind: ops.IssueOrphanSnapshot, Ref: "img@--head--", Detail: "快照 img@--head-- 不属于任何还原点"},
	}})
	if len(conds) != 4 {
		t.Fatalf("conditions = %#v", conds)
	}
	bySeverity := map[string]int{}
	for _, c := range conds {
		bySeverity[c.Severity]++
		if c.Message == "" || c.Resource == "" {
			t.Fatalf("condition says nothing actionable: %#v", c)
		}
		if !strings.Contains(c.Key, "consistency") {
			t.Fatalf("key %q must not collide with other sources", c.Key)
		}
	}
	// 库里有、池上缺会导致开不了机；无主数据集只占空间。
	if bySeverity["error"] != 2 || bySeverity["warn"] != 2 {
		t.Fatalf("severities = %#v", bySeverity)
	}
}

func TestConsistencyAlarmsAreSilentWhenTheTwoSidesAgree(t *testing.T) {
	if conds := consistencyAlarmConditions(ops.ConsistencyReport{OK: true}); len(conds) != 0 {
		t.Fatalf("conditions = %#v", conds)
	}
}

// 分组网段不在服务器客户机网段内时报 error 并给出修法；
// 中继部署下此类分组合法，只检查中继网关通不通。
func TestGroupNetworkAlarmConditions(t *testing.T) {
	client := assets.ClientNetwork{
		Iface: "ndbr0", Known: true,
		Prefixes:    []netip.Prefix{netip.MustParsePrefix("192.168.50.1/24")},
		ServerAddrs: []netip.Addr{netip.MustParseAddr("192.168.50.1")},
	}
	groups := []domain.Group{
		{ID: "g-1", Name: "一班", StartIP: "192.168.50.100", ClientMax: 10, Gateway: "192.168.50.254"}, // 同网段，正常
		{ID: "g-2", Name: "二班", StartIP: "192.168.10.10", ClientMax: 30, Gateway: "192.168.10.1"},    // 不在客户机网段
		{ID: "g-3", Name: "坏数据", StartIP: "不是地址", ClientMax: 10},                                     // 读不出来的不猜
	}
	conds := groupNetworkAlarmConditions(groups, client, nil)
	if len(conds) != 1 {
		t.Fatalf("想要 1 条告警，得到 %d 条：%#v", len(conds), conds)
	}
	got := conds[0]
	if got.Resource != "g-2" || got.Severity != "error" {
		t.Fatalf("告警指错了对象或级别：%#v", got)
	}
	for _, want := range []string{"二班", "192.168.10.10", "ndbr0", "192.168.50.1/24", "改到服务器网段", "跨网段分组"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("告警文案里缺 %q：%s", want, got.Message)
		}
	}

	// 中继模式：网关 ping 不通才提醒。
	client.AllowCrossSubnet = true
	down := func(string) bool { return false }
	conds = groupNetworkAlarmConditions(groups, client, down)
	if len(conds) != 1 || conds[0].Resource != "g-2" || conds[0].Severity != "warn" || !strings.Contains(conds[0].Message, "192.168.10.1") || !strings.Contains(conds[0].Message, "helper-address 192.168.50.1") {
		t.Fatalf("中继网关不可达应报 warn 并给出中继配置：%#v", conds)
	}
	up := func(string) bool { return true }
	if conds = groupNetworkAlarmConditions(groups, client, up); len(conds) != 0 {
		t.Fatalf("中继网关通了不该报：%#v", conds)
	}
	if conds = groupNetworkAlarmConditions(groups, client, nil); len(conds) != 0 {
		t.Fatalf("没有探测手段时不猜：%#v", conds)
	}
}

// 一块网卡都读不到时不报分组告警，读不到不等于配错。
func TestGroupNetworkAlarmStaysQuietWithoutLocalAddresses(t *testing.T) {
	conds := groupNetworkAlarmConditions([]domain.Group{
		{ID: "g-1", Name: "一班", StartIP: "192.168.50.100", ClientMax: 10},
	}, assets.ClientNetwork{}, nil)
	if len(conds) != 0 {
		t.Fatalf("读不到本机地址却报了 %d 条告警：%#v", len(conds), conds)
	}
}

// 判定函数必须接进规则表，否则扫描器不会调用。
func TestDefaultAlarmRulesIncludesTheGroupNetworkCheck(t *testing.T) {
	rules := DefaultAlarmRules(ops.PoolService{}, ServiceService{}, ops.BackupService{}, ops.ReconcileService{}, nil, AlarmThresholds{}, NetworkAlarmInputs{})
	for _, r := range rules {
		if r.Source == "group-network" {
			return
		}
	}
	var sources []string
	for _, r := range rules {
		sources = append(sources, r.Source)
	}
	t.Fatalf("规则表里没有 group-network：%v", sources)
}

// 复制滞后时告警并点名对端；新近同步过则不告警。
func TestReplicationAlarmRule(t *testing.T) {
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)
	stale := ops.ReplicationStatus{Targets: []ops.ReplicationTarget{{
		Target: "http://a:8080", LastOKAt: &old, LagSeconds: 600, LastError: "short read",
	}}}
	rule := ReplicationAlarmRule(func(context.Context) (ops.ReplicationStatus, error) { return stale, nil }, 5*time.Minute)
	conds, err := rule.Eval(context.Background())
	if err != nil || len(conds) != 1 {
		t.Fatalf("conds = %#v err=%v", conds, err)
	}
	if conds[0].Severity != "warn" || conds[0].Type != "复制滞后" || !strings.Contains(conds[0].Message, "http://a:8080") || !strings.Contains(conds[0].Message, "short read") {
		t.Fatalf("cond = %#v", conds[0])
	}

	fresh := now.Add(-time.Minute)
	quiet := ops.ReplicationStatus{Targets: []ops.ReplicationTarget{{Target: "http://a:8080", LastOKAt: &fresh, LagSeconds: 60}}}
	rule = ReplicationAlarmRule(func(context.Context) (ops.ReplicationStatus, error) { return quiet, nil }, 5*time.Minute)
	if conds, _ := rule.Eval(context.Background()); len(conds) != 0 {
		t.Fatalf("conds = %#v", conds)
	}
}

// oneshot 单元 inactive 不是故障，按 Expected/OK 判定而非 Status。
// target.service 加载 LIO 配置后即退出，常态 inactive；按 Status 判会产生常驻误报。
func TestServiceAlarmTrustsExpectedNotRawStatus(t *testing.T) {
	items := []ServiceItem{
		// oneshot：自身 stopped，但能力探针正常
		{Key: "iscsi", Label: "iSCSI Target · LIO", Unit: "target.service",
			Critical: true, Status: "stopped", Expected: "ready", OK: true},
		// 备机上的 dnsmasq 本就停着
		{Key: "dnsmasq", Label: "DNSMASQ", Unit: "dnsmasq.service",
			Critical: true, Status: "stopped", Expected: "stopped", OK: true},
	}
	if got := serviceAlarmConditions(items); len(got) != 0 {
		t.Fatalf("按预期在跑的服务不该报警：%+v", got)
	}
}

// 能力真的没了要告警，文案说操作者失去了什么，而不是 unit 的 ActiveState。
func TestServiceAlarmStillFiresWhenTheCapabilityIsGone(t *testing.T) {
	items := []ServiceItem{
		{Key: "iscsi", Label: "iSCSI Target · LIO", Unit: "target.service",
			Critical: true, Status: "stopped", Expected: "ready", OK: false,
			Detail: "内核态缺失：客户机无法连接磁盘"},
	}
	got := serviceAlarmConditions(items)
	if len(got) != 1 {
		t.Fatalf("能力缺失必须报警：%+v", got)
	}
	if !strings.Contains(got[0].Message, "客户机无法连接磁盘") {
		t.Fatalf("要把原因说给运维听，而不是只报单元状态：%q", got[0].Message)
	}
}

// 别的节点的池不由本机规则告警：库里有其它节点的池行，拿去问本机 zfs 只会得到 UNKNOWN。
func TestPoolAlarmOnlyJudgesThisNodesOwnPools(t *testing.T) {
	ctx := context.Background()
	st := newAlarmStore(t)
	for i, id := range []string{"me", "peer-a", "peer-b"} {
		ip := fmt.Sprintf("10.9.0.%d", i+1)
		if err := st.Servers().Create(ctx, domain.Server{ID: id, Name: id, IP: ip, PortalIP: ip, Role: "all", Status: "up"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []domain.Pool{
		{ID: "pool-me", ServerID: "me", Name: "tank"},
		{ID: "pool-a", ServerID: "peer-a", Name: "data"},
		{ID: "pool-b", ServerID: "peer-b", Name: "data1"},
	} {
		if err := st.Pools().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	local := &localPools{self: "me", health: map[string]string{"tank": "ONLINE"}}
	rule := poolRule(t, ops.PoolService{Store: st, Storage: local})

	if conds, err := rule.Eval(ctx); err != nil || len(conds) != 0 {
		t.Fatalf("本机池健康、别台的池不归本机管，应当无告警：%+v err=%v", conds, err)
	}
	for _, name := range local.asked {
		if name != "tank" {
			t.Fatalf("不该拿别台的池 %q 去问本机的 zfs（asked=%v）", name, local.asked)
		}
	}

	// 本机的池真降级了必须告警
	local.health["tank"] = "DEGRADED"
	conds, err := rule.Eval(ctx)
	if err != nil || len(conds) == 0 || conds[0].Resource != "tank" {
		t.Fatalf("本机的池降级必须报：%+v err=%v", conds, err)
	}
}

func poolRule(t *testing.T, pools ops.PoolService) AlarmRule {
	t.Helper()
	for _, r := range DefaultAlarmRules(pools, ServiceService{}, ops.BackupService{}, ops.ReconcileService{}, nil, AlarmThresholds{}, NetworkAlarmInputs{}) {
		if r.Source == "pool" {
			return r
		}
	}
	t.Fatal("no pool rule")
	return AlarmRule{}
}

// localPools 模拟单节点 zfs：只认得自己的池。
type localPools struct {
	storage.PoolAdmin
	self   string
	health map[string]string
	asked  []string
}

func (l *localPools) ServerID() string                               { return l.self }
func (l *localPools) DataPoolName() string                           { return "tank" }
func (l *localPools) BackupPoolName(context.Context) (string, error) { return "", nil }
func (l *localPools) PoolStatus(_ context.Context, name string) (domain.PoolStatus, error) {
	l.asked = append(l.asked, name)
	h, ok := l.health[name]
	if !ok {
		return domain.PoolStatus{}, fmt.Errorf("cannot open '%s': no such pool", name)
	}
	return domain.PoolStatus{Name: name, Health: h, Capacity: 100, Used: 10}, nil
}

// 别的节点的池由写入者去问该节点（告警只在写入者上评估），否则备机池降级无人报。
func TestPeerPoolAlarmsJudgeEachPeerByItsOwnAnswer(t *testing.T) {
	st := newAlarmStore(t)
	view := ClusterPoolsResult{Nodes: []ClusterPoolView{
		{NodeID: "me", IP: "10.0.0.4", IsSelf: true, Reachable: true, Pools: []ops.PoolItem{{Name: "tank", Health: "DEGRADED", Capacity: 100, Used: 10}}},
		{NodeID: "peer-a", IP: "10.0.0.3", Reachable: true, Pools: []ops.PoolItem{{Name: "data", Health: "DEGRADED", Capacity: 100, Used: 10}}},
		{NodeID: "peer-b", IP: "10.0.0.5", Reachable: true, Pools: []ops.PoolItem{{Name: "data1", Health: "ONLINE", Capacity: 100, Used: 10}}},
	}}
	rule := PeerPoolAlarmRule(func(context.Context) (ClusterPoolsResult, error) { return view, nil }, st, AlarmThresholds{})
	conds, err := rule.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 本机的池由 source=pool 规则报，这里不重复
	if len(conds) != 1 {
		t.Fatalf("只该报 peer-a 的 data：%+v", conds)
	}
	c := conds[0]
	if c.Key != "pool-health:data@peer-a" || !strings.Contains(c.Message, "10.0.0.3") || !strings.Contains(c.Message, "DEGRADED") {
		t.Fatalf("要说清是哪台的哪个池：%+v", c)
	}
}

// 别的节点暂时问不到时保留其已有池告警，否则一次网络抖动就会误判恢复；节点失联由复制告警报。
func TestPeerPoolAlarmsHoldWhileAPeerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	st := newAlarmStore(t)
	svc := AlarmService{Store: st}
	degraded := []AlarmCondition{{Key: "pool-health:data@peer-a", Severity: "error", Type: "存储池降级", Resource: "10.0.0.3:data", Value: "DEGRADED", Message: "节点 10.0.0.3 存储池 data 健康状态 DEGRADED"}}
	if err := svc.reconcile(ctx, "peer-pool", degraded); err != nil {
		t.Fatal(err)
	}

	view := ClusterPoolsResult{Nodes: []ClusterPoolView{
		{NodeID: "me", IsSelf: true, Reachable: true},
		{NodeID: "peer-a", IP: "10.0.0.3", Error: "节点失联"},
	}}
	rule := PeerPoolAlarmRule(func(context.Context) (ClusterPoolsResult, error) { return view, nil }, st, AlarmThresholds{})
	conds, err := rule.Eval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) != 1 || conds[0].Key != "pool-health:data@peer-a" || conds[0].Value != "DEGRADED" {
		t.Fatalf("连不上的节点，其已有告警要原样保留：%+v", conds)
	}

	// 节点恢复且池正常后才恢复告警
	view.Nodes[1] = ClusterPoolView{NodeID: "peer-a", IP: "10.0.0.3", Reachable: true, Pools: []ops.PoolItem{{Name: "data", Health: "ONLINE", Capacity: 100, Used: 10}}}
	if conds, err := rule.Eval(ctx); err != nil || len(conds) != 0 {
		t.Fatalf("节点回来且池健康，应当无告警：%+v err=%v", conds, err)
	}
}

// 集群视图整体读不到时返回错误（未知），不能当作全部恢复。
func TestPeerPoolAlarmsReportAnUnreadableView(t *testing.T) {
	rule := PeerPoolAlarmRule(func(context.Context) (ClusterPoolsResult, error) {
		return ClusterPoolsResult{}, fmt.Errorf("database is closed")
	}, newAlarmStore(t), AlarmThresholds{})
	if _, err := rule.Eval(context.Background()); err == nil {
		t.Fatal("视图读不到必须返回错误，让告警原样保留")
	}
}

// 保留的目录副本是「数据还在、但不在在用目录里」的唯一线索，必须告警。
func TestPreservedCopyAlarmNamesTheNodeAndWhatIsInside(t *testing.T) {
	view := ClusterPreservedResult{Nodes: []PreservedCopyView{
		{NodeID: "node-a", IP: "10.0.0.3", Reachable: true, Copies: []storage.PreservedCopy{
			{Dataset: "data/nd-diverged-1790068582", Kind: storage.PreservedDiverged, Used: 13 << 30, Points: 3},
			{Dataset: "data/nd-rebuilding-1790068600", Kind: storage.PreservedRebuilding, Used: 1 << 30},
		}},
		{NodeID: "node-b", IP: "10.0.0.4", Reachable: true},
	}}
	rule := PreservedCopyAlarmRule(func(context.Context) (ClusterPreservedResult, error) { return view, nil })

	conds, err := rule.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 重建的临时副本不告警，它是正常重建的产物
	if len(conds) != 1 {
		t.Fatalf("只该报有内容的那份：%+v", conds)
	}
	c := conds[0]
	if !strings.Contains(c.Message, "10.0.0.3") || !strings.Contains(c.Message, "nd-diverged-1790068582") {
		t.Fatalf("要说清哪台机器、哪个数据集：%+v", c)
	}
	if !strings.Contains(c.Message, "3") || !strings.Contains(c.Message, "GiB") {
		t.Fatalf("要说清里面有多少内容、占多少空间：%+v", c)
	}
	if c.Key != "preserved:node-a:data/nd-diverged-1790068582" {
		t.Fatalf("键要能对上这一份，删掉后才会自动恢复：%q", c.Key)
	}
}

// 问不到的节点不报：其清单不可信（此处特意带上副本，确保测到这道防御）。节点失联由复制告警报。
func TestPreservedCopyAlarmSaysNothingAboutNodesItCannotAsk(t *testing.T) {
	view := ClusterPreservedResult{Nodes: []PreservedCopyView{
		{NodeID: "node-a", IP: "10.0.0.3", Error: "节点失联", Copies: []storage.PreservedCopy{
			{Dataset: "data/nd-diverged-1", Kind: storage.PreservedDiverged, Points: 2},
		}},
	}}
	rule := PreservedCopyAlarmRule(func(context.Context) (ClusterPreservedResult, error) { return view, nil })
	if conds, err := rule.Eval(context.Background()); err != nil || len(conds) != 0 {
		t.Fatalf("conds = %+v err = %v", conds, err)
	}
}

// 后加入的节点地址落在已有分组区间里，建组检查拦不到，只能靠告警。
func TestGroupNetworkAlarmFlagsAServerAddressInsideAGroup(t *testing.T) {
	client := assets.ClientNetwork{Known: true, Iface: "ens33",
		Prefixes:    []netip.Prefix{netip.MustParsePrefix("192.168.1.3/24")},
		ServerAddrs: []netip.Addr{netip.MustParseAddr("192.168.1.3"), netip.MustParseAddr("192.168.1.12")}}
	groups := []domain.Group{{ID: "g1", Name: "开黑房 A", StartIP: "192.168.1.10", ClientMax: 5, Netmask: "255.255.255.0"}}
	conds := groupNetworkAlarmConditions(groups, client, nil)
	if len(conds) != 1 || conds[0].Key != "group-server-addr:g1" || conds[0].Severity != "error" ||
		!strings.Contains(conds[0].Message, "开黑房 A") || !strings.Contains(conds[0].Message, "192.168.1.12") {
		t.Fatalf("conds = %#v", conds)
	}
}

// 数据盘没有系统，即使留有旧体检报告也不告警。
func TestImageAlarmSkipsDataDisks(t *testing.T) {
	reports := []domain.ImageHealthReport{
		{ImageID: "steam", Level: domain.HealthBlock},
		{ImageID: "win11", Level: domain.HealthWarn},
	}
	images := []domain.Image{{ID: "steam", Purpose: domain.ImagePurposeData}, {ID: "win11", Purpose: domain.ImagePurposeSystem}}
	kept := systemImageReports(reports, images)
	if len(kept) != 1 || kept[0].ImageID != "win11" {
		t.Fatalf("kept = %#v", kept)
	}
}
