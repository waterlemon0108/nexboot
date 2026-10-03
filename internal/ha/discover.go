package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotAdoptable 表示节点拒绝被纳管：它有池或已在集群中。原样展示，因为处理方式不同于
// 「连不上」：那台机器必须在它自己的界面上加入。
var ErrNotAdoptable = errors.New("该节点不是出厂态，不接受自动纳管")

// Candidate 是控制台可以提议加入的一台机器。
type Candidate struct {
	NodeID  string `json:"node_id"`
	Version string `json:"version"`
	// Address 是 host:port，正是 AdoptNode 接受的格式。
	Address string `json:"address"`
}

// Discovery 在客户机网段上发现未配置节点并纳管它们，让加机器变成「指一下」，
// 而不是走过去重敲 VIP 和令牌，集群本来就知道这两样。
type Discovery struct {
	// VIP 和 Token 是集群下发给被纳管节点的内容。
	VIP   string
	Token string
	// Candidates 列出值得探测的 host:port 地址（客户机子网）。
	Candidates func(ctx context.Context) []string
	// InCluster 报告节点 ID 是否已在名册中；已在的无论自称多空都不是候选。
	InCluster func(nodeID string) bool
	// Timeout 限定每次探测，安静的地址不能拖住整个扫描。
	Timeout time.Duration
	HTTP    *http.Client
}

func (d Discovery) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 2 * time.Second
}

func (d Discovery) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: d.timeout()}
}

// Scan 并行探测所有候选并返回可纳管的。不可达地址直接不算候选：扫子网时遇到的大多不是我们的机器。
func (d Discovery) Scan(ctx context.Context) ([]Candidate, error) {
	if d.Candidates == nil {
		return nil, nil
	}
	addrs := d.Candidates(ctx)
	out := make([]Candidate, len(addrs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64) // 别一次拉满：一个 /24 就是 254 个连接
	for i, addr := range addrs {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, d.timeout())
			defer cancel()
			id, err := Peer{BaseURL: "http://" + addr, HTTP: d.client()}.Identity(pctx)
			if err != nil || !id.Adoptable || id.NodeID == "" {
				return
			}
			if d.InCluster != nil && d.InCluster(id.NodeID) {
				return
			}
			out[i] = Candidate{NodeID: id.NodeID, Version: id.Version, Address: addr}
		}(i, addr)
	}
	wg.Wait()
	found := make([]Candidate, 0, len(out))
	for _, c := range out {
		if c.NodeID != "" {
			found = append(found, c)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Address < found[j].Address })
	return found, nil
}

// AdoptNode 纳管一个节点：提交集群自己的 VIP 和令牌，运维无需抄写。
func (d Discovery) AdoptNode(ctx context.Context, addr string) error {
	body, err := json.Marshal(JoinRequest{VIP: d.VIP, ClusterToken: d.Token})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/internal/cluster/adopt", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("节点 %s 未响应：%w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(msg))
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w：%s", ErrNotAdoptable, detail)
	}
	return fmt.Errorf("纳管节点 %s 失败（%s）：%s", addr, resp.Status, detail)
}

// SubnetCandidates 列出 cidr 内与 port 组合的所有主机地址，跳过本节点已在应答的地址。
// 这是默认候选集：新节点会接在客户机网络上。
func SubnetCandidates(cidr, port string, skip map[string]bool) []string {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	ones, bits := network.Mask.Size()
	if bits-ones > 12 { // 大于 /20 不扫：那不是一个机房网段，是一次网络扫荡
		return nil
	}
	var out []string
	for ip := network.IP.Mask(network.Mask); network.Contains(ip); ip = nextIP(ip) {
		s := ip.String()
		if strings.HasSuffix(s, ".0") || strings.HasSuffix(s, ".255") || skip[s] {
			continue
		}
		out = append(out, net.JoinHostPort(s, port))
	}
	return out
}

func nextIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}
