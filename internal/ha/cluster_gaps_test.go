package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var errNotReachable = errors.New("dial tcp: connection refused")

// —— 客户端一侧：节点如何把自己送进集群 ——
//
// RegisterSelf 是新节点加入的唯一入口，也是每台机器的心跳。这条路不通时，节点在名册里显示离线而它自己一切正常，
// 两边看到的世界都自洽，最难排查。

func TestRegisterSelfCarriesTheTokenAndReportsRejection(t *testing.T) {
	var got NodeInfo
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if auth != "Bearer s3cret" {
			http.Error(w, "cluster token required", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := Peer{BaseURL: srv.URL, Token: "s3cret"}
	me := NodeInfo{NodeID: "n9", APIURL: "http://n9:8080", PortalIP: "10.0.0.9", Role: "standby"}
	if err := p.RegisterSelf(context.Background(), me); err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "n9" || got.APIURL != "http://n9:8080" || got.Role != "standby" {
		t.Fatalf("送过去的身份不完整：%+v", got)
	}

	// 令牌错时必须报错，不能当成功，否则节点会一直以为自己已入册
	bad := Peer{BaseURL: srv.URL, Token: "wrong"}
	if err := bad.RegisterSelf(context.Background(), me); err == nil {
		t.Fatal("被拒绝却报成功，节点会一直以为自己在集群里")
	}
}

func TestRegisterSelfSurfacesAnUnreachablePeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 模拟对端不在
	p := Peer{BaseURL: url, Token: "t"}
	if err := p.RegisterSelf(context.Background(), NodeInfo{NodeID: "n1"}); err == nil {
		t.Fatal("连不上要如实报错——调用方据此决定重试")
	}
}

// —— ProbeQuorum ——
//
// ProbeQuorum 统计联系得上几个，并报出见到的 epoch 最高的主机声明。

func TestProbeQuorumCountsReachableAndPicksHighestActiveClaim(t *testing.T) {
	mk := func(role string, epoch int64, id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(PeerStatus{NodeID: id, Role: role, Epoch: epoch})
		}))
	}
	a := mk("active", 5, "a")
	b := mk("active", 9, "b") // epoch 更高，应被选中
	c := mk("standby", 12, "c")
	defer func() { a.Close(); b.Close(); c.Close() }()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	peers := []Peer{
		{BaseURL: a.URL, Token: "t"}, {BaseURL: b.URL, Token: "t"},
		{BaseURL: c.URL, Token: "t"}, {BaseURL: deadURL, Token: "t"},
	}
	reached, claim := ProbeQuorum(context.Background(), peers, 2*time.Second)
	if reached != 3 {
		t.Fatalf("联系上的台数 = %d，应为 3（第四台已关）", reached)
	}
	if claim == nil || claim.NodeID != "b" || claim.Epoch != 9 {
		t.Fatalf("要报出 epoch 最高的那个主机声明，实际 %+v", claim)
	}

	// 无人自称主机时不能编一个出来，上层据此判断可以安全接任
	onlyStandby := []Peer{{BaseURL: c.URL, Token: "t"}}
	reached, claim = ProbeQuorum(context.Background(), onlyStandby, 2*time.Second)
	if reached != 1 || claim != nil {
		t.Fatalf("没有主机声明时必须是 nil：reached=%d claim=%+v", reached, claim)
	}
}

// 各次探测分别计时：一台卡住的机器不能把整轮探测拖过 keepalived 的检查窗口，否则健康节点会被判故障。
func TestProbeQuorumTimesOutPerPeerInsteadOfHanging(t *testing.T) {
	var hits int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PeerStatus{NodeID: "f", Role: "standby", Epoch: 1})
	}))
	defer fast.Close()

	start := time.Now()
	reached, _ := ProbeQuorum(context.Background(),
		[]Peer{{BaseURL: slow.URL, Token: "t"}, {BaseURL: fast.URL, Token: "t"}},
		300*time.Millisecond)
	elapsed := time.Since(start)
	if reached != 1 {
		t.Fatalf("卡住的那台不该算进联系上的台数：reached=%d", reached)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("整轮探测耗时 %v——一台卡住就拖垮全轮，健康节点会被判故障", elapsed)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("慢节点根本没被探到，用例没测到想测的东西")
	}
}

// —— 两个 HTTP 处理器的守卫分支 ——
//
// 它们是 HA 控制面的网络入口：一个能改名册，一个能让节点当场易主。鉴权、方法、参数校验出洞，
// 任何能碰到端口的人都能操纵集群角色。

func TestClusterHandlerGuards(t *testing.T) {
	reg := &fakeRegistry{}
	h := ClusterHandler{Registry: reg, Token: "s3cret"}

	do := func(method, path, body, token string) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if got := do("POST", "/internal/cluster/register", `{"node_id":"n1"}`, ""); got != 401 {
		t.Fatalf("无令牌应 401，得到 %d", got)
	}
	if got := do("POST", "/internal/cluster/register", `{"node_id":"n1"}`, "wrong"); got != 401 {
		t.Fatalf("错令牌应 401，得到 %d", got)
	}
	// 令牌为空时不能变成谁都放行：空配置在部署里最容易出现
	empty := ClusterHandler{Registry: reg, Token: ""}
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/cluster/peers", nil))
	if rec.Code != 401 {
		t.Fatalf("没配令牌的节点必须一律拒绝，得到 %d", rec.Code)
	}

	if got := do("POST", "/internal/cluster/register", `not json`, "s3cret"); got != 400 {
		t.Fatalf("坏请求体应 400，得到 %d", got)
	}
	// 没有 node_id 的登记要挡住：无名的行进了名册就再也对不上人
	if got := do("POST", "/internal/cluster/register", `{"api_url":"http://x"}`, "s3cret"); got != 400 {
		t.Fatalf("缺 node_id 应 400，得到 %d", got)
	}
	if got := do("GET", "/internal/cluster/nope", "", "s3cret"); got != 404 {
		t.Fatalf("未知路径应 404，得到 %d", got)
	}
}

func TestControlHandlerGuardsAndForceFlag(t *testing.T) {
	c, _ := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 1})
	// 会被拦下的场景：有对端正在以更高 epoch 服务。
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errNotReachable},
			// 在服务的节点必然持有 VIP：这是 servingNow 里不依赖 epoch 的判据。
			fakePeerStatus{status: PeerStatus{NodeID: "p", Role: "active", Epoch: 9, HoldsVIP: true}},
		}
	}
	h := ControlHandler{Controller: c, NodeID: "self", Token: "s3cret"}

	call := func(method, path, token string) (int, string) {
		r := httptest.NewRequest(method, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}

	if code, _ := call("GET", "/internal/ha/status", ""); code != 401 {
		t.Fatalf("无令牌读状态应 401，得到 %d", code)
	}
	if code, _ := call("POST", "/internal/ha/activate", "wrong"); code != 401 {
		t.Fatalf("错令牌激活应 401，得到 %d", code)
	}
	// 被激活守卫拦下要报 409 而不是 500：前者是按规则拒绝，后者是出故障，keepalived 通知脚本据此决定是否重试。
	if code, body := call("POST", "/internal/ha/activate", "s3cret"); code != 409 {
		t.Fatalf("对端仍在服务时激活应 409，得到 %d %s", code, body)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("被拒之后角色不能变：%+v", rs)
	}
	// ?force=1 已删除：带上它也不能有任何特权
	if code, body := call("POST", "/internal/ha/activate?force=1", "s3cret"); code != 409 {
		t.Fatalf("对端仍在服务时，带 force 也应 409，得到 %d %s", code, body)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "standby" {
		t.Fatalf("强制被拒之后角色也不能变：%+v", rs)
	}

	// 换成谁都联系不上：这是最后幸存节点的处境，普通激活应放行
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{err: errNotReachable}, fakePeerStatus{err: errNotReachable},
		}
	}
	if code, body := call("POST", "/internal/ha/activate", "s3cret"); code != 200 {
		t.Fatalf("联系不上任何对端时激活应 200，得到 %d %s", code, body)
	}
	if rs, _ := ReadRoleState(c.RoleFile); rs.Role != "active" {
		t.Fatalf("强制激活后应为主机：%+v", rs)
	}
	if code, _ := call("GET", "/internal/ha/nope", "s3cret"); code != 404 {
		t.Fatalf("未知路径应 404，得到 %d", code)
	}
}
