package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/store"
)

// ClusterDisks 返回指定节点上的空闲盘。在别的节点建池必须看那台的盘：同一个 /dev/sdb
// 在每台机器上是不同的盘，选错可能是系统盘或别的池成员，而界面上看不出区别。
type ClusterDisks struct {
	Store   store.Store
	NodeID  string
	Token   string
	Timeout time.Duration
	Local   func(ctx context.Context) (ops.DiskListResult, error)
	Fetch   func(ctx context.Context, apiURL string) (ops.DiskListResult, error)
}

func (c ClusterDisks) List(ctx context.Context, node string) (ops.DiskListResult, error) {
	if node == "" || node == c.NodeID {
		return c.Local(ctx)
	}
	srv, err := c.Store.Servers().Get(ctx, node)
	if err != nil {
		return ops.DiskListResult{}, fmt.Errorf("节点 %s 不在集群花名册里：%w", node, err)
	}
	if srv.APIURL == "" {
		return ops.DiskListResult{}, fmt.Errorf("节点 %s 没有登记管理地址", node)
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetch := c.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, apiURL string) (ops.DiskListResult, error) {
			return fetchNodeDisks(ctx, apiURL, c.Token)
		}
	}
	return fetch(ctx, srv.APIURL)
}

func fetchNodeDisks(ctx context.Context, apiURL, token string) (ops.DiskListResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/disks", nil)
	if err != nil {
		return ops.DiskListResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ops.DiskListResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ops.DiskListResult{}, &nodeError{status: resp.Status}
	}
	var out ops.DiskListResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ops.DiskListResult{}, err
	}
	return out, nil
}
