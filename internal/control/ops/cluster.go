package ops

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
	"strings"
)

// ClusterRegistry 是集群成员管理的主机侧，处理 /internal/cluster/register。
// 加入与心跳是同一个幂等 upsert；刻意不用 ensureServer，它的旧身份改键逻辑会
// 在单行表里把主机自己的行当成新来者的旧身份吞掉。
type ClusterRegistry struct {
	Store  store.Store
	Now    func() time.Time
	Logger *slog.Logger
	// SelfNodeID 是本机 id，Forget 据此拒绝移除本机。
	SelfNodeID string
	// LivePools 询问某节点当前实际有哪些池。别台的池记录可能已过时（跨节点
	// 销毁在对方执行，本机记录会残留），Forget 先问节点本身再信记录。
	LivePools func(ctx context.Context, node string) ([]string, error)
}

// Forget 把已下线的节点移出名册，否则它会一直计入法定人数分母并参与放置。
// 两种情况拒绝：节点名下还有池或客户机（记录会悬空），以及移除本机。
func (r ClusterRegistry) Forget(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return errs.Invalid("要移除哪个节点？")
	}
	if nodeID == r.SelfNodeID {
		// 用 errs.Conflict 而不是裸 error，否则 API 回 500 并丢掉原文。
		return errs.Conflict("不能移除本机：正在处理这个请求的就是它。" +
			"请到集群里另一台机器上操作，或先把本机退位")
	}
	if _, err := r.Store.Servers().Get(ctx, nodeID); err != nil {
		return err
	}
	pools, err := r.Store.Pools().List(ctx)
	if err != nil {
		return err
	}
	var owned []domain.Pool
	for _, p := range pools {
		if p.ServerID == nodeID {
			owned = append(owned, p)
		}
	}
	if len(owned) > 0 {
		// 记录可能已过时，问节点本身：答「没有池」就清掉陈旧记录；答有池或
		// 联系不上则拒绝。
		if err := r.refuseUnlessNodeHasNoPools(ctx, nodeID, owned); err != nil {
			return err
		}
	}
	terminals, err := r.Store.Terminals().List(ctx)
	if err != nil {
		return err
	}
	for _, t := range terminals {
		if t.StorageServerID != nil && *t.StorageServerID == nodeID {
			return errs.Conflict(fmt.Sprintf("还有客户机的盘落在节点 %s 上（例如 %s）。"+
				"请先让它们改投其它节点（关机后重新开机即可）再移除", nodeID, t.MAC))
		}
	}
	if r.Logger != nil {
		r.Logger.Info("移除已下线的节点", "node", nodeID)
	}
	return r.Store.Servers().Delete(ctx, nodeID)
}

// refuseUnlessNodeHasNoPools 在节点报告没有池时清掉它的池记录，否则（含问不到）拒绝。
func (r ClusterRegistry) refuseUnlessNodeHasNoPools(ctx context.Context, nodeID string, owned []domain.Pool) error {
	refuse := func() error {
		return errs.Conflict(fmt.Sprintf("节点 %s 名下还有存储池 %s，移除后这些池的记录会悬空。"+
			"请先销毁或转移它的池", nodeID, owned[0].Name))
	}
	if r.LivePools == nil {
		return refuse()
	}
	live, err := r.LivePools(ctx, nodeID)
	if err != nil || len(live) > 0 {
		return refuse()
	}
	return r.Store.Tx(ctx, func(tx store.Store) error {
		disks, err := tx.PoolDisks().List(ctx)
		if err != nil {
			return err
		}
		for _, p := range owned {
			for _, d := range disks {
				if d.PoolID == p.ID {
					if err := tx.PoolDisks().Delete(ctx, d.ID); err != nil {
						return err
					}
				}
			}
			if err := tx.Pools().Delete(ctx, p.ID); err != nil {
				return err
			}
			if r.Logger != nil {
				r.Logger.Info("清掉陈旧的池记录：节点报告该池已不存在", "node", nodeID, "pool", p.Name)
			}
		}
		return nil
	})
}

func (r ClusterRegistry) Register(ctx context.Context, n ha.NodeInfo) error {
	now := r.Now()
	// 与建分组时的区间检查互为反向：节点地址落在分组区间内会被 dnsmasq 发给
	// 客户机，节点随之从网络上消失。注册时当场拒绝，比事后排查便宜。
	if err := r.refuseAddressInsideAGroup(ctx, n); err != nil {
		return err
	}
	existing, err := r.Store.Servers().Get(ctx, n.NodeID)
	if err != nil {
		if !errs.IsNotFound(err) {
			return err
		}
		srv := serverFromNodeInfo(n)
		srv.Name = n.NodeID
		srv.Status = domain.ServerStatusUp
		srv.LastSeenAt = &now
		// 新节点自报的 epoch 不采信，从 0 起算：重建的机器可能带着旧库里的
		// 大 epoch，而 KnownEpoch 取名册最大值，会让它一注册就压过所有节点。
		if srv.Epoch != 0 {
			if r.Logger != nil {
				r.Logger.Warn("新节点自报的 epoch 不予采信，从 0 起算",
					"node", n.NodeID, "claimed", srv.Epoch)
			}
			srv.Epoch = 0
		}
		if err := r.Store.Servers().Create(ctx, srv); err != nil {
			return err
		}
		return r.reconcilePools(ctx, n)
	}
	// 同一模板克隆的机器共用 /etc/machine-id，即同一 node_id。当前持有者
	// 仍新鲜时有别的地址来注册，视为身份冲突而不是改地址。
	if existing.APIURL != "" && existing.APIURL != n.APIURL &&
		existing.LastSeenAt != nil && now.Sub(*existing.LastSeenAt) <= place.Freshness {
		return fmt.Errorf("节点身份冲突：%s 与 %s 上报了同一个节点标识 %s，"+
			"通常是从同一个系统模板克隆出来的机器共用了 /etc/machine-id。"+
			"请在新机器上执行 rm -f /etc/machine-id && systemd-machine-id-setup 后重启服务",
			existing.APIURL, n.APIURL, n.NodeID)
	}
	existing.IP = hostOf(n.APIURL)
	existing.PortalIP = n.PortalIP
	existing.APIURL = n.APIURL
	existing.HAState = n.Role
	existing.Epoch = n.Epoch
	existing.Status = domain.ServerStatusUp
	existing.LastSeenAt = &now
	if err := r.Store.Servers().Update(ctx, existing); err != nil {
		return err
	}
	return r.reconcilePools(ctx, n)
}

// reconcilePools 按 n 的上报（见 ha.PoolReport）对账它的池记录：补上节点有而
// 目录缺的，删掉节点已销毁的；节点缺失但仍认得的池保留（是缺失不是销毁）。
// 只动 n 自己的记录。
func (r ClusterRegistry) reconcilePools(ctx context.Context, n ha.NodeInfo) error {
	if n.Pools == nil {
		return nil
	}
	present := map[string]ha.ReportedPool{}
	for _, p := range n.Pools.Present {
		if p.Pool.Name != "" {
			present[p.Pool.Name] = p
		}
	}
	known := map[string]bool{}
	for _, name := range n.Pools.Known {
		known[name] = true
	}
	return r.Store.Tx(ctx, func(tx store.Store) error {
		pools, err := tx.Pools().List(ctx)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, p := range pools {
			if p.ServerID != n.NodeID {
				continue
			}
			have[p.Name] = true
			if _, ok := present[p.Name]; ok || known[p.Name] {
				continue
			}
			if err := deletePoolRecord(ctx, tx, p.ID); err != nil {
				return err
			}
			r.logf("节点报告这个池已被销毁，删掉它的记录", "node", n.NodeID, "pool", p.Name)
		}
		for name, p := range present {
			if have[name] {
				continue
			}
			pool := p.Pool
			pool.ID, pool.ServerID = storage.PoolID(n.NodeID, name), n.NodeID
			if err := tx.Pools().Create(ctx, pool); err != nil {
				return err
			}
			if err := replacePoolDisksIn(ctx, tx, pool.ID, p.Disks); err != nil {
				return err
			}
			r.logf("节点上有这个池、目录里没有它的记录，补上", "node", n.NodeID, "pool", name)
		}
		return nil
	})
}

func (r ClusterRegistry) logf(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Info(msg, args...)
	}
}

func deletePoolRecord(ctx context.Context, tx store.Store, poolID string) error {
	disks, err := tx.PoolDisks().List(ctx)
	if err != nil {
		return err
	}
	for _, d := range disks {
		if d.PoolID == poolID {
			if err := tx.PoolDisks().Delete(ctx, d.ID); err != nil {
				return err
			}
		}
	}
	return tx.Pools().Delete(ctx, poolID)
}

// refuseAddressInsideAGroup 拒绝管理地址或 portal 地址落在分组客户机区间内的节点。
func (r ClusterRegistry) refuseAddressInsideAGroup(ctx context.Context, n ha.NodeInfo) error {
	groups, err := r.Store.Groups().List(ctx)
	if err != nil {
		// 读不到分组时放行，不让一次数据库抖动挡住注册。
		if r.Logger != nil {
			r.Logger.Warn("could not read groups; the node's address was not checked against client ranges", "error", err)
		}
		return nil
	}
	for _, candidate := range []string{hostOf(n.APIURL), n.PortalIP} {
		addr, err := netip.ParseAddr(strings.TrimSpace(candidate))
		if err != nil || !addr.Is4() {
			continue
		}
		for _, g := range groups {
			if !groupRangeContains(g, addr) {
				continue
			}
			return errs.Conflict(fmt.Sprintf(
				"节点地址 %s 落在分组「%s」的客户机区间（%s 起 %d 个）内：这个地址迟早会被发给某台客户机，届时该节点会从网络上消失。请先改分组区间或换一个节点地址",
				candidate, g.Name, g.StartIP, g.ClientMax))
		}
	}
	return nil
}

// groupRangeContains 判断 addr 是否在分组的客户机区间内。
func groupRangeContains(g domain.Group, addr netip.Addr) bool {
	start, err := netip.ParseAddr(strings.TrimSpace(g.StartIP))
	if err != nil || !start.Is4() || g.ClientMax <= 0 {
		return false
	}
	s4, a4 := start.As4(), addr.As4()
	toN := func(b [4]byte) uint32 {
		return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	}
	first := toN(s4)
	return toN(a4) >= first && toN(a4) <= first+uint32(g.ClientMax)-1
}

// Peers 返回完整名册，调用方自行滤掉自己。
func (r ClusterRegistry) Peers(ctx context.Context) ([]ha.NodeInfo, error) {
	servers, err := r.Store.Servers().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ha.NodeInfo, 0, len(servers))
	for _, s := range servers {
		out = append(out, ha.NodeInfo{NodeID: s.ID, APIURL: s.APIURL, PortalIP: s.PortalIP, Role: s.HAState, Epoch: s.Epoch, LastSeenAt: s.LastSeenAt})
	}
	return out, nil
}

// SyncPeersIntoStore 用主机返回的名册刷新非主机节点的本地名册。本机行归注册
// 循环管，不动；主机不再列出的行删除，被移除的节点下次同步后处处消失。
func SyncPeersIntoStore(ctx context.Context, st store.Store, selfID string, roster []ha.NodeInfo, now time.Time) error {
	listed := map[string]bool{selfID: true}
	for _, n := range roster {
		if n.NodeID == selfID || n.NodeID == "" {
			continue
		}
		listed[n.NodeID] = true
		existing, err := st.Servers().Get(ctx, n.NodeID)
		if err != nil {
			if !errs.IsNotFound(err) {
				return err
			}
			srv := serverFromNodeInfo(n)
			srv.Name = n.NodeID
			srv.Status = domain.ServerStatusUp
			srv.LastSeenAt = &now
			if n.LastSeenAt != nil {
				seen := *n.LastSeenAt
				srv.LastSeenAt = &seen
			}
			if err := st.Servers().Create(ctx, srv); err != nil {
				return err
			}
			continue
		}
		existing.IP = hostOf(n.APIURL)
		existing.PortalIP = n.PortalIP
		existing.APIURL = n.APIURL
		existing.HAState = n.Role
		existing.Epoch = n.Epoch
		// 新鲜度以主机为准，否则行会停在首次见到的时间而显示离线。
		// 旧版本名册不带时间，保留现值。
		if n.LastSeenAt != nil {
			seen := *n.LastSeenAt
			existing.LastSeenAt = &seen
		}
		if err := st.Servers().Update(ctx, existing); err != nil {
			return err
		}
	}
	servers, err := st.Servers().List(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if !listed[s.ID] {
			if err := st.Servers().Delete(ctx, s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func serverFromNodeInfo(n ha.NodeInfo) domain.Server {
	return domain.Server{
		ID:       n.NodeID,
		IP:       hostOf(n.APIURL),
		PortalIP: n.PortalIP,
		APIURL:   n.APIURL,
		Role:     domain.ServerRoleAll,
		HAState:  n.Role,
		Epoch:    n.Epoch,
	}
}

func hostOf(apiURL string) string {
	if u, err := url.Parse(apiURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return apiURL
}

// NodeView 是面向操作者的名册条目，供选择节点和查看健康。
type NodeView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	IP         string     `json:"ip"`
	PortalIP   string     `json:"portal_ip"`
	HAState    string     `json:"ha_state"`
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

// NodeListResult 是 /api/cluster/nodes 的响应。
type NodeListResult struct {
	Items []NodeView `json:"items"`
	Total int        `json:"total"`
}

// Nodes 列出名册；Online 沿用放置的新鲜度规则，让界面和开机路径对存活的判断一致。
func (r ClusterRegistry) Nodes(ctx context.Context) (NodeListResult, error) {
	servers, err := r.Store.Servers().List(ctx)
	if err != nil {
		return NodeListResult{}, err
	}
	now := r.Now()
	items := make([]NodeView, 0, len(servers))
	for _, s := range servers {
		items = append(items, NodeView{
			ID: s.ID, Name: s.Name, IP: s.IP, PortalIP: s.PortalIP, HAState: s.HAState,
			Online:     s.LastSeenAt != nil && now.Sub(*s.LastSeenAt) <= place.Freshness,
			LastSeenAt: s.LastSeenAt,
		})
	}
	return NodeListResult{Items: items, Total: len(items)}, nil
}
