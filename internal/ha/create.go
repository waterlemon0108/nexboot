package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CreateRequest 是「把本机建成集群」表单的全部内容：一个地址。
// 故意不含集群令牌：它是机器间的共享密钥，让运维自己编只是把生成值过一遍人手；由节点生成、存进 env，再下发给纳管的节点。
type CreateRequest struct {
	VIP string `json:"vip"`
}

// CreateService 把单机节点变成集群的第一台：生成令牌、确认地址是 keepalived 能挂上的，并把本机重配置为主机。
// 之后加节点、给它们建池都在本机控制台经 VIP 完成。
type CreateService struct {
	AlreadyClustered func() bool
	// LocalPool 可以为空：集群可以先于任何池建立，之后和其他节点一样经 VIP 建池。
	LocalPool func() string
	// OnLocalSubnet 报告是否有本地网卡与 VIP 同网段，keepalived 必须把地址挂到其中一块上。
	OnLocalSubnet   func(vip string) bool
	ResolveSelfAddr func(vip string) (string, error)
	// AddressInUse 返回已在使用该地址的对象（分组区间、客户机），nil 时跳过检查。
	AddressInUse func(ctx context.Context, ip string) []string
	NewToken     func() (string, error)
	Reconfigure  func(ctx context.Context, p JoinParams) error
}

func (s CreateService) Create(ctx context.Context, req CreateRequest) error {
	vip := strings.TrimSpace(req.VIP)
	if vip == "" {
		return errors.New("请填写虚 IP（客户机与管理页面此后都用它）")
	}
	if err := validateVIP(vip); err != nil {
		return err
	}
	if s.AlreadyClustered != nil && s.AlreadyClustered() {
		return ErrAlreadyClustered
	}
	if s.OnLocalSubnet != nil && !s.OnLocalSubnet(vip) {
		return fmt.Errorf("虚 IP %s 不在本机任何网卡的网段内，keepalived 无处挂载它；"+
			"请选一个客户机网段内、尚未被占用、且不落在任何分组 IP 区间里的地址", vip)
	}
	if s.AddressInUse != nil {
		if users := s.AddressInUse(ctx, vip); len(users) > 0 {
			return fmt.Errorf("请换一个虚 IP：%s 已被%s使用，建集群后会和它们冲突", vip, strings.Join(users, "、"))
		}
	}
	self, err := s.ResolveSelfAddr(vip)
	if err != nil {
		return fmt.Errorf("无法确定本机地址：%v", err)
	}
	token, err := s.NewToken()
	if err != nil {
		return fmt.Errorf("生成集群令牌失败：%v", err)
	}
	pool := ""
	if s.LocalPool != nil {
		pool = s.LocalPool()
	}
	// 角色为 active、keepalived 立即启动：本节点是唯一候选，不需要先出现在谁的 peer 列表里。
	params := JoinParams{VIP: vip, Token: token, NodeAddr: self, Pool: pool, Role: "active"}
	if err := params.validate(); err != nil {
		return err
	}
	return s.Reconfigure(ctx, params)
}

// ServeHTTP 处理 POST /api/cluster/create。
func (s CreateService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJoinError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	err := s.Create(r.Context(), req)
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "creating",
			"role":   "active",
			"vip":    strings.TrimSpace(req.VIP),
			"detail": "正在建立集群：本机将重启为主机并接管虚 IP，随后请改用虚 IP 打开管理页面",
		})
		return
	}
	code := http.StatusBadRequest
	if errors.Is(err, ErrAlreadyClustered) {
		code = http.StatusConflict
	}
	writeJoinError(w, code, err.Error())
}
