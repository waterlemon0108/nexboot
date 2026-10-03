package main

import "testing"

// 注册要发给此刻的写入者，集群里唯一永远指向它的地址是 VIP。发给固定机器会静默失效：
// 那台停了注册就失败，那台变成备机则注册写进会被复制整体覆盖的库。存储节点会因此在花名册里时隐时现，
// 客户机随之被判离线、重新放置。
func TestRegistrationTargetPrefersTheVIP(t *testing.T) {
	cases := []struct {
		name       string
		portalAddr string // 有 HA 时是 VIP
		nodeAddr   string // 本机自己的地址
		peerURL    string
		port       string
		want       string
	}{
		{"三节点：peer 传成了某台的地址，仍要走 VIP",
			"192.168.10.250", "192.168.10.5", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.250:8080"},
		{"keepalived 对：备机走 VIP 就是走主机",
			"192.168.10.250", "192.168.10.4", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.250:8080"},
		{"单机：portal 就是本机，没有会合点可言，用配置里的 peer",
			"192.168.10.3", "192.168.10.3", "", "8080", ""},
		{"没配 VIP：退回 peer，总比不注册强",
			"", "192.168.10.5", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.3:8080"},
	}
	for _, c := range cases {
		got := writerURL(c.portalAddr, c.nodeAddr, c.peerURL, c.port)
		if got != c.want {
			t.Fatalf("%s：得到 %q，要 %q", c.name, got, c.want)
		}
	}
}

// 备机拒写时提示运维去哪操作，必须指 VIP：静态对端在三节点里会让两台备机互相指着，照提示走永远到不了写入者。
func TestGateHintPointsAtTheVIPNotAStaticPeer(t *testing.T) {
	cases := []struct {
		name, portalAddr, selfAddr, peerURL, port, want string
	}{
		{"有虚 IP：指虚 IP", "192.168.10.250", "192.168.10.4", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.250:8080"},
		{"虚 IP 就是本机地址（没有真虚 IP）：退回对端", "192.168.10.4", "192.168.10.4", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.3:8080"},
		{"没配虚 IP：退回对端", "", "192.168.10.4", "http://192.168.10.3:8080", "8080",
			"http://192.168.10.3:8080"},
	}
	for _, c := range cases {
		got := writerHint(c.portalAddr, c.selfAddr, c.peerURL, c.port)
		if got != c.want {
			t.Errorf("%s: 得到 %q，想要 %q", c.name, got, c.want)
		}
	}
}

// 备机从哪拉目录与注册发给谁是同一个问题：都要问当前写入者。静态对端在三节点里会让备机互相拉，
// 不报错地停在旧目录，计划切换也因备机追不上而永远被拒。
func TestStandbyPullsFromTheWriterNotAStaticPeer(t *testing.T) {
	cases := []struct {
		name, portalAddr, nodeAddr, peerURL, port, token string
		wantURL                                          string
	}{
		{"三节点：peer 指着一台会变成备机的机器，仍要走虚 IP",
			"192.168.10.250", "192.168.10.5", "http://192.168.10.3:8080", "8080", "tok",
			"http://192.168.10.250:8080"},
		{"没配虚 IP：退回对端，总比不复制强",
			"", "192.168.10.4", "http://192.168.10.3:8080", "8080", "tok",
			"http://192.168.10.3:8080"},
		{"单机：没有会合点，也没有对端，不复制",
			"192.168.10.3", "192.168.10.3", "", "8080", "tok", ""},
		{"没有集群令牌：连不上对端，别假装在复制",
			"192.168.10.250", "192.168.10.5", "http://192.168.10.3:8080", "8080", "", ""},
	}
	for _, c := range cases {
		peer, url := replicationPeer(c.portalAddr, c.nodeAddr, c.peerURL, c.port, c.token, "")
		if url != c.wantURL {
			t.Errorf("%s：拉取源得到 %q，要 %q", c.name, url, c.wantURL)
		}
		if peer.BaseURL != c.wantURL {
			t.Errorf("%s：Peer.BaseURL 得到 %q，要 %q", c.name, peer.BaseURL, c.wantURL)
		}
	}
}

// 建集群时要当场判断 VIP 能否挂到本机某块网卡上：这个错误发生在分离的配置进程里，运维看不到，必须在受理请求时挡下。
func TestSubnetHelpersMatchAddressesOnTheSameNetwork(t *testing.T) {
	nets := []string{"192.168.10.3/24", "10.0.0.5/8"}
	cases := []struct {
		vip  string
		want bool
	}{
		{"192.168.10.250", true},
		{"192.168.10.3", true},
		{"192.168.11.250", false},
		{"10.9.9.9", true},
		{"172.16.0.1", false},
		{"不是地址", false},
	}
	for _, c := range cases {
		if got := subnetHas(nets, c.vip); got != c.want {
			t.Fatalf("subnetHas(%q) = %v, want %v", c.vip, got, c.want)
		}
	}
	// 扫描候选按本机地址所在的 /24 取：机房网段就是这一段，再大就成了扫网。
	if got := subnetCIDROf("192.168.10.3"); got != "192.168.10.0/24" {
		t.Fatalf("subnetCIDROf = %q", got)
	}
	if got := subnetCIDROf(""); got != "" {
		t.Fatalf("空地址应返回空，得到 %q", got)
	}
}
