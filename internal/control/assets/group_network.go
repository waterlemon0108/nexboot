package assets

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// ClientNetwork 是服务器面向教室的网络：dnsmasq 服务客户机的网卡、其 IPv4 网段、本机持有的全部地址，
// 以及是否允许网段外的分组（交换机上有 DHCP 中继的现场才打开）。
// 读不到网卡或客户机网卡无地址时 Known 为 false，此时不据此判定任何事。
type ClientNetwork struct {
	Iface            string
	Prefixes         []netip.Prefix
	ServerAddrs      []netip.Addr
	RouteGateway     netip.Addr // 服务器的默认网关（如有）
	AllowCrossSubnet bool
	Known            bool
}

// Contains 判断地址是否在某个客户机网段内。
func (n ClientNetwork) Contains(addr netip.Addr) bool {
	for _, prefix := range n.Prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// IsServerAddr 判断地址是否为服务器自身地址。
func (n ClientNetwork) IsServerAddr(addr netip.Addr) bool {
	for _, a := range n.ServerAddrs {
		if a == addr {
			return true
		}
	}
	return false
}

// ServerAddrsInGroup 列出落在分组区间内的服务器地址，即分组保存后才加入的节点或 VIP。
func ServerAddrsInGroup(group domain.Group, net ClientNetwork) []netip.Addr {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range net.ServerAddrs {
		if a.Is4() && pool.contains(ipv4Uint32(a)) {
			out = append(out, a)
		}
	}
	return out
}

// AddressUsers 列出已在使用 ip 的对象：区间覆盖它的分组和持有它的客户机。读取失败时什么都不报。
func AddressUsers(ctx context.Context, st store.Store, ip string) []string {
	addr, err := parseRequiredIPv4(ip)
	if err != nil {
		return nil
	}
	var users []string
	if groups, err := st.Groups().List(ctx); err == nil {
		for _, g := range groups {
			if pool, err := groupAddressPool(g.StartIP, g.ClientMax); err == nil && pool.contains(ipv4Uint32(addr)) {
				users = append(users, "分组「"+g.Name+"」的地址范围")
			}
		}
	}
	if terminals, err := st.Terminals().List(ctx); err == nil {
		for _, t := range terminals {
			if strings.TrimSpace(t.IP) == addr.String() {
				name := t.Name
				if name == "" {
					name = t.MAC
				}
				users = append(users, "客户机「"+name+"」")
			}
		}
	}
	return users
}

// NetworkProvider 向 GroupService 提供判定分组用的客户机网络。
type NetworkProvider func(context.Context) (ClientNetwork, error)

// NetworkStatus 表示分组区间相对客户机网络的位置。
type NetworkStatus string

const (
	NetworkSame    NetworkStatus = "same"    // 在客户机网段内：DHCP 广播能到达服务器
	NetworkRelay   NetworkStatus = "relay"   // 在网段外且现场有 DHCP 中继：允许，网关即中继
	NetworkBlocked NetworkStatus = "blocked" // 在网段外且无中继：机器永远拿不到地址
	NetworkUnknown NetworkStatus = "unknown" // 读不到服务器自身网络
)

// ClassifyGroupNetwork 是判定分组区间能否被服务的唯一入口：表单状态行、新建/修改拒绝、告警和网络页都调用它。
func ClassifyGroupNetwork(group domain.Group, net ClientNetwork) NetworkStatus {
	if !net.Known || len(net.Prefixes) == 0 {
		return NetworkUnknown
	}
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return NetworkUnknown
	}
	start, end := addrOf(pool.start), addrOf(pool.end)
	for _, prefix := range net.Prefixes {
		if prefix.Contains(start) && prefix.Contains(end) {
			return NetworkSame
		}
	}
	if net.AllowCrossSubnet {
		return NetworkRelay
	}
	return NetworkBlocked
}

// SuggestedRange 是产品为分组算出的区间：新分组表单的初始值，也是拒绝时给出的替代方案。
type SuggestedRange struct {
	StartIP   string `json:"start_ip"`
	ClientMax int    `json:"client_max"`
	Netmask   string `json:"netmask"`
	Gateway   string `json:"gateway"`
}

// SuggestGroupRange 在客户机网段内挑 count 个连续地址：先从主机号 .10 起，再从 .2 起，
// 跳过服务器地址、网络/广播地址和其他分组区间（正在编辑的 exceptGroupID 不挡自己）。
// 服务器默认网关在同网段时用作网关，否则不填，因为服务器不做路由。
func SuggestGroupRange(net ClientNetwork, others []domain.Group, count int, exceptGroupID string) (SuggestedRange, bool) {
	if count <= 0 || len(net.Prefixes) == 0 {
		return SuggestedRange{}, false
	}
	taken := make([]addressPool, 0, len(others))
	for _, g := range others {
		if g.ID == exceptGroupID {
			continue
		}
		if pool, err := groupAddressPool(g.StartIP, g.ClientMax); err == nil {
			taken = append(taken, pool)
		}
	}
	for _, prefix := range net.Prefixes {
		prefix = prefix.Masked()
		if !prefix.Addr().Is4() {
			continue
		}
		network := ipv4Uint32(prefix.Addr())
		mask := uint32(0xffffffff) << (32 - prefix.Bits())
		broadcast := network | ^mask
		blocked := func(n uint32) bool {
			if n == network || n == broadcast || net.IsServerAddr(addrOf(n)) {
				return true
			}
			for _, p := range taken {
				if p.contains(n) {
					return true
				}
			}
			return false
		}
		for _, first := range []uint32{network + 10, network + 2} {
			for start := first; start+uint32(count)-1 <= broadcast && start >= first; start++ {
				fits := true
				for n := start; n < start+uint32(count); n++ {
					if blocked(n) {
						fits = false
						start = n // 跳过障碍
						break
					}
				}
				if fits {
					gateway := ""
					if net.RouteGateway.IsValid() && prefix.Contains(net.RouteGateway) {
						gateway = net.RouteGateway.String()
					}
					return SuggestedRange{StartIP: ipv4String(start), ClientMax: count, Netmask: netmaskString(prefix.Bits()), Gateway: gateway}, true
				}
			}
		}
	}
	return SuggestedRange{}, false
}

func addrOf(n uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

func netmaskString(bits int) string {
	mask := uint32(0xffffffff) << (32 - bits)
	return ipv4String(mask)
}

// GroupOutsideClientNetwork 表示分组区间不在客户机网段内、又无 DHCP 中继而被拒绝。
// 它点名网卡、网段和区间，并给出可用的区间或能让当前区间生效的开关。
type GroupOutsideClientNetwork struct {
	Group   domain.Group
	Network ClientNetwork
	Suggest *SuggestedRange
}

func (e GroupOutsideClientNetwork) Error() string {
	pool, _ := groupAddressPool(e.Group.StartIP, e.Group.ClientMax)
	nets := make([]string, 0, len(e.Network.Prefixes))
	for _, p := range e.Network.Prefixes {
		nets = append(nets, p.String())
	}
	sort.Strings(nets)
	msg := fmt.Sprintf("分组 %s 的网段 %s–%s 不在服务器客户机网卡 %s（%s）的网段内，客户机将拿不到地址",
		e.Group.Name, ipv4String(pool.start), ipv4String(pool.end), e.Network.Iface, strings.Join(nets, "、"))
	if e.Suggest != nil {
		end, _ := groupAddressPool(e.Suggest.StartIP, e.Suggest.ClientMax)
		msg += fmt.Sprintf("；改成 %s–%s", e.Suggest.StartIP, ipv4String(end.end))
	} else {
		msg += "；改到服务器网段"
	}
	return msg + "，或者客户机确实在别的 VLAN → 在系统参数打开「跨网段分组」"
}

func (e GroupOutsideClientNetwork) Is(target error) bool { return target == errs.ErrInvalid }

// clientNetwork 通过 provider 读取客户机网络；没有 provider（测试或无此能力的节点）时不判定。
func (s GroupService) clientNetwork(ctx context.Context) ClientNetwork {
	if s.Network == nil {
		return ClientNetwork{}
	}
	net, err := s.Network(ctx)
	if err != nil {
		warnDHCP(s.Logger, "could not read the client network; skipping the network check", err)
		return ClientNetwork{}
	}
	return net
}

// ensureGroupOnClientNetwork 拒绝无法被服务的区间。网关可以是服务器本身（双网卡服务器给教室做路由很常见），这里不判定网关。
func (s GroupService) ensureGroupOnClientNetwork(ctx context.Context, group domain.Group, others []domain.Group) error {
	net := s.clientNetwork(ctx)
	if ClassifyGroupNetwork(group, net) != NetworkBlocked {
		return nil
	}
	err := GroupOutsideClientNetwork{Group: group, Network: net}
	if suggest, ok := SuggestGroupRange(net, others, group.ClientMax, group.ID); ok {
		err.Suggest = &suggest
	}
	return err
}
