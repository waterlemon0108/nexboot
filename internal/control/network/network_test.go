package network

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func newTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedGroup 写入一个分组及其必须指向的镜像/配置。
func seedGroup(t *testing.T, st store.Store, g domain.Group) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.Images().Get(ctx, "img"); err != nil {
		cfg := domain.Config{ID: "cfg", ImageID: "img", Name: "default", CreatedAt: now}
		red := domain.Reduction{ID: "cfg_0", ConfigID: "cfg", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
		cfg.DefaultReductionID = &red.ID
		for _, err := range []error{
			st.Images().Create(ctx, domain.Image{ID: "img", Name: "img", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}),
			st.Configs().Create(ctx, cfg),
			st.Reductions().Create(ctx, red),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	g.SystemImageID, g.SystemConfigID, g.SystemReductionID = "img", "cfg", "cfg_0"
	if err := st.Groups().Create(ctx, g); err != nil {
		t.Fatal(err)
	}
}

var testIfaces = []Interface{
	{Name: "lo", Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}, Up: true, Loopback: true},
	{Name: "wlo1", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.124.56/24")}, Up: true},
	{Name: "ndbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.50.1/24")}, Up: true},
}

type fakeBase struct {
	synced []dhcp.BaseConfig
	err    error
}

func (b *fakeBase) SyncBase(_ context.Context, cfg dhcp.BaseConfig) error {
	b.synced = append(b.synced, cfg)
	return b.err
}

type fakeRunner struct {
	calls  [][]string
	output string
	err    error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return []byte(r.output), r.err
}

func newService(t *testing.T, st store.Store, baseConf string) (Service, *fakeBase, *fakeRunner) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "00-ndiskless-base.conf")
	if baseConf != "" {
		if err := os.WriteFile(path, []byte(baseConf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := &fakeBase{}
	runner := &fakeRunner{}
	svc := Service{
		Store:        st,
		Interfaces:   func() ([]Interface, error) { return testIfaces, nil },
		RouteGateway: func() (netip.Addr, bool) { return netip.MustParseAddr("192.168.124.1"), true },
		BootHost:     "192.168.50.1",
		BaseConfPath: path,
		Base:         base,
		Runner:       runner,
	}
	return svc, base, runner
}

// 未配置时客户机网卡取 dnsmasq 当前绑定的网卡（安装器的基础配置），其次取网段包含开机 URL 主机的网卡。
func TestClientInterfaceIsDetected(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newService(t, newTestStore(t), "port=0\ninterface=ndbr0\nbind-interfaces\n")
	net, err := svc.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if net.Iface != "ndbr0" || !net.Known || len(net.Prefixes) != 1 || net.Prefixes[0].String() != "192.168.50.1/24" || net.AllowCrossSubnet {
		t.Fatalf("client = %#v", net)
	}
	if len(net.ServerAddrs) != 2 { // 所有非回环 v4 地址，不只客户机网卡上的
		t.Fatalf("server addrs = %v", net.ServerAddrs)
	}

	svc, _, _ = newService(t, newTestStore(t), "") // 无基础配置：退回开机主机
	net, err = svc.Client(ctx)
	if err != nil || net.Iface != "ndbr0" {
		t.Fatalf("client = %#v err=%v", net, err)
	}
}

// 视图带齐页面所需：网卡、客户机网络、新分组的建议窗口和各分组状态。
func TestGetView(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for _, g := range []domain.Group{
		{ID: "g-same", Name: "一班", StartIP: "192.168.50.50", ClientMax: 30, Netmask: "255.255.255.0"},
		{ID: "g-off", Name: "vmdk", StartIP: "192.168.10.50", ClientMax: 30, Netmask: "255.255.255.0"},
	} {
		seedGroup(t, st, g)
	}
	svc, _, _ := newService(t, st, "interface=ndbr0\n")
	view, err := svc.Get(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if view.ClientIface != "ndbr0" || view.Suggest == nil || view.Suggest.StartIP != "192.168.50.10" || view.Suggest.Gateway != "" {
		t.Fatalf("view = %#v suggest=%#v", view, view.Suggest)
	}
	status := map[string]assets.NetworkStatus{}
	for _, g := range view.Groups {
		status[g.ID] = g.Status
	}
	if status["g-same"] != assets.NetworkSame || status["g-off"] != assets.NetworkBlocked {
		t.Fatalf("group statuses = %v", status)
	}
	if len(view.Interfaces) != 2 { // 不列回环网卡
		t.Fatalf("interfaces = %#v", view.Interfaces)
	}
}

// 打开跨网段后 dnsmasq 基础配置改为监听所有网卡（中继请求从上联口进来）；
// 仍有分组依赖时拒绝关闭，并点名该分组。
func TestUpdateSwitchesListeningAndGuardsTheWayBack(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	svc, base, _ := newService(t, st, "interface=ndbr0\n")

	view, err := svc.Update(ctx, Request{AllowCrossSubnet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !view.AllowCrossSubnet || len(base.synced) != 1 || !base.synced[0].ListenAll || base.synced[0].Iface != "ndbr0" {
		t.Fatalf("view=%#v synced=%#v", view, base.synced)
	}
	saved, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil || !saved.AllowCrossSubnet {
		t.Fatalf("saved = %#v err=%v", saved, err)
	}

	seedGroup(t, st, domain.Group{ID: "g-off", Name: "vmdk", StartIP: "192.168.10.50", ClientMax: 30, Netmask: "255.255.255.0"})
	_, err = svc.Update(ctx, Request{AllowCrossSubnet: false})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "vmdk") {
		t.Fatalf("err = %v", err)
	}
	if len(base.synced) != 1 {
		t.Fatalf("a refused update must not touch dnsmasq: %#v", base.synced)
	}

	// 显式选择的网卡被保存并采用。
	view, err = svc.Update(ctx, Request{ClientIface: "wlo1", AllowCrossSubnet: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.ClientIface != "wlo1" || base.synced[len(base.synced)-1].Iface != "wlo1" {
		t.Fatalf("view=%#v synced=%#v", view, base.synced)
	}
	if _, err := svc.Update(ctx, Request{ClientIface: "eth9", AllowCrossSubnet: true}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("unknown interface must be refused, got %v", err)
	}
}

// 探测是从服务器 ping 一次，结果为通/不通加往返时间；ping 本身跑不起来是错误而非不可达。
func TestProbePings(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newService(t, newTestStore(t), "")
	runner.output = "64 bytes from 192.168.10.1: icmp_seq=1 ttl=64 time=0.412 ms\n"
	res, err := svc.Probe(ctx, ProbeRequest{IP: "192.168.10.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reachable || res.RTTMillis != 0.412 || len(runner.calls) != 1 || runner.calls[0][0] != "ping" || runner.calls[0][len(runner.calls[0])-1] != "192.168.10.1" {
		t.Fatalf("res=%#v calls=%#v", res, runner.calls)
	}
	runner.err = errors.New("exit status 1")
	runner.output = ""
	res, err = svc.Probe(ctx, ProbeRequest{IP: "192.168.10.1"})
	if err != nil || res.Reachable {
		t.Fatalf("res=%#v err=%v", res, err)
	}
	if _, err := svc.Probe(ctx, ProbeRequest{IP: "not-an-ip"}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("bad ip err = %v", err)
	}
}

// 保存的网卡名只是单个节点的事实（备机网卡可能另有名字）：本机没有该网卡时按网段匹配，
// 不能写进 dnsmasq 成为永远起不来的绑定。
func TestClientIfaceNameFallsBackWhenStoredNameIsAbsent(t *testing.T) {
	svc := Service{BootHost: "192.168.50.1"}
	ifaces := []Interface{
		{Name: "eno1", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.50.1/24")}},
	}
	got := svc.clientIfaceName(domain.SystemSettings{ClientIface: "enp2s0"}, ifaces)
	if got != "eno1" {
		t.Fatalf("iface = %q, want fallback to the subnet match", got)
	}
	// 本机确有的已保存网卡名仍然优先。
	got = svc.clientIfaceName(domain.SystemSettings{ClientIface: "eno1"}, ifaces)
	if got != "eno1" {
		t.Fatalf("iface = %q", got)
	}
}

// keepalived 以 /32 把 VIP 加到客户机网卡上，它不承载客户机，不能列成第二个网段。
func TestViewListsSubnetsNotAddresses(t *testing.T) {
	st := newTestStore(t)
	svc, _, _ := newService(t, st, "interface=ens33\n")
	ifaces := []Interface{{Name: "ens33", Up: true, Addrs: []netip.Prefix{
		netip.MustParsePrefix("192.168.10.3/24"), netip.MustParsePrefix("192.168.10.250/32"),
	}}}
	view, err := svc.view(context.Background(), domain.SystemSettings{ClientIface: "ens33"}, ifaces, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ClientNetworks) != 1 || view.ClientNetworks[0] != "192.168.10.0/24" {
		t.Fatalf("client networks = %v", view.ClientNetworks)
	}
}

// 建议起始 IP 和告警都要避开别的节点和 VIP，不只是本机网卡上的地址。
func TestClientNetworkKnowsTheWholeClusterAddresses(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, srv := range []domain.Server{
		{ID: "a", Name: "a", IP: "192.168.50.1", PortalIP: "192.168.50.250", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
		{ID: "b", Name: "b", IP: "192.168.50.4", PortalIP: "192.168.50.4", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp},
	} {
		if err := st.Servers().Create(ctx, srv); err != nil {
			t.Fatal(err)
		}
	}
	svc, _, _ := newService(t, st, "interface=ndbr0\n")
	client, err := svc.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"192.168.50.4", "192.168.50.250"} {
		if !client.IsServerAddr(netip.MustParseAddr(want)) {
			t.Errorf("ServerAddrs %v 缺 %s", client.ServerAddrs, want)
		}
	}
}

// 未开跨网段时换客户机网卡（或改回自动识别），原网段里的分组会落到新网段外、客户机拿不到地址，必须拒绝并点名分组。
func TestUpdateRefusesAnIfaceChangeThatStrandsGroups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedGroup(t, st, domain.Group{ID: "g-50", Name: "一班", StartIP: "192.168.50.50", ClientMax: 30, Netmask: "255.255.255.0"})
	svc, base, _ := newService(t, st, "interface=ndbr0\n")

	_, err := svc.Update(ctx, Request{ClientIface: "wlo1"})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "一班") {
		t.Fatalf("换到 wlo1 会让分组落到网段外，应拒绝并点名：%v", err)
	}
	if len(base.synced) != 0 {
		t.Fatalf("被拒的修改不能动 dnsmasq：%#v", base.synced)
	}
	if saved, err := st.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID); err == nil && saved.ClientIface == "wlo1" {
		t.Fatalf("被拒的网卡不能保存：%#v", saved)
	}

	// 先选定 ndbr0，再改回自动识别（落到别的网卡）同样要检查。
	if _, err := svc.Update(ctx, Request{ClientIface: "ndbr0"}); err != nil {
		t.Fatal(err)
	}
	svc.BaseConfPath = filepath.Join(t.TempDir(), "absent.conf")
	svc.BootHost = "192.168.124.56"
	if _, err := svc.Update(ctx, Request{}); !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "一班") {
		t.Fatalf("改回自动识别落到 wlo1 也应拒绝：%v", err)
	}

	// 开着跨网段时网段外的分组走中继，可以换。
	if _, err := svc.Update(ctx, Request{ClientIface: "wlo1", AllowCrossSubnet: true}); err != nil {
		t.Fatalf("开着跨网段时应允许换网卡：%v", err)
	}
}
