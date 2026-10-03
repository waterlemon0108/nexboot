package ha

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

// NodeInfo 是集群成员在网络上交换的信息：身份、API 地址、客户机启动用的 IP，以及它当前声称的角色。
type NodeInfo struct {
	NodeID   string `json:"node_id"`
	APIURL   string `json:"api_url"`
	PortalIP string `json:"portal_ip"`
	Role     string `json:"role"`
	Epoch    int64  `json:"epoch,omitempty"`
	// LastSeenAt 是主机记录的该节点最近一次上报时间。只有收心跳的节点知道谁活着，
	// 所以随名册下发这个判断，而不是让每个读者自己猜。
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	// Pools 是节点实际拥有的池，供写入者据此对齐池记录。不上报的节点（旧版本）为 nil，不会替它改任何东西。
	Pools *PoolReport `json:"pools,omitempty"`
}

// PoolReport 是节点对自己池的上报。池在持有磁盘的节点上建删，记录写进该节点的库；
// 备机的库既不是目录也留不住（下次接任会被写入者的副本替换），所以写入者据此上报对账。
type PoolReport struct {
	// Present 是节点上当前已导入的全部产品池。
	Present []ReportedPool `json:"present"`
	// Known 是节点自己对其池的记录。写入者持有、两个列表里都没有的记录，说明池已在节点上销毁；
	// 只在 Known 里的是丢失了，保留给运维查看。
	Known []string `json:"known"`
}

// ReportedPool 是一个已导入的池：记录及其磁盘。
type ReportedPool struct {
	Pool  domain.Pool             `json:"pool"`
	Disks []domain.PoolDiskStatus `json:"disks"`
}

// NodeRegistry 是主机侧对注册的处理：upsert 行并返回名册。由 ops 基于 servers 表实现。
type NodeRegistry interface {
	Register(ctx context.Context, n NodeInfo) error
	Peers(ctx context.Context) ([]NodeInfo, error)
}

// JoinConfig 是节点成为可用成员所需的最小集群级密钥：JWT 签名密钥（接任后会话不失效）和
// iSCSI CHAP 密钥（客户机可重连到任一服务其 LUN 的节点）。故意不含管理员密码和目录，
// 它们在节点成为备机后随 DB 流复制过来。
type JoinConfig struct {
	JWTSecret  string `json:"jwt_secret"`
	CHAPSecret string `json:"chap_secret"`
	// Pool 是集群数据池名；加入节点没有本机池时用它作初值，见 JoinService.join。
	Pool string `json:"pool"`
	// Peers 是名册中各成员的真实地址。加入者据此生成 VRRP 单播 peer 列表并计算优先级排名；
	// 只有 VIP 的话会列出一个漂移的别名而不是机器。
	Peers []string `json:"peers,omitempty"`
}

// Addr 返回节点真实地址，即其 API URL 的主机部分（各节点都报自己的 IP，绝不是 VIP）。没有时退回 PortalIP。
func (n NodeInfo) Addr() string {
	if u, err := url.Parse(n.APIURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return n.PortalIP
}

// ClusterHandler 提供 /internal/cluster/：register 兼作加入与心跳（幂等 upsert），peers 返回名册，
// join-config 给界面加入的节点下发共享密钥。全部需令牌。
type ClusterHandler struct {
	Registry NodeRegistry
	Token    string
	// JWTSecret 和 CHAPSecret 是本（主机）节点的集群级密钥，经 join-config 下发给运维从界面加入的节点。
	JWTSecret  string
	CHAPSecret string
	// PoolName 在请求时返回集群数据池名（运行期选定且可重绑），保证 join-config 反映当前池名而非启动时的值。
	PoolName func() string
}

func (h ClusterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(h.Token)) != 1 {
		http.Error(w, "cluster token required", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/register"):
		var n NodeInfo
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.NodeID == "" {
			http.Error(w, "node_id required", http.StatusBadRequest)
			return
		}
		if err := h.Registry.Register(r.Context(), n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/peers"):
		peers, err := h.Registry.Peers(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peers)
	case strings.HasSuffix(r.URL.Path, "/join-config"):
		pool := ""
		if h.PoolName != nil {
			pool = h.PoolName()
		}
		var peers []string
		if roster, err := h.Registry.Peers(r.Context()); err == nil {
			for _, n := range roster {
				if a := n.Addr(); a != "" {
					peers = append(peers, a)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JoinConfig{JWTSecret: h.JWTSecret, CHAPSecret: h.CHAPSecret, Pool: pool, Peers: peers})
	default:
		http.NotFound(w, r)
	}
}

// RegisterSelf 向对端（通常是 VIP，即当前主机）上报本节点身份。加入和心跳是同一个幂等调用。
func (p Peer) RegisterSelf(ctx context.Context, n NodeInfo) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/internal/cluster/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register: %s", resp.Status)
	}
	return nil
}

// JoinConfig 从对端（VIP，即当前主机）拉取集群共享密钥，界面加入的节点据此自行配置，无需运维手抄密钥。
func (p Peer) JoinConfig(ctx context.Context) (JoinConfig, error) {
	resp, err := p.get(ctx, "/internal/cluster/join-config")
	if err != nil {
		return JoinConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JoinConfig{}, fmt.Errorf("join-config: %s", resp.Status)
	}
	var jc JoinConfig
	if err := json.NewDecoder(resp.Body).Decode(&jc); err != nil {
		return JoinConfig{}, err
	}
	return jc, nil
}

// ClusterPeers 从对端（通常是 VIP）读取名册。
func (p Peer) ClusterPeers(ctx context.Context) ([]NodeInfo, error) {
	resp, err := p.get(ctx, "/internal/cluster/peers")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var peers []NodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return nil, err
	}
	return peers, nil
}

// ProbeQuorum 统计给定对端中有多少在超时内应答状态探测，并返回见到的 epoch 最高的 active 声明（没有则为 nil）。
func ProbeQuorum(ctx context.Context, peers []Peer, timeout time.Duration) (reached int, activeClaim *PeerStatus) {
	for i := range peers {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		st, err := peers[i].Status(pctx)
		cancel()
		if err != nil {
			continue
		}
		reached++
		if st.Role == "active" {
			if activeClaim == nil || st.Epoch > activeClaim.Epoch {
				c := st
				activeClaim = &c
			}
		}
	}
	return reached, activeClaim
}
