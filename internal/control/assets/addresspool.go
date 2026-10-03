package assets

// addressPool 是分组的客户机地址窗口：从 StartIP 起连续 ClientMax 个 IPv4 地址。
// 「客户机 IP 落在分组窗口内」的判定、分配和边界只在这里算，不要在调用处另写区间运算。
type addressPool struct {
	start, end uint32
}

// groupAddressPool 由 StartIP + ClientMax 构造窗口；非 IPv4、非正数量、越过 255.255.255.255 的都拒绝。
func groupAddressPool(startIP string, clientMax int) (addressPool, error) {
	start, err := parseRequiredIPv4(startIP)
	if err != nil {
		return addressPool{}, err
	}
	if clientMax <= 0 {
		return addressPool{}, ErrGroupInvalidNetwork
	}
	startN := ipv4Uint32(start)
	offset := uint64(clientMax - 1)
	if offset > uint64(^uint32(0)-startN) {
		return addressPool{}, ErrGroupInvalidNetwork
	}
	return addressPool{start: startN, end: startN + uint32(offset)}, nil
}

func (p addressPool) contains(n uint32) bool {
	return n >= p.start && n <= p.end
}

// overlaps 判断两个分组是否会发出相同地址。每个分组各写一条 dhcp-range，
// 重叠时客户机拿到哪个网关取决于 dnsmasq 先匹配哪段。
func (p addressPool) overlaps(other addressPool) bool {
	return p.start <= other.end && other.start <= p.end
}

// coversReserved 判断窗口是否包含子网的网络地址或广播地址。这类地址分给客户机后无法通信，
// 而 dnsmasq 照样写入保留，下游不会报错。两者按掩码算而不是看末段：/23 下 x.10.255 是合法主机地址。
// 前提是 start 与 end 同一子网，由 validateGroupNetwork 先行检查。
func (p addressPool) coversReserved(mask uint32) bool {
	network := p.start & mask
	return p.contains(network) || p.contains(network|^mask)
}

// allocate 返回窗口内最小的未占用地址。
func (p addressPool) allocate(occupied map[uint32]bool) (string, bool) {
	for current := p.start; ; current++ {
		if !occupied[current] {
			return ipv4String(current), true
		}
		if current == p.end {
			return "", false
		}
	}
}
