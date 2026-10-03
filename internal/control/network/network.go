// Package network 管理面向客户机的网络：dnsmasq 在哪块网卡上服务客户机、客户机因此处在
// 哪些网段、分组能否落在这些网段之外（交换机做 DHCP 中继的场景）。它是这份状态的唯一读取方，
// 分组校验、告警和网络页都以它为准。
package network

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// Interface 是服务器的一块网卡及其 IPv4 网段。
type Interface struct {
	Name     string
	Addrs    []netip.Prefix
	Up       bool
	Loopback bool
}

// BaseSyncer 写 dnsmasq 的监听配置，dhcp.Manager 即其实现。
type BaseSyncer interface {
	SyncBase(context.Context, dhcp.BaseConfig) error
}

// Runner 执行命令，用于可达性探测。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Service struct {
	Store store.Store
	// Interfaces 读取服务器网卡；nil 时读真实网卡。
	Interfaces func() ([]Interface, error)
	// RouteGateway 返回服务器默认网关（如有）；nil 时读路由表。
	RouteGateway func() (netip.Addr, bool)
	// BootHost 是告诉客户机去开机的地址（开机 URL 的主机），用于兜底判断哪块网卡面向客户机。
	BootHost string
	// BaseConfPath 是 dnsmasq 基础配置，读它得知当前绑定的网卡，切换模式时经 Base 写入。
	BaseConfPath string
	Base         BaseSyncer
	Runner       Runner
	Logger       *slog.Logger
}

// Client 返回 GroupService 和告警用来判定的客户机网络。
func (s Service) Client(ctx context.Context) (assets.ClientNetwork, error) {
	settings, err := s.settings(ctx)
	if err != nil {
		return assets.ClientNetwork{}, err
	}
	ifaces, err := s.interfaces()
	if err != nil {
		return assets.ClientNetwork{}, err
	}
	return s.withCluster(ctx, s.clientFrom(settings, ifaces)), nil
}

// withCluster 把其它节点地址和 VIP 加入 ServerAddrs，使建议和告警也避开它们。
func (s Service) withCluster(ctx context.Context, client assets.ClientNetwork) assets.ClientNetwork {
	if s.Store == nil {
		return client
	}
	addrs, err := assets.ClusterAddresses(ctx, s.Store)
	if err != nil {
		return client
	}
	for _, a := range addrs {
		if !client.IsServerAddr(a.Addr) {
			client.ServerAddrs = append(client.ServerAddrs, a.Addr)
		}
	}
	return client
}

func (s Service) clientFrom(settings domain.SystemSettings, ifaces []Interface) assets.ClientNetwork {
	out := assets.ClientNetwork{AllowCrossSubnet: settings.AllowCrossSubnet}
	for _, iface := range ifaces {
		if iface.Loopback {
			continue
		}
		for _, p := range iface.Addrs {
			out.ServerAddrs = append(out.ServerAddrs, p.Addr())
		}
	}
	name := s.clientIfaceName(settings, ifaces)
	for _, iface := range ifaces {
		if iface.Name != name {
			continue
		}
		out.Iface = iface.Name
		out.Prefixes = append(out.Prefixes, iface.Addrs...)
	}
	out.Known = out.Iface != "" && len(out.Prefixes) > 0
	if s.RouteGateway != nil {
		if gw, ok := s.RouteGateway(); ok {
			out.RouteGateway = gw
		}
	} else if gw, ok := readDefaultGateway(); ok {
		out.RouteGateway = gw
	}
	return out
}

// clientIfaceName 依次取：配置的网卡、dnsmasq 当前绑定的网卡（安装器写的基础配置）、
// 网段包含开机主机的网卡、第一块有地址的非回环网卡。
func (s Service) clientIfaceName(settings domain.SystemSettings, ifaces []Interface) string {
	if name := strings.TrimSpace(settings.ClientIface); name != "" {
		// 保存的网卡名是单个节点的事实：本机确有该网卡才采用，否则按网段匹配，
		// 避免把 dnsmasq 绑到永远起不来的网卡名上。
		for _, iface := range ifaces {
			if iface.Name == name {
				return name
			}
		}
	}
	if name := s.boundInterface(); name != "" {
		return name
	}
	if host, err := netip.ParseAddr(strings.TrimSpace(s.BootHost)); err == nil {
		for _, iface := range ifaces {
			for _, p := range iface.Addrs {
				if p.Contains(host) {
					return iface.Name
				}
			}
		}
	}
	for _, iface := range ifaces {
		if !iface.Loopback && len(iface.Addrs) > 0 {
			return iface.Name
		}
	}
	return ""
}

// boundInterface 从基础配置读取 `interface=`。
func (s Service) boundInterface() string {
	if s.BaseConfPath == "" {
		return ""
	}
	f, err := os.Open(s.BaseConfPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if name, ok := strings.CutPrefix(line, "interface="); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// InterfaceView 是网络页列出的一块网卡。
type InterfaceView struct {
	Name   string   `json:"name"`
	Addrs  []string `json:"addrs"`
	Up     bool     `json:"up"`
	Client bool     `json:"client"`
}

// GroupNetworkView 是一个分组相对客户机网络的状态。
type GroupNetworkView struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	StartIP   string               `json:"start_ip"`
	ClientMax int                  `json:"client_max"`
	Gateway   string               `json:"gateway"`
	Status    assets.NetworkStatus `json:"status"`
}

type View struct {
	Interfaces         []InterfaceView        `json:"interfaces"`
	ClientIface        string                 `json:"client_iface"`
	ClientIfaceSetting string                 `json:"client_iface_setting"` // "" 表示自动检测
	ClientNetworks     []string               `json:"client_networks"`
	ServerAddrs        []string               `json:"server_addrs"`
	RouteGateway       string                 `json:"route_gateway,omitempty"`
	BootHost           string                 `json:"boot_host"`
	AllowCrossSubnet   bool                   `json:"allow_cross_subnet"`
	Known              bool                   `json:"known"`
	Suggest            *assets.SuggestedRange `json:"suggest,omitempty"`
	Groups             []GroupNetworkView     `json:"groups"`
}

// Get 返回网络页数据：网卡、客户机网络、clientMax 台机器的建议地址窗口，以及各分组的状态。
func (s Service) Get(ctx context.Context, clientMax int) (View, error) {
	settings, err := s.settings(ctx)
	if err != nil {
		return View{}, err
	}
	ifaces, err := s.interfaces()
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, settings, ifaces, clientMax)
}

// subnets 列出客户机可处的网段：单地址（如集群 VIP）不算，除非只剩它们。
func subnets(prefixes []netip.Prefix) []string {
	keep := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		if !p.IsSingleIP() {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		keep = prefixes
	}
	out := []string{}
	seen := map[netip.Prefix]bool{}
	for _, p := range keep {
		if m := p.Masked(); !seen[m] {
			seen[m] = true
			out = append(out, m.String())
		}
	}
	return out
}

func (s Service) view(ctx context.Context, settings domain.SystemSettings, ifaces []Interface, clientMax int) (View, error) {
	client := s.withCluster(ctx, s.clientFrom(settings, ifaces))
	view := View{
		ClientIface:        client.Iface,
		ClientIfaceSetting: settings.ClientIface,
		BootHost:           s.BootHost,
		AllowCrossSubnet:   client.AllowCrossSubnet,
		Known:              client.Known,
		ClientNetworks:     []string{},
		ServerAddrs:        []string{},
		Groups:             []GroupNetworkView{},
	}
	view.ClientNetworks = subnets(client.Prefixes)
	for _, a := range client.ServerAddrs {
		view.ServerAddrs = append(view.ServerAddrs, a.String())
	}
	if client.RouteGateway.IsValid() {
		view.RouteGateway = client.RouteGateway.String()
	}
	for _, iface := range ifaces {
		// 只列可服务客户机的网卡：回环和无地址网卡（docker veth、未插线的口）都是噪音。
		if iface.Loopback || (len(iface.Addrs) == 0 && iface.Name != client.Iface) {
			continue
		}
		iv := InterfaceView{Name: iface.Name, Up: iface.Up, Client: iface.Name == client.Iface, Addrs: []string{}}
		for _, p := range iface.Addrs {
			iv.Addrs = append(iv.Addrs, p.String())
		}
		view.Interfaces = append(view.Interfaces, iv)
	}
	sort.Slice(view.Interfaces, func(i, j int) bool { return view.Interfaces[i].Name < view.Interfaces[j].Name })
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return View{}, err
	}
	for _, g := range groups {
		view.Groups = append(view.Groups, GroupNetworkView{
			ID: g.ID, Name: g.Name, StartIP: g.StartIP, ClientMax: g.ClientMax, Gateway: g.Gateway,
			Status: assets.ClassifyGroupNetwork(g, client),
		})
	}
	if clientMax > 0 {
		if suggest, ok := assets.SuggestGroupRange(client, groups, clientMax, ""); ok {
			view.Suggest = &suggest
		}
	}
	return view, nil
}

// Request 是网络页可修改的两项设置。
type Request struct {
	ClientIface      string `json:"client_iface"`
	AllowCrossSubnet bool   `json:"allow_cross_subnet"`
}

// Update 保存客户机网卡与跨网段开关，并让 dnsmasq 监听正确的网卡。
// 修改会让分组落到客户机网段外时拒绝，并点名该分组。
func (s Service) Update(ctx context.Context, req Request) (View, error) {
	settings, err := s.settings(ctx)
	if err != nil {
		return View{}, err
	}
	ifaces, err := s.interfaces()
	if err != nil {
		return View{}, err
	}
	name := strings.TrimSpace(req.ClientIface)
	if name != "" {
		found := false
		for _, iface := range ifaces {
			if iface.Name == name && !iface.Loopback {
				found = true
			}
		}
		if !found {
			return View{}, errs.Invalid("本机没有叫 " + name + " 的网卡")
		}
	}
	before := s.clientFrom(settings, ifaces)
	settings.ID = domain.SystemSettingsDefaultID
	settings.ClientIface = name
	settings.AllowCrossSubnet = req.AllowCrossSubnet
	// 换网卡和关跨网段都会让分组落到网段外、客户机拿不到地址；原本就在网段外的不挡这次修改。
	after := s.clientFrom(settings, ifaces)
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return View{}, err
	}
	var relayed, stranded []string
	for _, g := range groups {
		if assets.ClassifyGroupNetwork(g, after) != assets.NetworkBlocked {
			continue
		}
		switch assets.ClassifyGroupNetwork(g, before) {
		case assets.NetworkBlocked:
		case assets.NetworkRelay:
			relayed = append(relayed, g.Name)
		default:
			stranded = append(stranded, g.Name)
		}
	}
	if len(relayed) > 0 {
		return View{}, errs.Invalid(fmt.Sprintf("还有 %d 个跨网段分组：%s。先把它们改到服务器网段，再关闭跨网段分组", len(relayed), strings.Join(relayed, "、")))
	}
	if len(stranded) > 0 {
		return View{}, errs.Invalid(fmt.Sprintf("请先把分组改到网卡 %s 的网段（%s），或打开跨网段分组，再换网卡：%s",
			after.Iface, strings.Join(subnets(after.Prefixes), "、"), strings.Join(stranded, "、")))
	}
	if err := s.Store.SystemSettings().Upsert(ctx, settings); err != nil {
		return View{}, err
	}
	if s.Base != nil {
		if err := s.Base.SyncBase(ctx, dhcp.BaseConfig{Iface: after.Iface, ListenAll: settings.AllowCrossSubnet}); err != nil {
			return View{}, err
		}
	}
	return s.view(ctx, settings, ifaces, 30)
}

// SyncBaseAtStartup 按已保存的模式写基础配置，把此前手写配置的节点纳入管理。
func (s Service) SyncBaseAtStartup(ctx context.Context) error {
	if s.Base == nil {
		return nil
	}
	client, err := s.Client(ctx)
	if err != nil {
		return err
	}
	if client.Iface == "" && !client.AllowCrossSubnet {
		return nil // 无可绑定网卡，保持现状
	}
	return s.Base.SyncBase(ctx, dhcp.BaseConfig{Iface: client.Iface, ListenAll: client.AllowCrossSubnet})
}

type ProbeRequest struct {
	IP string `json:"ip"`
}

type ProbeResult struct {
	IP        string  `json:"ip"`
	Reachable bool    `json:"reachable"`
	RTTMillis float64 `json:"rtt_ms,omitempty"`
}

var pingTime = regexp.MustCompile(`time[=<]([0-9.]+) ?ms`)

// Probe 从服务器 ping 一次某地址，用于判断中继网关是否应答；不应答是结果而非错误。
func (s Service) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(req.IP))
	if err != nil || !addr.Is4() {
		return ProbeResult{}, errs.Invalid("要检测的地址不是有效的 IPv4 地址")
	}
	runner := s.Runner
	if runner == nil {
		runner = dhcp.ExecRunner{}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := runner.Run(ctx, "ping", "-c", "1", "-W", "2", addr.String())
	res := ProbeResult{IP: addr.String()}
	if err != nil {
		return res, nil
	}
	res.Reachable = true
	if m := pingTime.FindSubmatch(out); m != nil {
		res.RTTMillis, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	return res, nil
}

func (s Service) settings(ctx context.Context) (domain.SystemSettings, error) {
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil && !errs.IsNotFound(err) {
		return domain.SystemSettings{}, err
	}
	return settings, nil
}

func (s Service) interfaces() ([]Interface, error) {
	if s.Interfaces != nil {
		return s.Interfaces()
	}
	return ReadInterfaces()
}

// ReadInterfaces 列出主机网卡及其 IPv4 网段。
func ReadInterfaces() ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		item := Interface{Name: iface.Name, Up: iface.Flags&net.FlagUp != 0, Loopback: iface.Flags&net.FlagLoopback != 0}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			item.Addrs = append(item.Addrs, prefix)
		}
		out = append(out, item)
	}
	return out, nil
}

// readDefaultGateway 从 /proc/net/route 读 IPv4 默认路由。
func readDefaultGateway() (netip.Addr, bool) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		raw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			continue
		}
		// 小端十六进制
		return netip.AddrFrom4([4]byte{byte(raw), byte(raw >> 8), byte(raw >> 16), byte(raw >> 24)}), true
	}
	return netip.Addr{}, false
}
