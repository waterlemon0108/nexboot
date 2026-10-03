package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/store"
)

// ClusterServiceView 是服务矩阵中一个节点的一列。
type ClusterServiceView struct {
	NodeID    string        `json:"node_id"`
	IP        string        `json:"ip"`
	Role      string        `json:"role"`
	IsSelf    bool          `json:"is_self"`
	Online    bool          `json:"online"`
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error,omitempty"`
	Services  []ServiceItem `json:"services"`
	// Host 是该机器自报的主机名、OS、运行时长和版本；节点未应答时为 nil。
	Host *HostFacts `json:"host,omitempty"`
}

// ClusterServicesResult 是整个矩阵：每个节点一项，带同一份单元白名单，供控制台按行排布。
type ClusterServicesResult struct {
	Nodes []ClusterServiceView `json:"nodes"`
}

// ClusterServices 逐个询问节点自己的服务状态。节点只能看到自己的 systemd，
// 所以由写入者经集群通道汇总，否则控制台只能看到持有 VIP 的那台。
type ClusterServices struct {
	Store store.Store
	// Local 不经网络直接回答本节点。
	Local func(ctx context.Context) (ServiceListResult, error)
	// SelfRole 是本节点的自报角色。节点表是复制副本、天然滞后，备机上可能仍把已退位的节点
	// 记成写入者，画出两个主机。以第一手为准。
	SelfRole func() string
	NodeID   string
	Token    string
	Now      func() time.Time
	// Timeout 限制单个节点的应答时间，慢或失联的节点不能拖住整个矩阵。
	Timeout time.Duration
	// Fetch 读取一个远端节点的服务；nil 时用集群 HTTP 通道。
	Fetch func(ctx context.Context, apiURL string) (ServiceListResult, error)
}

func (c ClusterServices) List(ctx context.Context) (ClusterServicesResult, error) {
	servers, err := c.Store.Servers().List(ctx)
	if err != nil {
		return ClusterServicesResult{}, err
	}
	now := c.Now()
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 6 * time.Second
	}

	fetch := c.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, apiURL string) (ServiceListResult, error) {
			return fetchNodeServices(ctx, apiURL, c.Token)
		}
	}

	out := make([]ClusterServiceView, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		srv := servers[i]
		view := ClusterServiceView{
			NodeID: srv.ID, IP: srv.IP, Role: srv.HAState,
			IsSelf: srv.ID == c.NodeID,
			Online: srv.LastSeenAt != nil && now.Sub(*srv.LastSeenAt) <= place.Freshness,
		}
		if view.IsSelf {
			if c.SelfRole != nil {
				if r := c.SelfRole(); r != "" {
					view.Role = r
				}
			}
			if res, err := c.Local(ctx); err == nil {
				view.Services, view.Reachable, view.Host = res.Items, true, res.Host
			} else {
				view.Error = err.Error()
			}
			out[i] = view
			continue
		}
		out[i] = view
		// 节点表已标记离线的节点直接跳过，不拿整个请求去换一个必然的超时。
		if srv.APIURL == "" || !view.Online {
			if !view.Online {
				out[i].Error = "节点失联"
			}
			continue
		}
		wg.Add(1)
		go func(idx int, apiURL string) {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			res, err := fetch(nctx, apiURL)
			if err != nil {
				out[idx].Error = err.Error()
				return
			}
			out[idx].Services, out[idx].Reachable, out[idx].Host = res.Items, true, res.Host
			if res.Role != "" {
				out[idx].Role = res.Role // 第一手，优先于节点表副本
			}
		}(i, srv.APIURL)
	}
	wg.Wait()

	// 写入者排第一，其余按地址：提供服务的节点是操作者最先看的。
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Role == "active") != (out[j].Role == "active") {
			return out[i].Role == "active"
		}
		return out[i].IP < out[j].IP
	})
	return ClusterServicesResult{Nodes: out}, nil
}

func fetchNodeServices(ctx context.Context, apiURL, token string) (ServiceListResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/services", nil)
	if err != nil {
		return ServiceListResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ServiceListResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ServiceListResult{}, &nodeError{status: resp.Status}
	}
	var res ServiceListResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ServiceListResult{}, err
	}
	return res, nil
}

type nodeError struct{ status string }

func (e *nodeError) Error() string { return "节点无响应: " + e.status }
