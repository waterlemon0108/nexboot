package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// AlarmThresholds 是 DefaultAlarmRules 的可调阈值，零值取默认。
type AlarmThresholds struct {
	PoolCapWarnPct  float64 // 容量偏高 warn 阈值，默认 85
	PoolCapErrorPct float64 // 容量告急 error 阈值，默认 95
}

func (t AlarmThresholds) withDefaults() AlarmThresholds {
	if t.PoolCapWarnPct == 0 {
		t.PoolCapWarnPct = 85
	}
	if t.PoolCapErrorPct == 0 {
		t.PoolCapErrorPct = 95
	}
	return t
}

// NetworkAlarmInputs 是分组网络规则的输入：用来判定分组的客户机网段，
// 以及某地址能否 ping 通（中继网关不通则该教室可能开不了机）。为 nil 时禁用对应部分。
type NetworkAlarmInputs struct {
	Client    func(context.Context) (assets.ClientNetwork, error)
	Reachable func(context.Context, string) bool
}

// DefaultAlarmRules 构造基于真实服务的健康告警规则。数据源读不到时返回错误
// （记为 unknown、不动现有告警），避免瞬时故障把告警误判为恢复。
func DefaultAlarmRules(pools ops.PoolService, server ServiceService, backup ops.BackupService, reconcile ops.ReconcileService, st store.Store, th AlarmThresholds, network NetworkAlarmInputs) []AlarmRule {
	th = th.withDefaults()
	return []AlarmRule{
		{Source: "pool", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			// 只查本节点的池：库里也有别的节点的池，本机 zfs 查不到会变成常驻 UNKNOWN 告警。
			res, err := pools.List(ctx, localServerID(pools))
			if err != nil {
				return nil, err
			}
			return poolAlarmConditions(res.Items, th), nil
		}},
		{Source: "service", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			res, err := server.ListServices(ctx)
			if err != nil {
				return nil, err
			}
			return serviceAlarmConditions(res.Items), nil
		}},
		{Source: "backup", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			status, err := backup.Status(ctx)
			if err != nil {
				return nil, err
			}
			return backupAlarmConditions(status), nil
		}},
		// 库与池不一致（手工删了快照、无主数据集）在开机前不会暴露，靠这条告警提前展示。
		{Source: "consistency", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			report, err := reconcile.Check(ctx)
			if err != nil {
				return nil, err
			}
			return consistencyAlarmConditions(report), nil
		}},
		// 见 groupNetworkAlarmConditions。
		{Source: "group-network", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			if st == nil {
				return nil, nil
			}
			groups, err := st.Groups().List(ctx)
			if err != nil {
				return nil, err
			}
			if network.Client == nil {
				return nil, nil // 没有判定依据
			}
			client, err := network.Client(ctx)
			if err != nil {
				return nil, err
			}
			var reachable func(string) bool
			if network.Reachable != nil {
				reachable = func(ip string) bool { return network.Reachable(ctx, ip) }
			}
			return groupNetworkAlarmConditions(groups, client, reachable), nil
		}},
		{Source: "image", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
			reports, err := st.ImageHealthReports().List(ctx)
			if err != nil {
				return nil, err
			}
			images, err := st.Images().List(ctx)
			if err != nil {
				return nil, err
			}
			names := make(map[string]string, len(images))
			for _, img := range images {
				names[img.ID] = img.Name
			}
			return imageAlarmConditions(systemImageReports(reports, images), names), nil
		}},
		BootFailureAlarmRule(st, time.Now),
	}
}

// consistencyAlarmConditions 把库与池的比对结果转成告警：库里有、池上缺会导致开不了机，
// 记 error；池上无主的数据集或快照只占空间、可能挡住删除，记 warn。
func consistencyAlarmConditions(report ops.ConsistencyReport) []AlarmCondition {
	var conds []AlarmCondition
	for _, issue := range report.Issues {
		severity, kind := "warn", "数据残留"
		switch issue.Kind {
		case ops.IssueMissingDataset, ops.IssueMissingSnapshot:
			severity, kind = "error", "库与池不一致"
		}
		conds = append(conds, AlarmCondition{
			Key:      "consistency:" + issue.Kind + ":" + issue.Ref,
			Severity: severity,
			Type:     kind,
			Resource: issue.Ref,
			Value:    issue.Kind,
			Message:  issue.Detail,
		})
	}
	return conds
}

// groupNetworkAlarmConditions 报告服务器服务不了的分组。dnsmasq 只在持有地址的网段上应答
// DHCP，分组网段不在客户机网段内就发不出地址，而其它各层看起来都正常。新建时已拒绝这种分组，
// 这里兜住存量配置和服务器改地址后的情况。走 DHCP 中继的分组是合法的，只检查中继网关是否可达。
func groupNetworkAlarmConditions(groups []domain.Group, client assets.ClientNetwork, reachable func(string) bool) []AlarmCondition {
	if !client.Known {
		return nil // 读不到网卡不能当作有问题的证据
	}
	nets := make([]string, 0, len(client.Prefixes))
	for _, p := range client.Prefixes {
		nets = append(nets, p.String())
	}
	var conds []AlarmCondition
	for _, g := range groups {
		if hit := assets.ServerAddrsInGroup(g, client); len(hit) > 0 {
			addrs := make([]string, len(hit))
			for i, a := range hit {
				addrs[i] = a.String()
			}
			conds = append(conds, AlarmCondition{
				Key:      "group-server-addr:" + g.ID,
				Severity: "error",
				Type:     "分组地址含服务器地址",
				Resource: g.ID,
				Value:    strings.Join(addrs, "、"),
				Message: fmt.Sprintf("请到分组页改分组 %s 的起始 IP 或台数：它的地址范围包含服务器地址 %s，分到这个地址的客户机会和服务器冲突",
					g.Name, strings.Join(addrs, "、")),
			})
		}
		switch assets.ClassifyGroupNetwork(g, client) {
		case assets.NetworkBlocked:
			conds = append(conds, AlarmCondition{
				Key:      "group-network:" + g.ID,
				Severity: "error",
				Type:     "分组不在服务器客户机网段",
				Resource: g.ID,
				Value:    g.StartIP,
				Message: fmt.Sprintf("分组 %s 的网段 %s 不在服务器客户机网卡 %s（%s）的网段内，这组客户机拿不到地址、开不了机；到分组页点「改到服务器网段」，或在系统参数打开「跨网段分组」",
					g.Name, g.StartIP, client.Iface, strings.Join(nets, "、")),
			})
		case assets.NetworkRelay:
			gw := strings.TrimSpace(g.Gateway)
			if gw == "" || reachable == nil || reachable(gw) {
				continue
			}
			server := ""
			for _, p := range client.Prefixes {
				server = p.Addr().String()
				break
			}
			conds = append(conds, AlarmCondition{
				Key:      "group-relay:" + g.ID,
				Severity: "warn",
				Type:     "中继网关不可达",
				Resource: g.ID,
				Value:    gw,
				Message: fmt.Sprintf("从服务器 ping 不通分组 %s 的网关 %s。客户机可能拿不到地址：请网管检查该 VLAN 三层接口上的 DHCP 中继（helper-address %s）和到服务器的路由（交换机禁 ping 时可忽略）",
					g.Name, gw, server),
			})
		}
	}
	return conds
}

// localServerID 返回本节点 ID；未接存储时返回 ""，List 视为全部池。
func localServerID(pools ops.PoolService) string {
	if pools.Storage == nil {
		return ""
	}
	return pools.Storage.ServerID()
}

func poolAlarmConditions(items []ops.PoolItem, th AlarmThresholds) []AlarmCondition {
	var conds []AlarmCondition
	for _, p := range items {
		health := strings.ToUpper(p.Health)
		if health != "" && health != "ONLINE" && health != "PENDING" {
			conds = append(conds, AlarmCondition{
				Key: "pool-health:" + p.Name, Severity: "error", Type: "存储池降级",
				Resource: p.Name, Value: p.Health, Message: "存储池 " + p.Name + " 健康状态 " + p.Health,
			})
		}
		// 镜像数据组掉了成员时再坏一块就丢池；按组和盘点名，让操作者知道去换哪块。
		for _, g := range p.Groups {
			if g.Role != string(domain.PoolDiskRoleData) || g.Kind != "mirror" || len(g.Disks) < 2 {
				continue
			}
			online, out := 0, []string{}
			for _, d := range g.Disks {
				if strings.EqualFold(d.Status, "ONLINE") {
					online++
				} else {
					out = append(out, d.Path+" "+d.Status)
				}
			}
			if len(out) == 0 {
				continue
			}
			sev := "warn"
			if online <= 1 {
				sev = "error"
			}
			conds = append(conds, AlarmCondition{
				Key: "pool-mirror:" + p.Name + ":" + g.Name, Severity: sev, Type: "镜像组降级",
				Resource: p.Name + "/" + g.Name, Value: fmt.Sprintf("%d/%d 在线", online, len(g.Disks)),
				Message: fmt.Sprintf("存储池 %s 的 %s 仅剩 %d 路在线（%s），再坏一块该组数据即丢；请尽快换盘", p.Name, g.Name, online, strings.Join(out, "、")),
			})
		}
		if p.Capacity > 0 {
			pct := float64(p.Used) / float64(p.Capacity) * 100
			if pct >= th.PoolCapErrorPct {
				conds = append(conds, AlarmCondition{Key: "pool-cap:" + p.Name, Severity: "error", Type: "存储池将满", Resource: p.Name, Threshold: fmt.Sprintf("≥%.0f%%", th.PoolCapErrorPct), Value: fmt.Sprintf("%.0f%%", pct), Message: "存储池 " + p.Name + " 容量告急"})
			} else if pct >= th.PoolCapWarnPct {
				conds = append(conds, AlarmCondition{Key: "pool-cap:" + p.Name, Severity: "warn", Type: "存储池将满", Resource: p.Name, Threshold: fmt.Sprintf("≥%.0f%%", th.PoolCapWarnPct), Value: fmt.Sprintf("%.0f%%", pct), Message: "存储池 " + p.Name + " 容量偏高"})
			}
		}
	}
	return conds
}

// serviceAlarmConditions 按 Expected/OK（与控制台一致）而非原始 unit 状态判定：
// target.service 是 oneshot，备机的 dnsmasq 本就停着，直接看 Status 会误报。
func serviceAlarmConditions(items []ServiceItem) []AlarmCondition {
	var conds []AlarmCondition
	for _, s := range items {
		if !s.Critical || s.OK || s.Status == "unknown" {
			continue
		}
		// 先说操作者失去了什么，而不是 unit 的 ActiveState；服务层有说明时放在 Detail。
		sev := "error"
		msg := "关键服务 " + s.Label + " 未运行 (" + s.Status + ")"
		if s.Detail != "" {
			msg = "关键服务 " + s.Label + "：" + s.Detail
		}
		if s.Status == "not-installed" {
			sev, msg = "warn", "关键服务 "+s.Label+" 未安装"
		}
		conds = append(conds, AlarmCondition{Key: "service-down:" + s.Key, Severity: sev, Type: "服务异常", Resource: s.Unit, Value: s.Status, Message: msg})
	}
	return conds
}

// backupAlarmConditions 报告备份悄悄失效的两种情况：上一轮失败，或开了定时备份却没有备份池。
func backupAlarmConditions(status ops.BackupStatus) []AlarmCondition {
	var conds []AlarmCondition
	if t := status.Task; t != nil && t.Status == domain.TaskStatusFailed {
		msg := t.Error
		if msg == "" {
			msg = "备份任务失败"
		}
		conds = append(conds, AlarmCondition{Key: "backup-error", Severity: "warn", Type: "备份失败", Resource: "backup", Value: msg, Message: "上一轮备份失败：" + msg})
	}
	if status.Config.Enabled && status.Node.BackupPool == "" {
		conds = append(conds, AlarmCondition{Key: "backup-no-pool", Severity: "warn", Type: "无备份池", Resource: "backup", Value: "未配置",
			Message: "定时备份已开启，但这台机器上没有备份池——给它加一个池，副本才有地方放"})
	}
	return conds
}

// systemImageReports 去掉数据盘镜像的体检报告：数据盘上没有系统可查，旧报告只是噪音。
func systemImageReports(reports []domain.ImageHealthReport, images []domain.Image) []domain.ImageHealthReport {
	data := map[string]bool{}
	for _, img := range images {
		if img.Purpose == domain.ImagePurposeData {
			data[img.ID] = true
		}
	}
	kept := make([]domain.ImageHealthReport, 0, len(reports))
	for _, r := range reports {
		if !data[r.ImageID] {
			kept = append(kept, r)
		}
	}
	return kept
}

// imageAlarmConditions 在告警里写明体检查出的具体问题，操作者不必再打开镜像查看。
func imageAlarmConditions(reports []domain.ImageHealthReport, names map[string]string) []AlarmCondition {
	var conds []AlarmCondition
	for _, r := range reports {
		if r.Level != domain.HealthBlock && r.Level != domain.HealthWarn {
			continue
		}
		sev := "warn"
		if r.Level == domain.HealthBlock {
			sev = "error"
		}
		name := names[r.ImageID]
		if name == "" {
			name = r.ImageID
		}
		var problems []string
		for _, item := range r.Items {
			if item.Level == domain.HealthWarn || item.Level == domain.HealthBlock {
				problems = append(problems, item.Detail)
			}
		}
		msg := "镜像 " + name + " 体检未通过，请到镜像管理里查看体检明细"
		if len(problems) > 0 {
			msg = fmt.Sprintf("镜像 %s 体检有 %d 项需要处理：%s", name, len(problems), strings.Join(problems, "；"))
		}
		conds = append(conds, AlarmCondition{Key: "image-health:" + r.ImageID, Severity: sev, Type: "镜像体检", Resource: name, Value: string(r.Level), Message: msg})
	}
	return conds
}

// PeerPoolAlarmRule 按其它节点自报的池状态为它们产生池告警（告警只在写入者上跑，
// 本机 zfs 只看得到自己的池）。键带节点 ID，因为不同节点的池可能同名；
// 问不到的节点保留现有告警，不能因网络抖动误判恢复。
func PeerPoolAlarmRule(list func(context.Context) (ClusterPoolsResult, error), st store.Store, th AlarmThresholds) AlarmRule {
	th = th.withDefaults()
	return AlarmRule{Source: "peer-pool", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
		view, err := list(ctx)
		if err != nil {
			return nil, err
		}
		var conds []AlarmCondition
		var unread []string
		for _, n := range view.Nodes {
			if n.IsSelf {
				continue
			}
			if !n.Reachable {
				unread = append(unread, "@"+n.NodeID)
				continue
			}
			for _, c := range poolAlarmConditions(n.Pools, th) {
				c.Key += "@" + n.NodeID
				c.Resource = n.IP + ":" + c.Resource
				c.Message = "节点 " + n.IP + " " + c.Message
				conds = append(conds, c)
			}
		}
		if len(unread) == 0 {
			return conds, nil
		}
		open, err := st.Alarms().ListOpen(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range open {
			if a.Source != "peer-pool" {
				continue
			}
			for _, suffix := range unread {
				if strings.HasSuffix(a.AlarmKey, suffix) {
					conds = append(conds, AlarmCondition{Key: a.AlarmKey, Severity: a.Severity, Type: a.Type, Resource: a.Resource, Threshold: a.Threshold, Value: a.Value, Message: a.Message})
					break
				}
			}
		}
		return conds, nil
	}}
}

// PreservedCopyAlarmRule 把节点留存的目录副本展示到告警页：跟随对端会毁掉本机内容时
// 才留副本，不告警就没人知道。只有 -diverged- 副本告警，-rebuilding- 是重建的
// 正常残留、会自动清理；操作者删掉副本后告警自动恢复。
func PreservedCopyAlarmRule(list func(context.Context) (ClusterPreservedResult, error)) AlarmRule {
	return AlarmRule{Source: "preserved-copy", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
		if list == nil {
			return nil, nil
		}
		view, err := list(ctx)
		if err != nil {
			return nil, err
		}
		var conds []AlarmCondition
		for _, node := range view.Nodes {
			if !node.Reachable {
				continue // 问不到不等于没有，但也不能凭空报；失联由复制告警说
			}
			for _, copy := range node.Copies {
				if copy.Kind != storage.PreservedDiverged {
					continue
				}
				where := node.IP
				if where == "" {
					where = node.NodeID
				}
				conds = append(conds, AlarmCondition{
					Key:      "preserved:" + node.NodeID + ":" + copy.Dataset,
					Severity: "warn",
					Type:     "保留的目录副本",
					Resource: copy.Dataset,
					Value:    fmt.Sprintf("%d 个还原点", copy.Points),
					Message: fmt.Sprintf("节点 %s 保留了一份目录副本 %s（%d 个还原点，%s）：这些内容不在正在用的目录里，确认后删除该数据集可释放空间",
						where, copy.Dataset, copy.Points, humanBytes(copy.Used)),
				})
			}
		}
		return conds, nil
	}}
}

// humanBytes 只给告警文案用，够读就行。
func humanBytes(n int64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MiB", n/(1<<20))
}

// ReplicationAlarmRule 在目录复制滞后时告警，备机停止跟随时操作者还来得及处理。
// status 为 nil 时禁用该规则。
func ReplicationAlarmRule(status func(context.Context) (ops.ReplicationStatus, error), threshold time.Duration) AlarmRule {
	return AlarmRule{Source: "replication", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
		if status == nil {
			return nil, nil
		}
		st, err := status(ctx)
		if err != nil {
			return nil, err
		}
		var out []AlarmCondition
		for _, tg := range st.Targets {
			if tg.LastOKAt != nil && time.Duration(tg.LagSeconds)*time.Second <= threshold {
				continue
			}
			value := fmt.Sprintf("%d 秒未同步", tg.LagSeconds)
			if tg.LastOKAt == nil {
				value = "从未同步成功"
			}
			msg := fmt.Sprintf("目录复制滞后：对端 %s %s", tg.Target, value)
			if tg.LastError != "" {
				msg += "，最近错误：" + tg.LastError
			}
			out = append(out, AlarmCondition{
				Key: "replication-lag-" + tg.Target, Severity: "warn", Type: "复制滞后",
				Resource: tg.Target, Threshold: threshold.String(), Value: value, Message: msg,
			})
		}
		return out, nil
	}}
}
