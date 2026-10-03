// Package place 决定由哪个节点服务客户机：分组指定落点（节点失联时回落本机，
// 全量目录副本保证回落安全）；客户机按上次实际落点清理（跟随克隆，不跟随分组当前指定）；
// 巡检遍历所有可能有在线会话的节点。
package place

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/remote"
	"github.com/tianwei/diskless/internal/store"

	"github.com/tianwei/diskless/internal/domain"
)

// Freshness 是节点 last_seen_at 判活的时限：连续错过三次 30s 心跳即视为失联。
const Freshness = 90 * time.Second

// Placed 是一次落点结果：要调用的 agent 和客户机连接的地址。
// 发生回落（或 ForTerminal 时归属节点失联）时 Healthy 为 false。
type Placed struct {
	Agent    storage.ClientHostAgent
	ServerID string
	Portal   string
	Remote   bool
	Healthy  bool
}

// Router 按 servers 表计算落点。没有远端节点时一律落本机，即单机行为。
type Router struct {
	Store  store.Store
	NodeID string
	Local  storage.ClientHostAgent
	// LocalPortal 是落本机的客户机连接的地址，HA 下为 VIP。
	LocalPortal string
	Token       string
	Now         func() time.Time
	Logger      *slog.Logger

	// load 记录上次对账以来本进程分给各节点的落点数。按决策计数而非会话数，
	// 开机风暴中同时决策的机器才不会全挑同一个节点。
	// 必须用原子量而非互斥锁：此路径上的全局锁曾让 7.6 秒的开机风暴变成 90 秒。
	load sync.Map // nodeID -> *atomic.Int64
}

func (r *Router) counter(nodeID string) *atomic.Int64 {
	v, _ := r.load.LoadOrStore(nodeID, new(atomic.Int64))
	return v.(*atomic.Int64)
}

// ReconcileLoad 用各节点实际服务数校准计数器。计数只增不减，客户机关机、
// 掉线、回收都不经过这里，不定期校准会让均衡逐渐失真。
func (r *Router) ReconcileLoad(actual map[string]int64) {
	for id, n := range actual {
		r.counter(id).Store(n)
	}
	// 未上报在线客户机的节点清零。
	r.load.Range(func(k, v any) bool {
		if _, ok := actual[k.(string)]; !ok {
			v.(*atomic.Int64).Store(0)
		}
		return true
	})
}

// ForClient 决定由哪个节点服务这台机器的盘，按顺序取第一条适用的规则：
//
//  1. 超管机落写入者（见函数内说明）。
//  2. 分组有人工指定节点时照办。
//  3. 否则回上次开机的节点：克隆还在、ARC 是热的，重启最快。
//  4. 否则落客户机最少的健康节点。
//
// 规则 3 中上次节点负载明显超出均值时让出，否则新加节点永远分不到机器。
func (r *Router) ForClient(ctx context.Context, g domain.Group, t domain.Terminal) Placed {
	candidates := r.candidates(ctx)

	// 1. 超管机必须落写入者：保存会把克隆 promote 进 <pool>/nd/ 并打快照，
	// 而该目录由写入者 `recv -F` 单向复制，在备机上写入会被下一轮复制回滚，
	// 只剩下一条指向不存在快照的还原点记录，此后该分组 /boot 全部 500。
	// 超管会话只有一台机器，不分散也没有代价。
	if t.IsSuper {
		if p, ok := r.writer(candidates); ok {
			if t.StorageServerID != nil && *t.StorageServerID != "" && *t.StorageServerID != p.ServerID {
				r.logf("placement: super machine moved to the writer; saving anywhere else is rolled back by replication",
					"terminal", t.ID, "was", *t.StorageServerID, "now", p.ServerID)
			}
			return p
		}
		// 无写入者（单机或节点表尚未收敛）时回退到原锚定节点，保证单机可用。
		if t.StorageServerID != nil && *t.StorageServerID != "" {
			if p, ok := r.pick(*t.StorageServerID, candidates); ok {
				return p
			}
			// 锚定节点已不在，别处无法服务这块盘：降级回落本机，不在别处悄悄另起一份。
			return r.local(false)
		}
	}

	// 2. 人工指定节点
	if g.StorageServerID != nil && *g.StorageServerID != "" {
		return r.ForGroup(ctx, g)
	}

	// 3. 回上次节点，除非其负载明显超出均值
	if t.StorageServerID != nil && *t.StorageServerID != "" {
		if p, ok := r.pick(*t.StorageServerID, candidates); ok && !r.overloaded(*t.StorageServerID, candidates) {
			r.counter(p.ServerID).Add(1)
			return p
		}
	}

	// 4. 分散到最空节点
	return r.leastLoaded(candidates)
}

// candidates 返回当前可服务客户机的节点：本机加心跳新鲜的远端节点。
func (r *Router) candidates(ctx context.Context) []domain.Server {
	servers, err := r.Store.Servers().List(ctx)
	if err != nil {
		r.logf("placement: server list failed, placing locally", "error", err)
		return nil
	}
	out := make([]domain.Server, 0, len(servers))
	for _, s := range servers {
		if s.ID == r.NodeID || (r.fresh(s) && s.APIURL != "") {
			out = append(out, s)
		}
	}
	return out
}

// writer 返回接受写入的节点，只有它的目录改动不会被复制回滚。
// HAState 为空（单机或节点表未追平）表示未知，不等于否。
func (r *Router) writer(candidates []domain.Server) (Placed, bool) {
	for _, s := range candidates {
		if strings.EqualFold(strings.TrimSpace(s.HAState), "active") {
			return r.pick(s.ID, candidates)
		}
	}
	return Placed{}, false
}

func (r *Router) pick(nodeID string, candidates []domain.Server) (Placed, bool) {
	if nodeID == r.NodeID {
		return r.local(true), true
	}
	for _, s := range candidates {
		if s.ID == nodeID {
			return Placed{Agent: r.dial(s), ServerID: s.ID, Portal: s.PortalIP, Remote: true, Healthy: true}, true
		}
	}
	return Placed{}, false
}

// overloaded 判断节点负载是否明显超出均值，超出时让新加节点也能分到机器。
func (r *Router) overloaded(nodeID string, candidates []domain.Server) bool {
	if len(candidates) < 2 {
		return false
	}
	var total int64
	for _, s := range candidates {
		total += r.counter(s.ID).Load()
	}
	share := float64(total) / float64(len(candidates))
	return float64(r.counter(nodeID).Load()) > share*1.5+1
}

func (r *Router) leastLoaded(candidates []domain.Server) Placed {
	if len(candidates) == 0 {
		p := r.local(true)
		r.counter(p.ServerID).Add(1)
		return p
	}
	best := candidates[0]
	bestN := r.counter(best.ID).Load()
	for _, s := range candidates[1:] {
		if n := r.counter(s.ID).Load(); n < bestN {
			best, bestN = s, n
		}
	}
	r.counter(best.ID).Add(1)
	if p, ok := r.pick(best.ID, candidates); ok {
		return p
	}
	return r.local(true)
}

func (r *Router) local(healthy bool) Placed {
	return Placed{Agent: r.Local, ServerID: r.NodeID, Portal: r.LocalPortal, Healthy: healthy}
}

// ForGroup 落到分组指定节点；该节点失联时降级回落本机（Healthy 为 false，供调用方告警）。
func (r *Router) ForGroup(ctx context.Context, g domain.Group) Placed {
	if g.StorageServerID == nil || *g.StorageServerID == "" || *g.StorageServerID == r.NodeID {
		return r.local(true)
	}
	srv, err := r.Store.Servers().Get(ctx, *g.StorageServerID)
	if err != nil {
		r.logf("placement: group's storage node unknown, falling back local", "group", g.ID, "server", *g.StorageServerID, "error", err)
		return r.local(false)
	}
	if !r.fresh(srv) {
		r.logf("placement: group's storage node is dark, falling back local", "group", g.ID, "server", srv.ID)
		return r.local(false)
	}
	return Placed{Agent: r.dial(srv), ServerID: srv.ID, Portal: srv.PortalIP, Remote: true, Healthy: true}
}

// ForTerminal 路由到客户机上次开机的节点。不回落：克隆在哪就只能在哪清理，
// Healthy 为 false 提示调用方此次调用大概率失败。
func (r *Router) ForTerminal(ctx context.Context, t domain.Terminal) Placed {
	if t.StorageServerID == nil || *t.StorageServerID == "" || *t.StorageServerID == r.NodeID {
		return r.local(true)
	}
	srv, err := r.Store.Servers().Get(ctx, *t.StorageServerID)
	if err != nil {
		r.logf("placement: terminal's storage node unknown, treating as local", "terminal", t.ID, "server", *t.StorageServerID)
		return r.local(false)
	}
	return Placed{Agent: r.dial(srv), ServerID: srv.ID, Portal: srv.PortalIP, Remote: true, Healthy: r.fresh(srv)}
}

// AllHosts 列出本机及所有心跳新鲜的远端节点，供巡检遍历；本机排第一。
func (r *Router) AllHosts(ctx context.Context) []Placed {
	out := []Placed{r.local(true)}
	servers, err := r.Store.Servers().List(ctx)
	if err != nil {
		r.logf("placement: server list failed, census is local-only", "error", err)
		return out
	}
	for _, srv := range servers {
		if srv.ID == r.NodeID || !r.fresh(srv) || srv.APIURL == "" {
			continue
		}
		out = append(out, Placed{Agent: r.dial(srv), ServerID: srv.ID, Portal: srv.PortalIP, Remote: true, Healthy: true})
	}
	return out
}

func (r *Router) dial(srv domain.Server) storage.ClientHostAgent {
	return remote.Client{BaseURL: srv.APIURL, Token: r.Token}
}

func (r *Router) fresh(srv domain.Server) bool {
	return srv.LastSeenAt != nil && r.now().Sub(*srv.LastSeenAt) <= Freshness
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Router) logf(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Warn(msg, args...)
	}
}
