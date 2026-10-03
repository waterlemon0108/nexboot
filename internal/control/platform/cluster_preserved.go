package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// PreservedCopyView 是一个节点留存的目录副本。Reachable 为 false 表示没问到，不代表没有。
type PreservedCopyView struct {
	NodeID    string                  `json:"node_id"`
	IP        string                  `json:"ip"`
	IsSelf    bool                    `json:"is_self"`
	Reachable bool                    `json:"reachable"`
	Error     string                  `json:"error,omitempty"`
	Copies    []storage.PreservedCopy `json:"copies"`
}

type ClusterPreservedResult struct {
	Nodes []PreservedCopyView `json:"nodes"`
	Total int                 `json:"total"`
}

// ClusterPreserved 询问每个节点留存了什么。副本在留存它的节点上（通常是备机），
// 而告警只在写入者上评估，所以要由写入者去问。
type ClusterPreserved struct {
	Store   store.Store
	Local   func(ctx context.Context) ([]storage.PreservedCopy, error)
	NodeID  string
	Token   string
	Now     func() time.Time
	Timeout time.Duration
	// Fetch 读取一个远端节点的副本；nil 时用集群 HTTP 通道。
	Fetch func(ctx context.Context, apiURL string) ([]storage.PreservedCopy, error)
}

func (c ClusterPreserved) List(ctx context.Context) (ClusterPreservedResult, error) {
	servers, err := c.Store.Servers().List(ctx)
	if err != nil {
		return ClusterPreservedResult{}, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 6 * time.Second
	}
	fetch := c.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, apiURL string) ([]storage.PreservedCopy, error) {
			return fetchNodePreserved(ctx, apiURL, c.Token)
		}
	}
	out := make([]PreservedCopyView, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		srv := servers[i]
		view := PreservedCopyView{NodeID: srv.ID, IP: srv.IP, IsSelf: srv.ID == c.NodeID}
		if view.IsSelf {
			if copies, err := c.Local(ctx); err == nil {
				view.Copies, view.Reachable = copies, true
			} else {
				view.Error = err.Error()
			}
			out[i] = view
			continue
		}
		out[i] = view
		if srv.APIURL == "" {
			out[i].Error = "节点地址未知"
			continue
		}
		wg.Add(1)
		go func(idx int, apiURL string) {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			copies, err := fetch(nctx, apiURL)
			if err != nil {
				out[idx].Error = err.Error()
				return
			}
			out[idx].Copies, out[idx].Reachable = copies, true
		}(i, srv.APIURL)
	}
	wg.Wait()

	res := ClusterPreservedResult{Nodes: out}
	for _, n := range out {
		res.Total += len(n.Copies)
	}
	sort.SliceStable(res.Nodes, func(i, j int) bool { return res.Nodes[i].IP < res.Nodes[j].IP })
	return res, nil
}

func fetchNodePreserved(ctx context.Context, apiURL, token string) ([]storage.PreservedCopy, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/preserved", nil)
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
		Items []storage.PreservedCopy `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
