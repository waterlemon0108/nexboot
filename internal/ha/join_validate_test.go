package ha

import (
	"context"
	"strings"
	"testing"
)

// 手工输错很常见（VIP 带了 /24、多敲一个字符）。这些值会传给以 root 运行的配置脚本，中途出错会留下半截配置，
// 节点重启成没有 keepalived 的备机、界面又拒绝重新加入；所以在动任何东西前拦住，并说清该怎么填。
func TestJoinRefusesAVIPThatIsNotAnAddress(t *testing.T) {
	for _, vip := range []string{"192.168.10.250/24", "192.168.10", "vip.local", "192.168.10.250 "} {
		js, got := joinStub()
		err := js.Join(context.Background(), JoinRequest{VIP: vip, ClusterToken: "t"})
		if strings.TrimSpace(vip) == "192.168.10.250" {
			if err != nil {
				t.Fatalf("前后空格应当被容忍: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "虚 IP") || !strings.Contains(err.Error(), vip) {
			t.Fatalf("虚 IP %q 应被拒绝并点名: %v", vip, err)
		}
		if got.VIP != "" {
			t.Fatalf("被拒绝时不该启动重配: %+v", got)
		}
	}
}

// 集群返回的节点地址只能是地址：它会被填进 keepalived 配置，其他内容不能混进去。
func TestJoinRefusesPeersThatAreNotAddresses(t *testing.T) {
	for _, bad := range []string{"192.168.10.4|e", "192.168.10.4/24", "node-b", "192.168.10.4\n192.168.10.5"} {
		js, got := joinStub()
		js.FetchConfig = func(context.Context, string, string) (JoinConfig, error) {
			return JoinConfig{Pool: "ndpool", Peers: []string{"192.168.10.3", bad}}, nil
		}
		err := js.Join(context.Background(), JoinRequest{VIP: "192.168.10.250", ClusterToken: "t"})
		if err == nil || !strings.Contains(err.Error(), "节点地址") {
			t.Fatalf("节点地址 %q 应被拒绝: %v", bad, err)
		}
		if got.VIP != "" {
			t.Fatalf("被拒绝时不该启动重配: %+v", got)
		}
	}
}

// 池名、令牌和两个密钥会写进配置文件：换行能多写出一行配置，所以拒收。
func TestJoinRefusesValuesThatWouldSpillIntoTheConfigFile(t *testing.T) {
	cases := map[string]func(*JoinService, *JoinRequest){
		"池名":   func(js *JoinService, _ *JoinRequest) { js.LocalPool = func() string { return "nd pool" } },
		"集群令牌": func(_ *JoinService, req *JoinRequest) { req.ClusterToken = "abc/def" },
		"登录签名密钥": func(js *JoinService, _ *JoinRequest) {
			js.FetchConfig = cfgWith(JoinConfig{Pool: "ndpool", JWTSecret: "a\nNDISKLESS_ROLE=active"})
		},
		"CHAP 密钥": func(js *JoinService, _ *JoinRequest) {
			js.FetchConfig = cfgWith(JoinConfig{Pool: "ndpool", CHAPSecret: "a\rb"})
		},
	}
	for what, breakIt := range cases {
		js, got := joinStub()
		req := JoinRequest{VIP: "192.168.10.250", ClusterToken: "t"}
		breakIt(js, &req)
		err := js.Join(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), what) {
			t.Fatalf("%s 含不允许的字符，应被拒绝并点名: %v", what, err)
		}
		if got.VIP != "" {
			t.Fatalf("被拒绝时不该启动重配: %+v", got)
		}
	}
}

func cfgWith(c JoinConfig) func(context.Context, string, string) (JoinConfig, error) {
	return func(context.Context, string, string) (JoinConfig, error) { return c, nil }
}

func TestCreateRefusesAVIPThatIsNotAnAddress(t *testing.T) {
	svc, got := createStub()
	err := svc.Create(context.Background(), CreateRequest{VIP: "192.168.10.250/24"})
	if err == nil || !strings.Contains(err.Error(), "虚 IP") || !strings.Contains(err.Error(), "例如") {
		t.Fatalf("应拒绝并告诉运维怎么填: %v", err)
	}
	if got.VIP != "" {
		t.Fatalf("被拒绝时不该启动重配: %+v", got)
	}
}
