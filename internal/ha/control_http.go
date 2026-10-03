package ha

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// ControlHandler 提供 /internal/ha/ 下的 HA 控制端点：status 供对端激活守卫读取，
// activate/standby 供 keepalived 通知脚本调用。需集群令牌，切换请求只限集群内部。
type ControlHandler struct {
	Controller *Controller
	NodeID     string
	Token      string
	// VRRPPeers 返回本节点当前的 keepalived 单播 peer 列表，请求时读取，刚完成的对账立即可见。
	VRRPPeers func() []string
}

func (h ControlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(h.Token)) != 1 {
		http.Error(w, "cluster token required", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/handover/"):
		h.serveHandover(w, r)
	case strings.HasSuffix(r.URL.Path, "/status"):
		rs := h.Controller.State()
		// 带上本机此刻是否持有 VIP，对端激活守卫靠它区分真在服务和角色文件尚未改过来。
		// 探针未接时报 false：谎称持有会挡住合法接任。
		holds := h.Controller.HoldsVIP != nil && h.Controller.HoldsVIP()
		var peers []string
		if h.VRRPPeers != nil {
			peers = h.VRRPPeers()
		}
		w.Header().Set("Content-Type", "application/json")
		st := PeerStatus{NodeID: h.NodeID, Role: rs.Role, Epoch: rs.Epoch, HoldsVIP: holds, VRRPPeers: peers}
		if h.Controller.CatalogueMark != nil {
			st.Mark = h.Controller.CatalogueMark(r.Context())
		}
		if h.Controller.Healthy != nil {
			st.Healthy = h.Controller.Healthy(r.Context())
		}
		_ = json.NewEncoder(w).Encode(st)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/activate"):
		// 不提供 force：唯一的栅栏（对端仍在服务）本就不允许跳过，真要抢应去那台上停服务。
		// 不用请求的 context：通知脚本五秒后挂断，而接任前追平可能要几分钟。
		if err := h.Controller.Activate(context.WithoutCancel(r.Context()), "notify: "+r.RemoteAddr); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/standby"):
		// 用 NotifyBackup 而非 Standby：keepalived 在启动和 FAULT→BACKUP→MASTER 过程中也会触发这里，
		// 见 NotifyBackup。
		if err := h.Controller.NotifyBackup(r.Context(), "notify: "+r.RemoteAddr); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

// Status 经 HTTP 实现 PeerStatusClient，供激活守卫读取对端状态。
func (p Peer) Status(ctx context.Context) (PeerStatus, error) {
	resp, err := p.get(ctx, "/internal/ha/status")
	if err != nil {
		return PeerStatus{}, err
	}
	defer resp.Body.Close()
	var st PeerStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return PeerStatus{}, err
	}
	return st, nil
}
