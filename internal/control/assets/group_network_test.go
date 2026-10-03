package assets

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
)

func testClientNetwork(allowCross bool) ClientNetwork {
	return ClientNetwork{
		Iface:            "ndbr0",
		Prefixes:         []netip.Prefix{netip.MustParsePrefix("192.168.50.1/24")},
		ServerAddrs:      []netip.Addr{netip.MustParseAddr("192.168.50.1"), netip.MustParseAddr("192.168.124.56")},
		AllowCrossSubnet: allowCross,
		Known:            true,
	}
}

func groupWith(start string, max int, gateway string) domain.Group {
	return domain.Group{ID: "g", Name: "一班", StartIP: start, ClientMax: max, Gateway: gateway, Netmask: "255.255.255.0"}
}

// 区间在客户机网段内为 same；不在且允许跨网段为 relay，不允许为 blocked；读不到服务器网络为 unknown，不据此下结论。
func TestClassifyGroupNetwork(t *testing.T) {
	cases := []struct {
		name  string
		group domain.Group
		net   ClientNetwork
		want  NetworkStatus
	}{
		{"inside the client network", groupWith("192.168.50.50", 30, "192.168.50.254"), testClientNetwork(false), NetworkSame},
		{"outside, relay not allowed", groupWith("192.168.10.50", 30, "192.168.10.1"), testClientNetwork(false), NetworkBlocked},
		{"outside, relay allowed", groupWith("192.168.10.50", 30, "192.168.10.1"), testClientNetwork(true), NetworkRelay},
		{"window straddles the network edge", groupWith("192.168.50.240", 30, "192.168.50.254"), testClientNetwork(false), NetworkBlocked},
		{"network unreadable", groupWith("192.168.10.50", 30, "192.168.10.1"), ClientNetwork{}, NetworkUnknown},
	}
	for _, c := range cases {
		if got := ClassifyGroupNetwork(c.group, c.net); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// 建议区间用于新分组表单和「自动填写」：在客户机网段内、从 .10 起，避开服务器地址、其他分组区间和网段保留地址；
// .10 起放不下时从 .2 起。
func TestSuggestGroupRange(t *testing.T) {
	net := testClientNetwork(false)
	got, ok := SuggestGroupRange(net, nil, 30, "")
	if !ok || got.StartIP != "192.168.50.10" || got.Netmask != "255.255.255.0" {
		t.Fatalf("empty network: %#v ok=%v", got, ok)
	}
	// .10–.39 已被别组占用：下一个空闲区间紧随其后。
	others := []domain.Group{{ID: "other", StartIP: "192.168.50.10", ClientMax: 30}}
	got, ok = SuggestGroupRange(net, others, 30, "")
	if !ok || got.StartIP != "192.168.50.40" {
		t.Fatalf("after another group: %#v ok=%v", got, ok)
	}
	// 正在编辑的分组不挡自己。
	got, ok = SuggestGroupRange(net, others, 30, "other")
	if !ok || got.StartIP != "192.168.50.10" {
		t.Fatalf("editing the same group: %#v ok=%v", got, ok)
	}
	// 服务器 .1 和网段 .0/.255 不进区间；从 .10 起放不下时尝试从 .2 起。
	big := []domain.Group{{ID: "a", StartIP: "192.168.50.10", ClientMax: 240}} // .10–.249
	got, ok = SuggestGroupRange(net, big, 8, "")
	if !ok || got.StartIP != "192.168.50.2" {
		t.Fatalf("fallback below .10: %#v ok=%v", got, ok)
	}
	if _, ok = SuggestGroupRange(net, big, 30, ""); ok {
		t.Fatal("30 addresses cannot fit; must report so")
	}
	// 服务器默认网关只有在客户机网段内时才作为建议网关。
	net.RouteGateway = netip.MustParseAddr("192.168.50.254")
	got, _ = SuggestGroupRange(net, nil, 30, "")
	if got.Gateway != "192.168.50.254" {
		t.Fatalf("gateway = %q, want the server's route gateway", got.Gateway)
	}
	net.RouteGateway = netip.MustParseAddr("192.168.124.1")
	got, _ = SuggestGroupRange(net, nil, 30, "")
	if got.Gateway != "" {
		t.Fatalf("gateway = %q, want none (route gateway is on another network)", got.Gateway)
	}
}

// 在客户机网段外新建分组时提前拒绝，点名网卡并给出可用区间；允许跨网段时同一请求通过。
// 网关可为空（教室无路由器），也可为服务器本身（服务器给教室做路由）。
func TestGroupServiceEnforcesClientNetwork(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)
	service.DHCP = &fakeDHCPSyncer{}
	allow := false
	service.Network = func(context.Context) (ClientNetwork, error) { return testClientNetwork(allow), nil }

	req := validGroupRequest("十段班") // 192.168.1.10 + 20，网关 192.168.1.1
	_, err := service.Create(ctx, req)
	var outside GroupOutsideClientNetwork
	if !errors.As(err, &outside) || !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("err = %v, want GroupOutsideClientNetwork", err)
	}
	for _, want := range []string{"ndbr0", "192.168.50.1/24", "192.168.1.10", "192.168.50.10", "跨网段"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q should mention %q", err.Error(), want)
		}
	}
	if outside.Suggest == nil || outside.Suggest.StartIP != "192.168.50.10" {
		t.Fatalf("suggest = %#v", outside.Suggest)
	}

	allow = true
	created, err := service.Create(ctx, req)
	if err != nil {
		t.Fatalf("with cross-subnet allowed the group must pass: %v", err)
	}
	if created.StartIP != "192.168.1.10" {
		t.Fatalf("created = %#v", created)
	}

	// 不填网关可以（教室无路由器）；网关为服务器自身地址也可以（经第二张网卡给教室做路由）。
	req = validGroupRequest("五十段班")
	req.StartIP, req.Gateway = "192.168.50.50", ""
	if _, err := service.Create(ctx, req); err != nil {
		t.Fatalf("empty gateway must be allowed: %v", err)
	}
	req = validGroupRequest("服务器做网关")
	req.StartIP, req.Gateway = "192.168.50.100", "192.168.50.1"
	if _, err := service.Create(ctx, req); err != nil {
		t.Fatalf("gateway = server must be allowed: %v", err)
	}
}
