package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/store"
)

// ClusterBackupView 是一个节点的备份情况。
type ClusterBackupView struct {
	NodeID    string `json:"node_id"`
	IP        string `json:"ip"`
	Role      string `json:"role"`
	Online    bool   `json:"online"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	ops.NodeBackupStatus
}

// ClusterBackupsResult 是全局备份情况，外加最关键的数字：当前实际持有副本的机器数。
type ClusterBackupsResult struct {
	Nodes     []ClusterBackupView `json:"nodes"`
	Protected int                 `json:"protected"`
	Total     int                 `json:"total"`
}

// ClusterBackups 询问每个节点自己的池里有什么。只问一台只能看到持有 VIP 的机器，
// 而按节点备份的意义恰在于副本不随 VIP 移动。
type ClusterBackups struct {
	Store   store.Store
	Local   func(ctx context.Context) (ops.NodeBackupStatus, error)
	NodeID  string
	Token   string
	Now     func() time.Time
	Timeout time.Duration
}

func (c ClusterBackups) List(ctx context.Context) (ClusterBackupsResult, error) {
	servers, err := c.Store.Servers().List(ctx)
	if err != nil {
		return ClusterBackupsResult{}, err
	}
	now := c.Now()
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 6 * time.Second
	}
	out := make([]ClusterBackupView, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		srv := servers[i]
		view := ClusterBackupView{
			NodeID: srv.ID, IP: srv.IP, Role: srv.HAState,
			Online: srv.LastSeenAt != nil && now.Sub(*srv.LastSeenAt) <= place.Freshness,
		}
		if srv.ID == c.NodeID {
			if st, err := c.Local(ctx); err == nil {
				view.NodeBackupStatus, view.Reachable = st, true
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
			st, err := fetchNodeBackup(nctx, apiURL, c.Token)
			if err != nil {
				out[idx].Error = err.Error()
				return
			}
			out[idx].NodeBackupStatus, out[idx].Reachable = st, true
		}(i, srv.APIURL)
	}
	wg.Wait()

	res := ClusterBackupsResult{Nodes: out, Total: len(out)}
	for _, n := range out {
		// 有副本才算受保护，只配了备份池而池里没东西的节点不算。
		if n.Reachable && n.LastBackupAt != nil {
			res.Protected++
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

func fetchNodeBackup(ctx context.Context, apiURL, token string) (ops.NodeBackupStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/backup", nil)
	if err != nil {
		return ops.NodeBackupStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ops.NodeBackupStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ops.NodeBackupStatus{}, &nodeError{status: resp.Status}
	}
	var out ops.NodeBackupStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ops.NodeBackupStatus{}, err
	}
	return out, nil
}
