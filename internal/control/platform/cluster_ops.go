package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tianwei/diskless/internal/store"
)

// ErrWrongNode 是节点被要求以别的节点身份执行时的应答：控制台点名目标，节点先核对再执行。
var ErrWrongNode = fmt.Errorf("请求指名的节点不是本机")

// AssertSelf 拒绝发给别的节点的操作。节点表里的陈旧地址可能已指向另一台机器，
// 重启错机器的 iSCSI 不会报错，只会让另一批客户机静默掉盘。want 为空表示未点名（本机直接调用），照旧放行。
func AssertSelf(self, want string) error {
	if want == "" || want == self {
		return nil
	}
	return fmt.Errorf("%w：本机是 %s，请求要的是 %s", ErrWrongNode, self, want)
}

// ClusterServiceOps 在集群任一节点上执行服务操作或读日志，不限于控制台当前连着的那台，
// 免得操作者在最需要时还得 SSH 去对端重启 dnsmasq。
type ClusterServiceOps struct {
	Store       store.Store
	NodeID      string
	Token       string
	Now         func() time.Time
	Timeout     time.Duration
	LocalAction func(ctx context.Context, key, action string) error
	LocalLogs   func(ctx context.Context, key string, lines int) (string, error)
	// Post/Fetch 访问对端；nil 时用集群 HTTP 通道。
	Post  func(ctx context.Context, apiURL, node, key, action string) error
	Fetch func(ctx context.Context, apiURL, node, key string, lines int) (string, error)
}

func (c ClusterServiceOps) Action(ctx context.Context, nodeID, key, action string) error {
	if nodeID == "" || nodeID == c.NodeID {
		return c.LocalAction(ctx, key, action)
	}
	apiURL, err := c.peerURL(ctx, nodeID)
	if err != nil {
		return err
	}
	post := c.Post
	if post == nil {
		post = c.postAction
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	return post(ctx, apiURL, nodeID, key, action)
}

func (c ClusterServiceOps) Logs(ctx context.Context, nodeID, key string, lines int) (string, error) {
	if nodeID == "" || nodeID == c.NodeID {
		return c.LocalLogs(ctx, key, lines)
	}
	apiURL, err := c.peerURL(ctx, nodeID)
	if err != nil {
		return "", err
	}
	fetch := c.Fetch
	if fetch == nil {
		fetch = c.fetchLogs
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	return fetch(ctx, apiURL, nodeID, key, lines)
}

func (c ClusterServiceOps) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// peerURL 不猜测：节点表里没有的节点不能操作，随便挑一台会对操作者没点名的机器执行。
func (c ClusterServiceOps) peerURL(ctx context.Context, nodeID string) (string, error) {
	srv, err := c.Store.Servers().Get(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("节点 %s 不在集群花名册里：%w", nodeID, err)
	}
	if srv.APIURL == "" {
		return "", fmt.Errorf("节点 %s 没有登记管理地址，无法远程操作", nodeID)
	}
	return srv.APIURL, nil
}

func (c ClusterServiceOps) postAction(ctx context.Context, apiURL, node, key, action string) error {
	body, _ := json.Marshal(map[string]string{"action": action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/internal/node/services/%s/action?node=%s", apiURL, url.PathEscape(key), url.QueryEscape(node)),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("节点 %s 拒绝了该操作：%s", node, resp.Status)
	}
	return nil
}

func (c ClusterServiceOps) fetchLogs(ctx context.Context, apiURL, node, key string, lines int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/internal/node/services/%s/logs?node=%s&lines=%d", apiURL, url.PathEscape(key), url.QueryEscape(node), lines), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("节点 %s 未能返回日志：%s", node, resp.Status)
	}
	var out struct {
		Logs string `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Logs, nil
}
