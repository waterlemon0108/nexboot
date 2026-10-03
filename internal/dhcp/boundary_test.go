package dhcp

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
)

// 渲染层有独立于控制层的区间算术（addIPv4/lessIPv4），端点判断必须与控制层一致；
// dnsmasq.conf 错一行整份配置都加载不了。

// 区间首尾地址都合法，不能判为越界。
func TestRenderAcceptsTerminalsOnBothEndsOfTheRange(t *testing.T) {
	cfg := testGroupConfig() // 192.168.1.10 起 10 个：.10–.19
	cfg.Terminals = []domain.Terminal{
		{MAC: "AABBCCDDEE01", IP: "192.168.1.10"},
		{MAC: "AABBCCDDEE02", IP: "192.168.1.19"},
	}
	got, err := testRenderer().Render([]GroupConfig{cfg})
	if err != nil {
		t.Fatalf("端点上的终端被当成越界：%v", err)
	}
	for _, want := range []string{
		"dhcp-host=AA:BB:CC:DD:EE:01,set:grp-1,192.168.1.10",
		"dhcp-host=AA:BB:CC:DD:EE:02,set:grp-1,192.168.1.19",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %q：\n%s", want, got)
		}
	}
}

// 端点外一位必须让整份渲染失败；少一台机器的配置比直接拒绝更难查。
func TestRenderRejectsTerminalsJustOutsideTheRange(t *testing.T) {
	for _, ip := range []string{"192.168.1.9", "192.168.1.20"} {
		t.Run(ip, func(t *testing.T) {
			cfg := testGroupConfig()
			cfg.Terminals = []domain.Terminal{{MAC: "AABBCCDDEE01", IP: ip}}
			if _, err := testRenderer().Render([]GroupConfig{cfg}); err == nil {
				t.Fatalf("区间外的 %s 被渲染进配置了", ip)
			}
		})
	}
}

// 每个分组一条 dhcp-range 和一个 tag，网关/掩码/DNS 挂在各自 tag 上，tag 串了会拿到别组的网关。
func TestRenderGivesEachGroupItsOwnTagAndRange(t *testing.T) {
	one := testGroupConfig()
	two := GroupConfig{
		Group: domain.Group{
			ID: "grp-2", Name: "二班", StartIP: "192.168.2.10", ClientMax: 5,
			Gateway: "192.168.2.254", Netmask: "255.255.255.0", DNS1: "223.5.5.5",
		},
		Terminals: []domain.Terminal{{MAC: "AABBCCDDEE99", IP: "192.168.2.14"}},
	}
	got, err := testRenderer().Render([]GroupConfig{two, one}) // 顺序颠倒，渲染自己排
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dhcp-range=set:grp-1,192.168.1.10,192.168.1.19,255.255.255.0,12h",
		"dhcp-option=tag:grp-1,option:router,192.168.1.1",
		"dhcp-range=set:grp-2,192.168.2.10,192.168.2.14,255.255.255.0,12h",
		"dhcp-option=tag:grp-2,option:router,192.168.2.254",
		"dhcp-option=tag:grp-2,option:dns-server,223.5.5.5",
		"dhcp-host=AA:BB:CC:DD:EE:99,set:grp-2,192.168.2.14",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %q：\n%s", want, got)
		}
	}
	// 二班的机器不能挂到一班的 tag 上。
	if strings.Contains(got, "dhcp-host=AA:BB:CC:DD:EE:99,set:grp-1") {
		t.Fatalf("终端挂错了分组 tag：\n%s", got)
	}
}

// 空分组也要输出自己的区间：先建分组再登记机器是正常顺序。
func TestRenderDeclaresTheRangeOfAGroupWithNoTerminals(t *testing.T) {
	cfg := testGroupConfig()
	cfg.Terminals = nil
	got, err := testRenderer().Render([]GroupConfig{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "dhcp-range=set:grp-1,192.168.1.10,192.168.1.19,255.255.255.0,12h") {
		t.Fatalf("空分组没渲染出区间：\n%s", got)
	}
	if strings.Contains(got, "dhcp-host=") {
		t.Fatalf("空分组渲染出了保留项：\n%s", got)
	}
}

// 保留项按 MAC 排序，相同数据渲染出相同文件，否则每次同步都会触发 dnsmasq 重启。
func TestRenderOrdersReservationsDeterministically(t *testing.T) {
	cfg := testGroupConfig()
	cfg.Terminals = []domain.Terminal{
		{MAC: "AABBCCDDEE03", IP: "192.168.1.12"},
		{MAC: "AABBCCDDEE01", IP: "192.168.1.10"},
		{MAC: "AABBCCDDEE02", IP: "192.168.1.11"},
	}
	got, err := testRenderer().Render([]GroupConfig{cfg})
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(got, "AA:BB:CC:DD:EE:01")
	second := strings.Index(got, "AA:BB:CC:DD:EE:02")
	third := strings.Index(got, "AA:BB:CC:DD:EE:03")
	if !(first < second && second < third) {
		t.Fatalf("保留项没有按 MAC 排序：\n%s", got)
	}
}

// 区间跨 8 位边界（.250 起 10 个到下一段 .3）时 addIPv4 必须正确进位。
func TestRenderCarriesTheRangeAcrossAnOctetBoundary(t *testing.T) {
	cfg := testGroupConfig()
	cfg.Group.StartIP, cfg.Group.ClientMax, cfg.Group.Netmask = "192.168.1.250", 10, "255.255.254.0"
	cfg.Terminals = []domain.Terminal{{MAC: "AABBCCDDEE01", IP: "192.168.2.1"}}
	got, err := testRenderer().Render([]GroupConfig{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "dhcp-range=set:grp-1,192.168.1.250,192.168.2.3,") {
		t.Fatalf("区间末址没有正确进位：\n%s", got)
	}
	if !strings.Contains(got, "dhcp-host=AA:BB:CC:DD:EE:01,set:grp-1,192.168.2.1") {
		t.Fatalf("进位后的地址被当成越界：\n%s", got)
	}
}

// 地址换主人时要用 dhcp_release 释放旧租约（否则 dnsmasq 拒发保留地址），且必须指定正确网卡。
func TestReleaseLeaseSendsTheReleaseOnTheInterfaceServingThatAddress(t *testing.T) {
	runner := &fakeRunner{}
	m := Manager{
		Runner: runner,
		InterfaceFor: func(addr netip.Addr) (string, bool) {
			if addr == netip.MustParseAddr("192.168.10.15") {
				return "ndbr0", true
			}
			return "", false
		},
	}
	if err := m.ReleaseLease(context.Background(), "192.168.10.15", "aabbccddeeff"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	got := runner.calls[0]
	want := []string{"ndbr0", "192.168.10.15", "AA:BB:CC:DD:EE:FF"}
	if got.name != "dhcp_release" || !reflect.DeepEqual(got.args, want) {
		t.Fatalf("想要 dhcp_release %v，得到 %s %v", want, got.name, got.args)
	}
}

// 没有网卡服务该网段时必须报错：dhcp_release 发给空气也返回 0，不能当成功。
func TestReleaseLeaseRefusesWhenNoInterfaceServesTheAddress(t *testing.T) {
	runner := &fakeRunner{}
	m := Manager{Runner: runner, InterfaceFor: func(netip.Addr) (string, bool) { return "", false }}
	if err := m.ReleaseLease(context.Background(), "10.9.9.9", "aabbccddeeff"); err == nil {
		t.Fatal("没有网卡服务这个地址，却照样发了")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("不该发出任何命令：%#v", runner.calls)
	}
}

func TestReleaseLeaseRejectsMalformedInput(t *testing.T) {
	m := Manager{Runner: &fakeRunner{}, InterfaceFor: func(netip.Addr) (string, bool) { return "ndbr0", true }}
	for _, tc := range []struct{ name, ip, mac string }{
		{"地址不是 IPv4", "不是地址", "aabbccddeeff"},
		{"MAC 不成形", "192.168.10.15", "zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.ReleaseLease(context.Background(), tc.ip, tc.mac); err == nil {
				t.Fatal("坏参数被接受了")
			}
		})
	}
}
