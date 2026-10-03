package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/store"
)

// ClusterPoolView 是一个节点的池以及是否联系得上它。
type ClusterPoolView struct {
	NodeID    string         `json:"node_id"`
	IP        string         `json:"ip"`
	Role      string         `json:"role"`
	IsSelf    bool           `json:"is_self"`
	Online    bool           `json:"online"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Pools     []ops.PoolItem `json:"pools"`
}

// ClusterPoolsResult 是各节点视图和集群合计。合计只计读得到的，给没应答的节点算 0 会低估集群容量。
type ClusterPoolsResult struct {
	Nodes         []ClusterPoolView `json:"nodes"`
	TotalCapacity int64             `json:"total_capacity"`
	TotalUsed     int64             `json:"total_used"`
	PoolCount     int               `json:"pool_count"`
	Unreachable   int               `json:"unreachable"`
}

// ClusterPools 逐个询问节点自己的池，回答集群有多少存储、在哪里；库里的池行只有本机能填上实时数字。
type ClusterPools struct {
	Store   store.Store
	Local   func(ctx context.Context) ([]ops.PoolItem, error)
	NodeID  string
	Token   string
	Now     func() time.Time
	Timeout time.Duration
	// Fetch 读取一个远端节点的池；nil 时用集群 HTTP 通道。
	Fetch func(ctx context.Context, apiURL string) ([]ops.PoolItem, error)
}

func (c ClusterPools) List(ctx context.Context) (ClusterPoolsResult, error) {
	servers, err := c.Store.Servers().List(ctx)
	if err != nil {
		return ClusterPoolsResult{}, err
	}
	now := c.Now()
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 6 * time.Second
	}
	fetch := c.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, apiURL string) ([]ops.PoolItem, error) {
			return fetchNodePools(ctx, apiURL, c.Token)
		}
	}

	out := make([]ClusterPoolView, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		srv := servers[i]
		view := ClusterPoolView{
			NodeID: srv.ID, IP: srv.IP, Role: srv.HAState, IsSelf: srv.ID == c.NodeID,
			Online: srv.LastSeenAt != nil && now.Sub(*srv.LastSeenAt) <= place.Freshness,
		}
		if view.IsSelf {
			if pools, err := c.Local(ctx); err == nil {
				view.Pools, view.Reachable = pools, true
			} else {
				view.Error = err.Error()
			}
			out[i] = view
			continue
		}
		out[i] = view
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
			pools, err := fetch(nctx, apiURL)
			if err != nil {
				out[idx].Error = err.Error()
				return
			}
			out[idx].Pools, out[idx].Reachable = pools, true
		}(i, srv.APIURL)
	}
	wg.Wait()

	res := ClusterPoolsResult{Nodes: out}
	for _, n := range out {
		if !n.Reachable {
			res.Unreachable++
			continue
		}
		for _, p := range n.Pools {
			res.PoolCount++
			res.TotalCapacity += p.Capacity
			res.TotalUsed += p.Used
		}
	}
	sort.SliceStable(res.Nodes, func(i, j int) bool {
		if (res.Nodes[i].Role == "active") != (res.Nodes[j].Role == "active") {
			return res.Nodes[i].Role == "active"
		}
		return res.Nodes[i].IP < res.Nodes[j].IP
	})
	return res, nil
}

// NodePoolNames 实时询问某节点有哪些池：本机直接答，远端走集群通道。
// 与库里的池行不同，这是节点自己的说法，Forget 据此确认节点是否仍拥有某个池。
func (c ClusterPools) NodePoolNames(ctx context.Context, nodeID string) ([]string, error) {
	var items []ops.PoolItem
	var err error
	if nodeID == c.NodeID && c.Local != nil {
		items, err = c.Local(ctx)
	} else {
		srv, gerr := c.Store.Servers().Get(ctx, nodeID)
		if gerr != nil {
			return nil, gerr
		}
		if srv.APIURL == "" {
			return nil, fmt.Errorf("节点 %s 没有登记管理地址", nodeID)
		}
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		fctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if c.Fetch != nil {
			items, err = c.Fetch(fctx, srv.APIURL)
		} else {
			items, err = fetchNodePools(fctx, srv.APIURL, c.Token)
		}
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, p := range items {
		names = append(names, p.Name)
	}
	return names, nil
}

func fetchNodePools(ctx context.Context, apiURL, token string) ([]ops.PoolItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/pools", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &nodeError{status: resp.Status}
	}
	var out struct {
		Pools []ops.PoolItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Pools, nil
}
