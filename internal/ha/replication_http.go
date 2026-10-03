package ha

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// 复制通道：主机通过 HTTP 提供目录清单和复制流，备机来拉。用拉模式，主机侧无需知道备机在哪、是否存在。

// ReplicationSource 是处理器所需的那部分 ZFS 客户端能力。
type ReplicationSource interface {
	ListGUIDs(ctx context.Context, root string) (zfs.GUIDInventory, error)
	SendReplication(ctx context.Context, root, fromSnap, toSnap string, w io.Writer) error
	// SendDataset 只发送单个数据集、不含子数据集，见 zfs.SendDataset。
	SendDataset(ctx context.Context, dataset, from, origin, to string, w io.Writer) error
	SendResume(ctx context.Context, token string, w io.Writer) error
}

// ReplicationInventory 是清单应答：对端条目加上它们所在的根。各节点池名可能不同，
// 接收方需要发送方的根才能跨节点比较快照名。
type ReplicationInventory struct {
	Root    string          `json:"root"`
	Entries []zfs.GUIDEntry `json:"entries"`
	// Incomplete 列出本节点库里仍记录、池里却已没有的数据集。备机只能靠它区分两种情况：
	// 运维删了镜像（数据集与库行一起没了，删除应传播），还是写入者丢了数据集（跟随会因
	// `send -R` + `recv -F` 镜像发送方而把它们从全集群删掉，必须拒绝）。
	Incomplete []string `json:"incomplete,omitempty"`
}

// ReplicationHandler 在路由挂载的前缀下提供 /inventory 和 /stream。
// 每个请求都必须带集群令牌：流里是本机的全部镜像。
type ReplicationHandler struct {
	Source ReplicationSource
	Root   string
	Token  string
	// Incomplete 返回本节点库里仍记录、池里却没有的数据集（一致性检查的 missing_dataset），
	// 随清单下发，让备机拒绝镜像一个自相矛盾的写入者；nil 表示没有。
	Incomplete func(ctx context.Context) []string
	// RateMBPerSec 在每条流开始时读取，限制发送速率（0 或 nil 为不限）。同步与客户机 iSCSI
	// 共用上行，运维会在运行中调整，例如无客户机的维护窗口里放开。
	RateMBPerSec func() int
	// OnServed 在备机每次请求时触发：发完流时带上该快照，清单轮询时带 ""，都算心跳。
	// from 标识备机（X-ND-Node 头，否则用来源地址）：多备机时必须分别记录进度，否则计划切换
	// 可能把 VIP 交给持有陈旧副本的那台。这是主机判断备机是否存活、计算滞后告警的唯一依据。
	OnServed func(from, to string)
	// FullSends 设置时，在流运行期间记录正在接收全量副本（无基准或续传）的备机。
	FullSends *FullSends
}

// nodeHeader 携带请求节点自己的 API URL，让主机区分各台备机。
const nodeHeader = "X-ND-Node"

// pullerOf 返回复制请求背后的备机：优先用其声明的身份，否则用来源地址（旧客户端）。
func pullerOf(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(nodeHeader)); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return r.RemoteAddr
	}
	return host
}

// confirmedMarker 返回备机追平后报告已完整持有的轮次（见 Peer.Confirm）。只有它是本节点
// 目录根上自己的标记时才记录，其他情况视为普通心跳（""）。
func confirmedMarker(marker, root string, inv zfs.GUIDInventory) string {
	if !strings.HasPrefix(marker, storage.ReplicationSnapshotPrefix) {
		return ""
	}
	for _, e := range inv.Entries {
		if e.Name == root+"@"+marker {
			return marker
		}
	}
	return ""
}

func (h ReplicationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(h.Token)) != 1 {
		http.Error(w, "cluster token required", http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/inventory"):
		inv, err := h.Source.ListGUIDs(r.Context(), h.Root)
		if zfs.IsNotExist(err) {
			http.Error(w, "主机还没有数据池，暂无可复制的内容；请在「存储池管理」里给主机建数据池", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 同时报出本机自身对不上的地方，备机据此区分「运维删了镜像」与「本机丢了数据集」。
		var incomplete []string
		// 确认请求只报进度，应答会被丢弃。
		if h.Incomplete != nil && !r.URL.Query().Has("confirmed") {
			incomplete = h.Incomplete(r.Context())
		}
		_ = json.NewEncoder(w).Encode(ReplicationInventory{
			Root: h.Root, Entries: inv.Entries, Incomplete: incomplete})
		if h.OnServed != nil {
			h.OnServed(pullerOf(r), confirmedMarker(r.URL.Query().Get("confirmed"), h.Root, inv))
		}
	case strings.HasSuffix(r.URL.Path, "/stream") && r.URL.Query().Has("dataset"):
		h.serveDataset(w, r)
	case strings.HasSuffix(r.URL.Path, "/stream"):
		if token := r.URL.Query().Get("token"); token != "" {
			w.Header().Set("Content-Type", "application/octet-stream")
			defer h.FullSends.begin(pullerOf(r))()
			_ = h.Source.SendResume(r.Context(), token, NewRateLimitedWriter(w, h.rate()))
			return
		}
		to := r.URL.Query().Get("to")
		if to == "" {
			http.Error(w, "to is required", http.StatusBadRequest)
			return
		}
		from := r.URL.Query().Get("from")
		if from == "" {
			defer h.FullSends.begin(pullerOf(r))()
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// 从这里开始的失败都在流中途，只能靠断开连接表达，接收端 zfs recv 本来也会报错。
		if err := h.Source.SendReplication(r.Context(), h.Root, from, to, NewRateLimitedWriter(w, h.rate())); err != nil {
			return
		}
		if h.OnServed != nil {
			h.OnServed(pullerOf(r), to)
		}
	default:
		http.NotFound(w, r)
	}
}

var (
	datasetRel = regexp.MustCompile(`^(/[A-Za-z0-9_.:-]+)*$`)
	snapName   = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// serveDataset 为追平轮次发送目录里的单个数据集。名字相对目录根传来、在本节点的根下解析，
// 必须留在根内。发送根自身时记录进度：追平计划最后才发根，持有根标记即持有整轮。
func (h ReplicationHandler) serveDataset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rel, from, origin, to := q.Get("dataset"), q.Get("from"), q.Get("origin"), q.Get("to")
	originRel, originSnap, _ := strings.Cut(origin, "@")
	if !datasetRel.MatchString(rel) || strings.Contains(rel, "/..") || strings.Contains(rel, "/.") ||
		!snapName.MatchString(to) || (from != "" && !snapName.MatchString(from)) ||
		(origin != "" && (originRel == "" || !datasetRel.MatchString(originRel) || strings.Contains(originRel, "/.") || !snapName.MatchString(originSnap))) {
		http.Error(w, "invalid dataset or snapshot name", http.StatusBadRequest)
		return
	}
	dataset := h.Root + rel
	if from != "" {
		from = dataset + "@" + from
	}
	if origin != "" {
		origin = h.Root + origin
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	defer h.FullSends.beginDataset(pullerOf(r), rel)()
	if err := h.Source.SendDataset(r.Context(), dataset, from, origin, to, NewRateLimitedWriter(w, h.rate())); err != nil {
		return
	}
	if rel == "" && h.OnServed != nil {
		h.OnServed(pullerOf(r), to)
	}
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// Peer 是客户端一侧：备机读取主机目录的方式。
type Peer struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Self 是本节点自己的 API URL，随每个请求发送，让对端记录是谁在问。身份无关的探测可留空。
	Self string
	// StreamIdle 是复制流单次读取等待数据的上限，超时即放弃；零值用默认。
	StreamIdle time.Duration
}

// defaultStreamIdle：一条流可能跑几十分钟，所以不限总时长，只限等待下一批字节的时间。
// 否则停止发送的对端（VIP 移走、链路断了）会让接收循环永远挂住。
const defaultStreamIdle = 2 * time.Minute

// stream 打开一条复制流，读取在 StreamIdle 内无数据就放弃。只计等网络的时间，
// zfs recv 背后磁盘慢只会让读取变稀疏。
func (p Peer) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	idle := p.StreamIdle
	if idle <= 0 {
		idle = defaultStreamIdle
	}
	sctx, cancel := context.WithCancel(ctx)
	resp, err := p.get(sctx, path)
	if err != nil {
		cancel()
		return nil, err
	}
	return &idleReader{body: resp.Body, idle: idle, cancel: cancel}, nil
}

type idleReader struct {
	body   io.ReadCloser
	idle   time.Duration
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (r *idleReader) Read(b []byte) (int, error) {
	timer := time.AfterFunc(r.idle, func() { r.fired.Store(true); r.cancel() })
	n, err := r.body.Read(b)
	timer.Stop()
	if err != nil && r.fired.Load() {
		err = fmt.Errorf("对端 %s 没有数据发来，复制流已放弃：%w", r.idle, err)
	}
	return n, err
}

func (r *idleReader) Close() error {
	err := r.body.Close()
	r.cancel()
	return err
}

func (p Peer) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p Peer) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	if p.Self != "" {
		req.Header.Set(nodeHeader, p.Self)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("对端 %s 返回 %d: %s", p.BaseURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// Inventory 返回对端复制根下持有的内容，以对端自己的根命名。
func (p Peer) Inventory(ctx context.Context) (ReplicationInventory, error) {
	resp, err := p.get(ctx, "/internal/replication/inventory")
	if err != nil {
		return ReplicationInventory{}, err
	}
	defer resp.Body.Close()
	var inv ReplicationInventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		return ReplicationInventory{}, err
	}
	return inv, nil
}

// Confirm 报告本节点已完整持有 marker。追平轮次可能根本不发根（它随失败的递归接收落地），
// 没有这一步写入者永远记不到这一轮。
func (p Peer) Confirm(ctx context.Context, marker string) error {
	resp, err := p.get(ctx, "/internal/replication/inventory?confirmed="+url.QueryEscape(marker))
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// Stream 打开到快照 to 的复制流，给了 from 时为增量流。调用方负责关闭。
func (p Peer) Stream(ctx context.Context, from, to string) (io.ReadCloser, error) {
	path := "/internal/replication/stream?to=" + to
	if from != "" {
		path += "&from=" + from
	}
	return p.stream(ctx, path)
}

// StreamDataset 为追平轮次打开单个数据集的流：rel 相对目录根（"" 为根本身），
// from 为快照名，origin 为相对的克隆源（"/img@0"）。
func (p Peer) StreamDataset(ctx context.Context, rel, from, origin, to string) (io.ReadCloser, error) {
	q := url.Values{"dataset": {rel}, "to": {to}}
	if from != "" {
		q.Set("from", from)
	}
	if origin != "" {
		q.Set("origin", origin)
	}
	return p.stream(ctx, "/internal/replication/stream?"+q.Encode())
}

// StreamResume 用中断的接收留下的令牌继续传输。
func (p Peer) StreamResume(ctx context.Context, token string) (io.ReadCloser, error) {
	return p.stream(ctx, "/internal/replication/stream?token="+token)
}

func (h ReplicationHandler) rate() int {
	if h.RateMBPerSec == nil {
		return 0
	}
	return h.RateMBPerSec()
}
