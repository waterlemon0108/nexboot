package ha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// joinStub 构造默认全部通过的 JoinService，每个用例只改它要测的字段。
func joinStub() (*JoinService, *JoinParams) {
	var got JoinParams
	js := &JoinService{
		AlreadyClustered: func() bool { return false },
		HasPool:          func(context.Context) bool { return true },
		LocalPool:        func() string { return "ndpool" },
		APIPort:          "8080",
		FetchConfig: func(_ context.Context, _, _ string) (JoinConfig, error) {
			return JoinConfig{JWTSecret: "jjj", CHAPSecret: "ccc", Pool: "ndpool",
				Peers: []string{"192.168.10.3", "192.168.10.4"}}, nil
		},
		ResolveSelfAddr: func(string) (string, error) { return "192.168.10.9", nil },
		Reconfigure: func(_ context.Context, p JoinParams) error {
			got = p
			return nil
		},
	}
	return js, &got
}

func TestJoinRefusesWithoutVIPOrToken(t *testing.T) {
	js, _ := joinStub()
	if err := js.Join(context.Background(), JoinRequest{VIP: "", ClusterToken: "t"}); err == nil {
		t.Fatal("empty VIP should be refused")
	}
	if err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: ""}); err == nil {
		t.Fatal("empty token should be refused")
	}
}

func TestJoinRefusesWhenAlreadyClustered(t *testing.T) {
	js, _ := joinStub()
	js.AlreadyClustered = func() bool { return true }
	err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "t"})
	if !errors.Is(err, ErrAlreadyClustered) {
		t.Fatalf("want ErrAlreadyClustered, got %v", err)
	}
}

func TestJoinRefusesWithoutPool(t *testing.T) {
	js, _ := joinStub()
	js.HasPool = func(context.Context) bool { return false }
	err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "t"})
	if !errors.Is(err, ErrNoPoolToJoin) {
		t.Fatalf("want ErrNoPoolToJoin, got %v", err)
	}
}

// 各节点池名互不相干：目录里不存带池名的绝对路径，池记录按 (节点, 池名) 区分。
// 所以不拦「池名与集群不符」，否则会拦住合法部署、逼每台机器取同样的名字。
func TestJoinAcceptsANodeWhosePoolIsNamedDifferently(t *testing.T) {
	js, got := joinStub()
	js.LocalPool = func() string { return "mypool" }
	if err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "t"}); err != nil {
		t.Fatalf("池名不同不该挡住加入：%v", err)
	}
	// 也不能把集群池名硬塞给它：它有自己的池，改名只会让它找不到自己的数据。
	if got.Pool != "mypool" {
		t.Fatalf("加入后应保留本机池名，得到 %q", got.Pool)
	}
}

func TestJoinReportsUnreachableCluster(t *testing.T) {
	js, _ := joinStub()
	js.FetchConfig = func(context.Context, string, string) (JoinConfig, error) {
		return JoinConfig{}, errors.New("dial tcp: connection refused")
	}
	err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "t"})
	if !errors.Is(err, ErrClusterUnreachable) {
		t.Fatalf("want ErrClusterUnreachable, got %v", err)
	}
}

func TestJoinHappyPathReconfiguresAsStandby(t *testing.T) {
	js, got := joinStub()
	err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "tok"})
	if err != nil {
		t.Fatalf("join err=%v", err)
	}
	if got.VIP != "10.0.0.1" || got.Token != "tok" || got.NodeAddr != "192.168.10.9" ||
		got.Pool != "ndpool" || got.JWTSecret != "jjj" || got.CHAPSecret != "ccc" {
		t.Fatalf("reconfigure params=%#v", *got)
	}
	// 名册地址成为加入者的 VRRP peer：不含自己，也不含 VIP（VIP 是当前 master 的别名，不是节点）。
	if len(got.Peers) != 2 || got.Peers[0] != "192.168.10.3" || got.Peers[1] != "192.168.10.4" {
		t.Fatalf("reconfigure peers=%v", got.Peers)
	}
}

func TestJoinPeersExcludeSelfAndVIP(t *testing.T) {
	js, got := joinStub()
	js.FetchConfig = func(context.Context, string, string) (JoinConfig, error) {
		return JoinConfig{Pool: "ndpool", Peers: []string{"192.168.10.3", "192.168.10.9", "10.0.0.1", ""}}, nil
	}
	if err := js.Join(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || got.Peers[0] != "192.168.10.3" {
		t.Fatalf("peers=%v, want self and VIP filtered out", got.Peers)
	}
}

// 经真实 HTTP 往返的端到端用例：加入节点的 FetchConfig 打到运行中的 ClusterHandler，
// 必须把主机实际的密钥和池名原样带到重配置。
func TestJoinFetchesRealConfigOverWire(t *testing.T) {
	primary := httptest.NewServer(ClusterHandler{Registry: &fakeRegistry{}, Token: "tok",
		JWTSecret: "JJ", CHAPSecret: "CC", PoolName: func() string { return "ndpool" }})
	defer primary.Close()
	u, _ := url.Parse(primary.URL)

	var got JoinParams
	js := JoinService{
		AlreadyClustered: func() bool { return false },
		HasPool:          func(context.Context) bool { return true },
		LocalPool:        func() string { return "ndpool" },
		APIPort:          u.Port(),
		FetchConfig: func(ctx context.Context, base, token string) (JoinConfig, error) {
			return Peer{BaseURL: base, Token: token}.JoinConfig(ctx)
		},
		ResolveSelfAddr: func(string) (string, error) { return "10.0.0.9", nil },
		Reconfigure:     func(_ context.Context, p JoinParams) error { got = p; return nil },
	}
	if err := js.Join(context.Background(), JoinRequest{VIP: u.Hostname(), ClusterToken: "tok"}); err != nil {
		t.Fatalf("join over wire: %v", err)
	}
	if got.JWTSecret != "JJ" || got.CHAPSecret != "CC" || got.Pool != "ndpool" || got.NodeAddr != "10.0.0.9" {
		t.Fatalf("params carried over wire=%#v", got)
	}

	// 令牌错误必须让拉取失败，而不是带着空密钥静默加入。
	js.FetchConfig = func(ctx context.Context, base, _ string) (JoinConfig, error) {
		return Peer{BaseURL: base, Token: "wrong"}.JoinConfig(ctx)
	}
	if err := js.Join(context.Background(), JoinRequest{VIP: u.Hostname(), ClusterToken: "wrong"}); !errors.Is(err, ErrClusterUnreachable) {
		t.Fatalf("wrong token should surface as unreachable/authless, got %v", err)
	}
}

// HTTP 面把每种拒绝映射到界面区分所需的状态码：需建池（400）、已入盟（409）、集群不可达（502）。
func TestJoinHandlerStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		tune func(*JoinService)
		want int
	}{
		{"ok", func(*JoinService) {}, http.StatusAccepted},
		{"already", func(j *JoinService) { j.AlreadyClustered = func() bool { return true } }, http.StatusConflict},
		{"no-pool", func(j *JoinService) { j.HasPool = func(context.Context) bool { return false } }, http.StatusBadRequest},
		// 池名不同已不是拒绝理由，见 TestJoinAcceptsANodeWhosePoolIsNamedDifferently。
		{"pool-named-differently", func(j *JoinService) { j.LocalPool = func() string { return "other" } }, http.StatusAccepted},
		{"unreachable", func(j *JoinService) {
			j.FetchConfig = func(context.Context, string, string) (JoinConfig, error) {
				return JoinConfig{}, errors.New("refused")
			}
		}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			js, _ := joinStub()
			tc.tune(js)
			req := httptest.NewRequest("POST", "/api/cluster/join",
				strings.NewReader(`{"vip":"10.0.0.1","cluster_token":"t"}`))
			rec := httptest.NewRecorder()
			js.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: code=%d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
