package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// JoinRequest 是运维在「加入集群」表单里填写的全部内容：集群 VIP 和集群令牌。
// 其余（本机地址、共享密钥、池名）由节点自行推导或拉取。
type JoinRequest struct {
	VIP          string `json:"vip"`
	ClusterToken string `json:"cluster_token"`
}

// JoinParams 是重配置步骤所需的参数，即原本要在安装器命令行上手敲的那些，现在自动收集。
type JoinParams struct {
	VIP        string
	Token      string
	NodeAddr   string
	Pool       string
	JWTSecret  string
	CHAPSecret string
	// Peers 是其他各成员的真实地址，作为加入者初始的 VRRP 单播列表。
	// 已滤掉自己和 VIP：VIP 是当前 master 的别名而非一台机器。
	Peers []string
	// Role 是本节点的初始角色：建集群的第一台为 "active"，之后加入的都是 "standby"。
	// 它也决定 keepalived 是否立即启动：加入者必须先出现在写入者的 peer 列表里，否则收不到通告会自选为 master。
	Role string
}

// StartsKeepalivedNow 判断本节点能否立即启动 VRRP：集群第一台可以（唯一候选），加入者不行。
func (p JoinParams) StartsKeepalivedNow() bool { return p.Role == "active" }

// 加入节点可能遇到的拒绝，映射到不同 HTTP 状态码，让界面区分「需要在本机处理」和「集群连不上」。
var (
	ErrAlreadyClustered   = errors.New("本机已在集群中，无需再次加入")
	ErrNoPoolToJoin       = errors.New("请先在本机建好数据池，再加入集群")
	ErrClusterUnreachable = errors.New("连接集群失败，请检查 VIP、令牌和网络")
)

// JoinService 是界面发起入盟时加入节点一侧的逻辑：检查本机就绪（有池、尚未入盟），
// 从 VIP 拉取共享密钥，再交给分离运行的重配置把本节点变为备机。主机侧只提供 join-config，不反向操作加入节点。
type JoinService struct {
	// AlreadyClustered 报告是否已配置了 peer；再次加入几乎肯定不是运维本意。
	AlreadyClustered func() bool
	// HasPool 报告本节点是否有可接收复制的数据池。提前拒绝，免得加入一台收不了副本的节点。
	HasPool func(ctx context.Context) bool
	// LocalPool 返回本节点数据池名；有池时保留本机池名，为空时用集群下发的池名作初值。
	LocalPool func() string
	// APIPort 是集群 API（及 VIP）监听的端口。
	APIPort string
	// FetchConfig 从 VIP 拉取集群密钥和池名。注入式，测试无需真实对端。
	FetchConfig func(ctx context.Context, vipBase, token string) (JoinConfig, error)
	// ResolveSelfAddr 返回本节点朝向 VIP 的可达 IP，即其他节点和客户机会连的地址（绝不是 VIP）。
	ResolveSelfAddr func(vip string) (string, error)
	// Reconfigure 启动分离运行的安装器：写 env、加入 keepalived、以备机重启本节点。注入式，便于测试。
	Reconfigure func(ctx context.Context, p JoinParams) error
}

// Join 执行加入节点的检查，通过后启动重配置。返回带类型的错误，供 HTTP 层映射状态码。
func (s JoinService) Join(ctx context.Context, req JoinRequest) error {
	return s.join(ctx, req, true)
}

// Adopt 是对出厂态被集群纳管的节点执行的 Join，不做池检查：可纳管的节点本来就没有池，
// 这正是它可以安全纳管的原因，之后运维在集群控制台用这里下发的池名给它建池。
func (s JoinService) Adopt(ctx context.Context, req JoinRequest) error {
	return s.join(ctx, req, false)
}

func (s JoinService) join(ctx context.Context, req JoinRequest, requirePool bool) error {
	req.VIP, req.ClusterToken = strings.TrimSpace(req.VIP), strings.TrimSpace(req.ClusterToken)
	if req.VIP == "" || req.ClusterToken == "" {
		return errors.New("请填写集群 VIP 和集群令牌")
	}
	if err := validateVIP(req.VIP); err != nil {
		return err
	}
	if s.AlreadyClustered != nil && s.AlreadyClustered() {
		return ErrAlreadyClustered
	}
	if requirePool && s.HasPool != nil && !s.HasPool(ctx) {
		return ErrNoPoolToJoin
	}
	vipBase := fmt.Sprintf("http://%s:%s", req.VIP, s.APIPort)
	jc, err := s.FetchConfig(ctx, vipBase, req.ClusterToken)
	if err != nil {
		return fmt.Errorf("%w：%v", ErrClusterUnreachable, err)
	}
	self, err := s.ResolveSelfAddr(req.VIP)
	if err != nil {
		return fmt.Errorf("无法确定本机地址：%v", err)
	}
	// 本机已有池就保留自己的池名，改成集群的会让它找不到自己的数据。
	// 还没有池（出厂态被纳管）时才用集群池名作初值。
	pool := jc.Pool
	if s.LocalPool != nil {
		if local := strings.TrimSpace(s.LocalPool()); local != "" {
			pool = local
		}
	}
	var peers []string
	for _, p := range jc.Peers {
		if p = strings.TrimSpace(p); p != "" && p != self && p != req.VIP {
			peers = append(peers, p)
		}
	}
	params := JoinParams{
		VIP:        req.VIP,
		Token:      req.ClusterToken,
		NodeAddr:   self,
		Pool:       pool,
		JWTSecret:  jc.JWTSecret,
		CHAPSecret: jc.CHAPSecret,
		Peers:      peers,
		Role:       "standby",
	}
	if err := params.validate(); err != nil {
		return err
	}
	return s.Reconfigure(ctx, params)
}

// ServeHTTP 处理 POST /api/cluster/join，成功返回 202。实际工作（改 env、keepalived、重启）
// 在分离进程中进行，这样本节点才能在请求处理中途重启。
func (s JoinService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJoinError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	err := s.Join(r.Context(), req)
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "joining",
			"role":   "standby",
			"detail": "已开始加入集群，本机将重启为备机；稍候在集群节点里查看是否入盟",
		})
		return
	}
	writeJoinError(w, joinStatus(err), err.Error())
}

// joinStatus 把加入失败映射为界面据以区分的 HTTP 状态码。
func joinStatus(err error) int {
	switch {
	case errors.Is(err, ErrAlreadyClustered):
		return http.StatusConflict
	case errors.Is(err, ErrClusterUnreachable):
		return http.StatusBadGateway
	default:
		// 缺池、字段为空等：都属于「需要在本机处理」。
		return http.StatusBadRequest
	}
}

// writeJoinError 用纯文本应答，让文案原样到达运维：前端会直接展示失败请求的 body，这些文案本身就是操作指引。
func writeJoinError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}
