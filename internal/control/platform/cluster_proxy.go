package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/store"
)

// ProxiedResponse 是对端的完整应答。
type ProxiedResponse struct {
	Status      int
	Body        []byte
	ContentType string
}

// ClusterProxy 把存储写操作转发给拥有磁盘的节点：池操作本质上只能在盘所在的节点执行。
type ClusterProxy struct {
	Store   store.Store
	NodeID  string
	Token   string
	SelfURL string
	Timeout time.Duration
}

func (c ClusterProxy) Forward(ctx context.Context, node, method, suffix, query string, body io.Reader) (ProxiedResponse, error) {
	base, err := c.nodeURL(ctx, node)
	if err != nil {
		return ProxiedResponse{}, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := base + "/internal/node/pools"
	if suffix != "" {
		target += "/" + strings.TrimPrefix(suffix, "/")
	}
	// 带上目标节点名，让对端拒绝发错的操作：节点表条目可能比机器活得久，在错的主机上销毁池无法靠重试挽回。
	q := url.Values{}
	if query != "" {
		if parsed, err := url.ParseQuery(query); err == nil {
			q = parsed
		}
	}
	q.Set("node", node)
	target += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return ProxiedResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ProxiedResponse{}, fmt.Errorf("节点 %s 未响应：%w", node, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ProxiedResponse{}, err
	}
	return ProxiedResponse{Status: resp.StatusCode, Body: payload, ContentType: resp.Header.Get("Content-Type")}, nil
}

// nodeURL 不猜测目标节点。本机也走同一端点的回环副本而不短路，确保每条路径都做目标节点核对，
// 包括 VIP 迁移后「本机」已换成另一台机器的情况。
func (c ClusterProxy) nodeURL(ctx context.Context, node string) (string, error) {
	if node == c.NodeID && c.SelfURL != "" {
		return c.SelfURL, nil
	}
	srv, err := c.Store.Servers().Get(ctx, node)
	if err != nil {
		return "", fmt.Errorf("节点 %s 不在集群花名册里：%w", node, err)
	}
	if srv.APIURL == "" {
		return "", fmt.Errorf("节点 %s 没有登记管理地址，无法远程操作", node)
	}
	return srv.APIURL, nil
}
