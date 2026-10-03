package assets

// 回收已关机客户机的临时克隆。否则克隆要等该机下次开机才清，轮换使用的机房会堆积 CLIENT-* 数据集：
// 占 CoW 空间、钉住源快照（挡住还原点/配置删除）、抬高池内 zvol 数量，而开机风暴成功率与 zvol 数量相关。

import (
	"context"
	"sort"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

const (
	// cloneGracePeriod 是客户机最后一个 iSCSI 会话断开后克隆的保留时长。会话断开不等于关机：
	// 链路抖动、交换机重启、客户机休眠都会短暂清空 dynamic_sessions，ReconcileOnline 的 2×5s 去抖
	// 远短于 initiator 重连窗口；重连中的客户机系统盘被删，会话就再也回不来。
	cloneGracePeriod = 5 * time.Minute
	// cloneReclaimBatch 限制每轮回收数量，避免整间机房同时关机时回收本身变成风暴；剩下的等下一轮。
	cloneReclaimBatch = 20
)

// ReclaimOfflineClones 删除超过 cloneGracePeriod 无 iSCSI 会话的客户机克隆，返回回收台数。
//
// 候选来自两处、互补盲区：ReconcileOnline 持久化的 terminal.OfflineAt（重启后仍在）；
// 节点自身的克隆列表（覆盖终端记录过期、已删或一直未被标离线的克隆）。
// firstSeen 跨轮记录后者每个 MAC 的计时（归 RunCloneReclaimer 所有），不能为 nil。
// 会话查询出错时整轮放弃，不能当作全部关机。
func (s TerminalService) ReclaimOfflineClones(ctx context.Context, firstSeen map[string]time.Time) (int, error) {
	// 以下都按这次快照决策；之后才开机的客户机由 host 在该客户机锁内复查放过。
	decidedAt := time.Now()
	hosts := s.hosts(ctx)
	online := map[string]struct{}{}
	for _, h := range hosts {
		live, err := h.Agent.ActiveClientMACs(ctx)
		if err != nil {
			return 0, err
		}
		for _, mac := range live {
			online[storage.NormalizeMAC(mac)] = struct{}{}
		}
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()

	known := make(map[string]domain.Terminal, len(terminals))
	doomed := map[string]struct{}{}
	for _, terminal := range terminals {
		key := storage.NormalizeMAC(terminal.MAC)
		known[key] = terminal
		if s.reclaimable(terminal, online, now) {
			doomed[key] = struct{}{}
		}
	}

	// 对账来源：会话列表显示无人使用的克隆。按 host 分别读，因为清理必须回到持有克隆的 host。
	stillHasClone := map[string]struct{}{}
	cloneHost := map[string]storage.ClientHostAgent{}
	for _, h := range hosts {
		clones, err := h.Agent.ClientCloneMACs(ctx)
		if err != nil {
			return 0, err
		}
		for _, mac := range clones {
			key := storage.NormalizeMAC(mac)
			stillHasClone[key] = struct{}{}
			cloneHost[key] = h.Agent
			if _, ok := online[key]; ok {
				delete(firstSeen, key) // 已重新上线，作废待回收计时
				continue
			}
			if terminal, ok := known[key]; ok && terminal.IsSuper {
				continue
			}
			if _, ok := doomed[key]; ok {
				continue // 已由终端自身的 OfflineAt 覆盖
			}
			if _, seen := firstSeen[key]; !seen {
				firstSeen[key] = now
			}
			if now.Sub(firstSeen[key]) >= cloneGracePeriod {
				doomed[key] = struct{}{}
			}
		}
	}
	// 克隆已不存在的 MAC 清掉计时，避免 map 随进程寿命无限增长。
	for key := range firstSeen {
		if _, ok := stillHasClone[key]; !ok {
			delete(firstSeen, key)
		}
	}

	macs := make([]string, 0, len(doomed))
	for key := range doomed {
		if _, ok := stillHasClone[key]; ok {
			macs = append(macs, key)
		}
	}
	sort.Strings(macs) // 固定顺序，批量截断才不随机
	if len(macs) > cloneReclaimBatch {
		macs = macs[:cloneReclaimBatch]
	}

	reclaimed := 0
	for _, mac := range macs {
		// 单台失败不影响本轮其它机器，下一轮重试。
		host := cloneHost[mac]
		if host == nil {
			host = s.Storage
		}
		done, err := host.ReclaimIdleClientClones(ctx, mac, decidedAt)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("offline clone reclaim failed", "mac", mac, "error", err)
			}
			continue
		}
		delete(firstSeen, mac)
		if done {
			reclaimed++
		}
	}
	if reclaimed > 0 && s.Logger != nil {
		s.Logger.Info("offline clones reclaimed", "count", reclaimed)
	}
	return reclaimed, nil
}

// reclaimable 判断终端记录本身是否表明克隆可回收：普通机、已确认离线足够久、且无活动会话。
func (s TerminalService) reclaimable(terminal domain.Terminal, online map[string]struct{}, now time.Time) bool {
	if terminal.IsSuper {
		return false // 超管机克隆是持久盘
	}
	if _, ok := online[storage.NormalizeMAC(terminal.MAC)]; ok {
		return false
	}
	if terminal.State != domain.TerminalStateOffline || terminal.OfflineAt == nil {
		return false
	}
	return now.Sub(*terminal.OfflineAt) >= cloneGracePeriod
}

// RunCloneReclaimer 定期回收离线客户机的克隆。存储节点报不出会话（如 LIO 不可用）时跳过本轮，不删任何东西。
func (s TerminalService) RunCloneReclaimer(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	firstSeen := map[string]time.Time{} // 对账来源的每 MAC 计时
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ReclaimOfflineClones(ctx, firstSeen); err != nil && s.Logger != nil {
				s.Logger.Error("offline clone reclaim skipped", "error", err)
			}
		}
	}
}

// ClientsPerNode 按节点 id 统计当前在服务的客户机数。放置用它校正自身计数：
// 那些计数只增不减，而客户机关机下线不经过放置。
func (s TerminalService) ClientsPerNode(ctx context.Context) (map[string]int64, error) {
	live, err := s.activeMACsAllHosts(ctx)
	if err != nil {
		return nil, err
	}
	online := make(map[string]struct{}, len(live))
	for _, mac := range live {
		online[storage.NormalizeMAC(mac)] = struct{}{}
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, t := range terminals {
		if _, ok := online[storage.NormalizeMAC(t.MAC)]; !ok {
			continue
		}
		node := ""
		if t.StorageServerID != nil {
			node = *t.StorageServerID
		}
		if node != "" {
			out[node]++
		}
	}
	return out, nil
}
