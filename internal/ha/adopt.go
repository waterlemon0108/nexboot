package ha

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// NodeIdentity 是未配置节点对任何询问者的自我介绍。不含任何密钥：只用于让集群控制台列出
// 本网段哪些空机器可以加入，不给陌生人任何可利用的东西。
type NodeIdentity struct {
	NodeID  string `json:"node_id"`
	Version string `json:"version"`
	// Adoptable 只在出厂态为 true：既无数据池也不在集群中。
	Adoptable bool `json:"adoptable"`
}

// AdoptHandler 提供全新节点对外开放的两个端点：identity（我是谁、是否为空）和 adopt（集群纳管我）。
// 两者都不鉴权，这是有意且有边界的取舍：只有什么都没有（无池即无镜像、配置、客户机）的节点
// 才可纳管，陌生人最多把一台空机器拉进自己的集群。有池的节点一律不可纳管，因为纳管会让复制流
// 回滚它的目录，造成静默丢数据，这类节点必须在它自己的界面上手动加入。
// 不想要该机制的站点设 NDISKLESS_ADOPT_OFF=1。
type AdoptHandler struct {
	NodeID  string
	Version string
	// Factory 报告是否处于可纳管状态：无数据池且不在集群中。
	Factory func(ctx context.Context) bool
	// Disabled 为希望每台节点都手动加入的站点关闭整个机制。
	Disabled bool
	// Adopt 执行纳管：与界面加入同一路径，只是不要求已有池。
	Adopt func(ctx context.Context, req JoinRequest) error
}

func (h AdoptHandler) adoptable(ctx context.Context) bool {
	return !h.Disabled && h.Factory != nil && h.Factory(ctx)
}

func (h AdoptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/identity"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(NodeIdentity{
			NodeID: h.NodeID, Version: h.Version, Adoptable: h.adoptable(r.Context()),
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adopt"):
		if h.Disabled {
			http.Error(w, "本机已关闭自动纳管（NDISKLESS_ADOPT_OFF），请在本机界面手动加入集群", http.StatusForbidden)
			return
		}
		if h.Factory == nil || !h.Factory(r.Context()) {
			http.Error(w, "本机不是出厂态（已有数据池或已在集群中），不接受自动纳管。"+
				"如果确实要把它加入集群，请到它自己的界面上手动加入", http.StatusConflict)
			return
		}
		var req JoinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "请求格式不正确", http.StatusBadRequest)
			return
		}
		if err := h.Adopt(r.Context(), req); err != nil {
			http.Error(w, err.Error(), joinStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "adopting", "role": "standby", "node_id": h.NodeID,
			"detail": "已受理纳管：本机将重启为备机加入集群，随后请在集群控制台给它建数据池",
		})
	default:
		http.NotFound(w, r)
	}
}

// Identity 询问节点身份及是否可被纳管，集群控制台扫描客户机网段时使用。
func (p Peer) Identity(ctx context.Context) (NodeIdentity, error) {
	resp, err := p.get(ctx, "/internal/cluster/identity")
	if err != nil {
		return NodeIdentity{}, err
	}
	defer resp.Body.Close()
	var id NodeIdentity
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		return NodeIdentity{}, err
	}
	return id, nil
}
