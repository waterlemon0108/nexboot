package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeRegistry struct {
	rows  map[string]NodeInfo
	peers []NodeInfo
}

func (f *fakeRegistry) Register(_ context.Context, n NodeInfo) error {
	if f.rows == nil {
		f.rows = map[string]NodeInfo{}
	}
	f.rows[n.NodeID] = n
	return nil
}
func (f *fakeRegistry) Peers(context.Context) ([]NodeInfo, error) { return f.peers, nil }

// 加入与心跳是同一个调用：节点上报身份和地址，主机 upsert 该行。幂等，所以每台节点定时上报即可，新节点无需单独的加入步骤。
func TestClusterRegisterAndPeers(t *testing.T) {
	reg := &fakeRegistry{peers: []NodeInfo{{NodeID: "n1", APIURL: "http://a:80", PortalIP: "10.0.0.1", Role: "active"}}}
	srv := httptest.NewServer(ClusterHandler{Registry: reg, Token: "s3cret"})
	defer srv.Close()

	body, _ := json.Marshal(NodeInfo{NodeID: "n2", APIURL: "http://b:80", PortalIP: "10.0.0.2", Role: "standby"})
	req, _ := http.NewRequest("POST", srv.URL+"/internal/cluster/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || reg.rows["n2"].PortalIP != "10.0.0.2" {
		t.Fatalf("code=%d rows=%#v", resp.StatusCode, reg.rows)
	}

	// 令牌错误被拒绝
	req2, _ := http.NewRequest("POST", srv.URL+"/internal/cluster/register", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer nope")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("code=%d", resp2.StatusCode)
	}

	// peers 列表走同一通道
	peer := Peer{BaseURL: srv.URL, Token: "s3cret"}
	peers, err := peer.ClusterPeers(context.Background())
	if err != nil || len(peers) != 1 || peers[0].NodeID != "n1" {
		t.Fatalf("peers=%#v err=%v", peers, err)
	}
}

// 从自己界面加入的节点需要集群共享密钥：JWT 签名密钥和 iSCSI CHAP 密钥，经集群令牌通道从主机拉取。
// 端点只下发这两样，绝不含管理员密码或目录。
func TestClusterJoinConfig(t *testing.T) {
	// 名册的真实地址一并下发，让加入者用全部成员（而不只是 VIP）生成 VRRP peer 列表并排名优先级。
	// 主机的 PortalIP 是 VIP，APIURL 才是真实地址。
	reg := &fakeRegistry{peers: []NodeInfo{
		{NodeID: "n1", APIURL: "http://192.168.10.3:8080", PortalIP: "192.168.10.250", Role: "active"},
		{NodeID: "n2", APIURL: "http://192.168.10.4:8080", PortalIP: "192.168.10.4", Role: "standby"},
	}}
	srv := httptest.NewServer(ClusterHandler{Registry: reg, Token: "s3cret", JWTSecret: "jjj", CHAPSecret: "ccc",
		PoolName: func() string { return "ndpool" }})
	defer srv.Close()

	// 令牌错误被拒绝，与处理器其余部分同一道闸
	badReq, _ := http.NewRequest("GET", srv.URL+"/internal/cluster/join-config", nil)
	badReq.Header.Set("Authorization", "Bearer nope")
	badResp, _ := http.DefaultClient.Do(badReq)
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token code=%d", badResp.StatusCode)
	}

	// 带上令牌后，加入节点拉到共享密钥
	peer := Peer{BaseURL: srv.URL, Token: "s3cret"}
	jc, err := peer.JoinConfig(context.Background())
	if err != nil {
		t.Fatalf("JoinConfig err=%v", err)
	}
	if jc.JWTSecret != "jjj" || jc.CHAPSecret != "ccc" || jc.Pool != "ndpool" {
		t.Fatalf("join config=%#v", jc)
	}
	if len(jc.Peers) != 2 || jc.Peers[0] != "192.168.10.3" || jc.Peers[1] != "192.168.10.4" {
		t.Fatalf("join config peers=%v, want the roster's real addresses", jc.Peers)
	}
}
