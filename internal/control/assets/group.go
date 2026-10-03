package assets

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrGroupExists            = errs.Conflict("分组已存在")
	ErrGroupInUse             = errs.Conflict("分组正在使用中")
	ErrGroupNameRequired      = errs.Invalid("分组名称必填")
	ErrGroupInvalidNetwork    = errs.Invalid("分组网络配置无效")
	ErrGroupRangeOverlap      = errs.Conflict("IP 区间与其它分组重叠，请改用不重叠的起始 IP 或客户机数")
	ErrGroupGatewayInRange    = errs.Invalid("网关地址落在客户机 IP 区间内，请把网关移出区间或调整起始 IP")
	ErrGroupRangeReserved     = errs.Invalid("客户机 IP 区间包含本网段的网络地址或广播地址，请缩小客户机数或上移起始 IP")
	ErrGroupGatewayConflict   = errs.Conflict("网关与其它分组的客户机 IP 区间冲突，两者不能落在同一地址上")
	ErrGroupRangeCoversServer = errs.Invalid("客户机 IP 区间包含本机自身的地址，落到该地址的终端会拿不到 IP")
	ErrGroupCascadeMismatch   = errs.Invalid("分组的镜像、配置与还原点不匹配")
	ErrGroupReductionRequired = errs.Invalid("分组必须绑定还原点")
	ErrDefaultGroupRequired   = errs.Invalid("必须保留一个默认分组")
)

type GroupService struct {
	Store  store.Store
	DHCP   DHCPSyncer
	Logger *slog.Logger
	Now    func() time.Time
	// Network 是判定分组网段的客户机网络：除非现场有 DHCP 中继，否则拒绝偏离它的区间。nil 时不判定。
	Network NetworkProvider
	// LocalAddrs 返回本机地址。区间若包含本机地址，dnsmasq 会在请求时拒绝分配，
	// 轮到该地址的那台客户机会静默拿不到 IP。nil 时跳过检查；零值不能读真实网卡，否则测试会依赖开发机网络。
	LocalAddrs func() ([]netip.Addr, error)
}

type DHCPSyncer interface {
	Sync(context.Context, []dhcp.GroupConfig) error
	// ReleaseLease 释放某地址仍被旧 MAC 占着的 DHCP 租约。
	ReleaseLease(ctx context.Context, ip, mac string) error
}

// afterGroupFitCheck 在算完扩缩容后机器落点、写入新大小之前调用，生产为 nil。
// 测试要靠它稳定撑开这个竞态窗口，交给调度器时 6 次有 2 次漏检。
var afterGroupFitCheck func()

type GroupListResult struct {
	Items []domain.Group `json:"items"`
	Total int            `json:"total"`
}

type GroupRequest struct {
	Name           string `json:"name"`
	IsDefault      bool   `json:"is_default"`
	StartIP        string `json:"start_ip"`
	ClientMax      int    `json:"client_max"`
	Gateway        string `json:"gateway"`
	Netmask        string `json:"netmask"`
	DNS1           string `json:"dns1"`
	DNS2           string `json:"dns2"`
	SystemImageID  string `json:"system_image_id"`
	SystemConfigID string `json:"system_config_id"`
	// SystemReductionID 仅为兼容旧客户端而接收并忽略：分组启动用的是配置的应用点。
	SystemReductionID string `json:"system_reduction_id"`
	// StorageServerID 把分组客户机固定到某存储节点。指针三态：nil 不改（部分编辑安全），
	// "" 清除固定（回到本机默认），id 固定到该节点。
	StorageServerID *string `json:"storage_server_id"`
}

func (s GroupService) List(ctx context.Context) (GroupListResult, error) {
	items, err := s.Store.Groups().List(ctx)
	if err != nil {
		return GroupListResult{}, err
	}
	if items == nil {
		items = []domain.Group{}
	}
	return GroupListResult{Items: items, Total: len(items)}, nil
}

func (s GroupService) Get(ctx context.Context, id string) (domain.Group, error) {
	return s.Store.Groups().Get(ctx, id)
}

func (s GroupService) GetDefault(ctx context.Context) (domain.Group, error) {
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return domain.Group{}, err
	}
	for _, group := range groups {
		if group.IsDefault {
			return group, nil
		}
	}
	return domain.Group{}, fmt.Errorf("default group: %w", errs.ErrNotFound)
}

func (s GroupService) Create(ctx context.Context, req GroupRequest) (domain.Group, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	group, err := s.prepareGroup(ctx, domain.Group{ID: s.newID()}, req)
	if err != nil {
		return domain.Group{}, err
	}
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return domain.Group{}, err
	}
	for _, item := range groups {
		if item.Name == group.Name {
			return domain.Group{}, ErrGroupExists
		}
	}
	if err := ensureRangeFree(group, groups); err != nil {
		return domain.Group{}, err
	}
	if err := s.ensureRangeClearOfServers(ctx, group); err != nil {
		return domain.Group{}, err
	}
	if err := s.ensureGroupOnClientNetwork(ctx, group, groups); err != nil {
		return domain.Group{}, err
	}
	if len(groups) == 0 {
		group.IsDefault = true
	}

	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if group.IsDefault {
			if err := clearDefaultGroups(ctx, tx, ""); err != nil {
				return err
			}
		}
		return tx.Groups().Create(ctx, group)
	}); err != nil {
		return domain.Group{}, err
	}
	if err := s.syncDHCP(ctx); err != nil {
		return domain.Group{}, err
	}
	return group, nil
}

func (s GroupService) Update(ctx context.Context, id string, req GroupRequest) (domain.Group, error) {
	// 与分配器共用锁：缩小区间时检查的是当前已有终端，并发注册可能把地址落在新区间之外，
	// 这样一行会让之后每次 dnsmasq 同步整体失败，重启也会直接失败。
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	current, err := s.Store.Groups().Get(ctx, id)
	if err != nil {
		return domain.Group{}, err
	}
	if err := refuseSuperGroupChange(ctx, s.Store, id, "修改分组"); err != nil {
		return domain.Group{}, err
	}
	group, err := s.prepareGroup(ctx, current, req)
	if err != nil {
		return domain.Group{}, err
	}
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return domain.Group{}, err
	}
	for _, item := range groups {
		if item.ID != id && item.Name == group.Name {
			return domain.Group{}, ErrGroupExists
		}
	}
	if current.IsDefault && !group.IsDefault && !hasOtherDefault(groups, id) {
		return domain.Group{}, ErrDefaultGroupRequired
	}
	if err := ensureRangeFree(group, groups); err != nil {
		return domain.Group{}, err
	}
	if err := s.ensureRangeClearOfServers(ctx, group); err != nil {
		return domain.Group{}, err
	}
	if err := s.ensureGroupOnClientNetwork(ctx, group, groups); err != nil {
		return domain.Group{}, err
	}
	renumbered, err := s.renumberTerminalsForGroup(ctx, current, group)
	if err != nil {
		return domain.Group{}, err
	}
	if afterGroupFitCheck != nil {
		afterGroupFitCheck()
	}

	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if group.IsDefault {
			if err := clearDefaultGroups(ctx, tx, id); err != nil {
				return err
			}
		}
		for _, item := range renumbered {
			if err := tx.Terminals().Update(ctx, item.terminal); err != nil {
				return err
			}
		}
		return tx.Groups().Update(ctx, group)
	}); err != nil {
		return domain.Group{}, err
	}
	// 旧地址可能仍租给这些机器，而 dnsmasq 不会提供已被别人租用的预留地址，所以写新预留前先释放租约。
	for _, item := range renumbered {
		releaseLease(ctx, s.DHCP, s.Logger, item.oldIP, item.terminal.MAC)
	}
	if err := s.syncDHCP(ctx); err != nil {
		return domain.Group{}, err
	}
	return group, nil
}

// GroupNetworkRequest 是分组编辑中的网络部分，供预览使用。
type GroupNetworkRequest struct {
	StartIP   string `json:"start_ip"`
	ClientMax int    `json:"client_max"`
	Gateway   string `json:"gateway"`
	Netmask   string `json:"netmask"`
}

// GroupNetworkPreviewTerminal 是单台机器改动前后的地址。
type GroupNetworkPreviewTerminal struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	MAC   string `json:"mac"`
	OldIP string `json:"old_ip"`
	NewIP string `json:"new_ip"`
	// Online 表示机器正在运行。保存时它靠租约保持旧地址，续租时 dnsmasq 拒绝旧地址、机器换成新地址；
	// 运行中的 Windows 换 IP 会立刻断开 iSCSI 会话。所以「下次开机生效」对预留成立，对机器不成立。
	Online bool `json:"online"`
}

// GroupNetworkPreview 描述网络改动会把哪些机器挪到哪里；已在新区间内的机器不列出。
type GroupNetworkPreview struct {
	Status    NetworkStatus                 `json:"status"`
	Terminals []GroupNetworkPreviewTerminal `json:"terminals"`
	// OnlineCount 是其中正在运行的台数。为 0 时可以放心不拦。
	OnlineCount int `json:"online_count"`
}

// PreviewNetwork 执行网络改动的检查与地址重分，但不写入；用于「改到服务器网段」的确认框。
func (s GroupService) PreviewNetwork(ctx context.Context, id string, req GroupNetworkRequest) (GroupNetworkPreview, error) {
	current, err := s.Store.Groups().Get(ctx, id)
	if err != nil {
		return GroupNetworkPreview{}, err
	}
	full := GroupRequest{
		Name: current.Name, IsDefault: current.IsDefault,
		StartIP: req.StartIP, ClientMax: req.ClientMax, Gateway: req.Gateway, Netmask: req.Netmask,
		DNS1: current.DNS1, DNS2: current.DNS2,
		SystemImageID: current.SystemImageID, SystemConfigID: current.SystemConfigID,
	}
	if err := validateGroupNetwork(full); err != nil {
		return GroupNetworkPreview{}, err
	}
	group := current
	group.StartIP, group.ClientMax = strings.TrimSpace(req.StartIP), req.ClientMax
	group.Gateway, group.Netmask = strings.TrimSpace(req.Gateway), strings.TrimSpace(req.Netmask)
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return GroupNetworkPreview{}, err
	}
	if err := ensureRangeFree(group, groups); err != nil {
		return GroupNetworkPreview{}, err
	}
	if err := s.ensureRangeClearOfServers(ctx, group); err != nil {
		return GroupNetworkPreview{}, err
	}
	if err := s.ensureGroupOnClientNetwork(ctx, group, groups); err != nil {
		return GroupNetworkPreview{}, err
	}
	renumbered, err := s.renumberTerminalsForGroup(ctx, current, group)
	if err != nil {
		return GroupNetworkPreview{}, err
	}
	preview := GroupNetworkPreview{Status: ClassifyGroupNetwork(group, s.clientNetwork(ctx)), Terminals: []GroupNetworkPreviewTerminal{}}
	for _, item := range renumbered {
		online := item.terminal.State == domain.TerminalStateOnline
		if online {
			preview.OnlineCount++
		}
		preview.Terminals = append(preview.Terminals, GroupNetworkPreviewTerminal{
			ID: item.terminal.ID, Name: item.terminal.Name, MAC: item.terminal.MAC,
			OldIP: item.oldIP, NewIP: item.terminal.IP, Online: online,
		})
	}
	sort.Slice(preview.Terminals, func(i, j int) bool { return preview.Terminals[i].OldIP < preview.Terminals[j].OldIP })
	return preview, nil
}

type renumberedTerminal struct {
	terminal domain.Terminal
	oldIP    string
}

// renumberTerminalsForGroup 把分组机器挪进新地址区间：原偏移空闲则保留偏移（start+3 仍是 start+3），
// 否则取最小空闲地址；已在新区间内的不动。区间容量小于机器数时拒绝，并给出两个数字供操作者调整。
func (s GroupService) renumberTerminalsForGroup(ctx context.Context, before, group domain.Group) ([]renumberedTerminal, error) {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return nil, err
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return nil, err
	}
	occupied := map[uint32]bool{}
	var members, misfits []domain.Terminal
	for _, terminal := range terminals {
		ip, err := parseRequiredIPv4(terminal.IP)
		if err != nil {
			return nil, err
		}
		n := ipv4Uint32(ip)
		if terminal.GroupID != group.ID {
			occupied[n] = true // 其他分组的地址，这里不再分配
			continue
		}
		members = append(members, terminal)
		if pool.contains(n) {
			occupied[n] = true // 已在新区间内，保持原地址
			continue
		}
		misfits = append(misfits, terminal)
	}
	if len(members) > group.ClientMax {
		return nil, GroupRangeTooSmall{Group: group, Terminals: len(members), Addresses: group.ClientMax}
	}
	if len(misfits) == 0 {
		return nil, nil
	}
	// 按旧地址升序，让偏移按教室顺序认领。
	sort.Slice(misfits, func(i, j int) bool { return misfits[i].IP < misfits[j].IP })
	// 第一轮：旧区间已知且槽位空闲时保持相同偏移；第二轮：其余取最小空闲地址。
	oldStart, oldStartErr := parseRequiredIPv4(before.StartIP)
	renumbered := make([]renumberedTerminal, 0, len(misfits))
	pending := make([]domain.Terminal, 0, len(misfits))
	for _, terminal := range misfits {
		placed := false
		if oldStartErr == nil {
			ip, _ := parseRequiredIPv4(terminal.IP)
			oldN, oldStartN := ipv4Uint32(ip), ipv4Uint32(oldStart)
			if oldN >= oldStartN {
				want := pool.start + (oldN - oldStartN)
				if pool.contains(want) && !occupied[want] {
					occupied[want] = true
					renumbered = append(renumbered, renumberedTerminal{terminal: withIP(terminal, ipv4String(want)), oldIP: terminal.IP})
					placed = true
				}
			}
		}
		if !placed {
			pending = append(pending, terminal)
		}
	}
	for _, terminal := range pending {
		ip, ok := pool.allocate(occupied)
		if !ok {
			return nil, GroupRangeTooSmall{Group: group, Terminals: len(members), Addresses: group.ClientMax}
		}
		occupied[ipv4Uint32(netip.MustParseAddr(ip))] = true
		renumbered = append(renumbered, renumberedTerminal{terminal: withIP(terminal, ip), oldIP: terminal.IP})
	}
	return renumbered, nil
}

func withIP(terminal domain.Terminal, ip string) domain.Terminal {
	terminal.IP = ip
	return terminal
}

// GroupRangeTooSmall 表示新区间地址数少于分组机器数而被拒绝的网络改动。
type GroupRangeTooSmall struct {
	Group     domain.Group
	Terminals int
	Addresses int
}

func (e GroupRangeTooSmall) Error() string {
	return fmt.Sprintf("分组 %s 有 %d 台终端，新范围只有 %d 个地址，装不下；把最大客户机数至少改到 %d", e.Group.Name, e.Terminals, e.Addresses, e.Terminals)
}

func (e GroupRangeTooSmall) Is(target error) bool { return target == errs.ErrInvalid }

func (s GroupService) Delete(ctx context.Context, id string) error {
	// 理由同 Update：「已无终端」检查与删除之间不能被落入本组的注册插队。
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	group, err := s.Store.Groups().Get(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseSuperGroupChange(ctx, s.Store, id, "删除分组"); err != nil {
		return err
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return err
	}
	for _, terminal := range terminals {
		if terminal.GroupID == id {
			return ErrGroupInUse
		}
	}
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return err
	}
	nextDefault := ""
	if group.IsDefault {
		for _, item := range groups {
			if item.ID != id {
				nextDefault = item.ID
				break
			}
		}
	}
	if nextDefault != "" {
		if err := refuseSuperGroupChange(ctx, s.Store, nextDefault, "切换默认分组"); err != nil {
			return err
		}
	}

	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Groups().Delete(ctx, id); err != nil {
			return err
		}
		if nextDefault == "" {
			return nil
		}
		next, err := tx.Groups().Get(ctx, nextDefault)
		if err != nil {
			return err
		}
		next.IsDefault = true
		return tx.Groups().Update(ctx, next)
	}); err != nil {
		return err
	}
	return s.syncDHCP(ctx)
}

func (s GroupService) SetDefault(ctx context.Context, id string) (domain.Group, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	group, err := s.Store.Groups().Get(ctx, id)
	if err != nil {
		return domain.Group{}, err
	}
	if err := refuseSuperGroupChange(ctx, s.Store, id, "切换默认分组"); err != nil {
		return domain.Group{}, err
	}
	group.IsDefault = true
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := clearDefaultGroups(ctx, tx, id); err != nil {
			return err
		}
		return tx.Groups().Update(ctx, group)
	}); err != nil {
		return domain.Group{}, err
	}
	return group, nil
}

func (s GroupService) prepareGroup(ctx context.Context, group domain.Group, req GroupRequest) (domain.Group, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return domain.Group{}, ErrGroupNameRequired
	}
	if err := validateGroupNetwork(req); err != nil {
		return domain.Group{}, err
	}
	// 还原点一律取配置的应用点，不看请求。
	reductionID, err := s.resolveSystemTriple(ctx, req.SystemImageID, req.SystemConfigID, "")
	if err != nil {
		return domain.Group{}, err
	}
	group.Name = req.Name
	group.IsDefault = req.IsDefault
	group.StartIP = strings.TrimSpace(req.StartIP)
	group.ClientMax = req.ClientMax
	group.Gateway = strings.TrimSpace(req.Gateway)
	group.Netmask = strings.TrimSpace(req.Netmask)
	group.DNS1 = strings.TrimSpace(req.DNS1)
	group.DNS2 = strings.TrimSpace(req.DNS2)
	group.SystemImageID = strings.TrimSpace(req.SystemImageID)
	group.SystemConfigID = strings.TrimSpace(req.SystemConfigID)
	group.SystemReductionID = reductionID
	if req.StorageServerID != nil {
		id := strings.TrimSpace(*req.StorageServerID)
		if id == "" {
			group.StorageServerID = nil
		} else {
			if _, err := s.Store.Servers().Get(ctx, id); err != nil {
				return domain.Group{}, fmt.Errorf("存储节点 %s 不存在，请刷新节点列表后重试", id)
			}
			group.StorageServerID = &id
		}
	}
	return group, nil
}

func (s GroupService) resolveSystemTriple(ctx context.Context, imageID, configID, reductionID string) (string, error) {
	imageID = strings.TrimSpace(imageID)
	configID = strings.TrimSpace(configID)
	reductionID = strings.TrimSpace(reductionID)
	if imageID == "" || configID == "" {
		return "", ErrGroupCascadeMismatch
	}
	img, err := s.Store.Images().Get(ctx, imageID)
	if err != nil {
		return "", err
	}
	if img.Purpose == domain.ImagePurposeData {
		return "", errs.Invalid(fmt.Sprintf("%s 是数据盘镜像，不能作分组的系统盘；系统盘请选系统盘镜像", img.Name))
	}
	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return "", err
	}
	if cfg.ImageID != imageID {
		return "", ErrGroupCascadeMismatch
	}
	if reductionID == "" {
		if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID == "" {
			return "", ErrGroupReductionRequired
		}
		reductionID = *cfg.DefaultReductionID
	}
	reduction, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return "", err
	}
	if reduction.ConfigID != configID {
		return "", ErrGroupCascadeMismatch
	}
	// C-12 门禁：镜像最近一次健康检查为 block 级时拒绝绑定；无报告或 unknown/warn 放行。
	report, err := s.Store.ImageHealthReports().GetByImage(ctx, imageID)
	if err == nil && report.Level == domain.HealthBlock {
		return "", fmt.Errorf("%w: %s", ErrImageHealthBlocked, healthBlockSummary(report))
	} else if err != nil && !errs.IsNotFound(err) {
		return "", err
	}
	return reduction.ID, nil
}

func healthBlockSummary(report domain.ImageHealthReport) string {
	var reasons []string
	for _, item := range report.Items {
		if item.Level == domain.HealthBlock {
			reasons = append(reasons, item.Detail)
		}
	}
	if len(reasons) == 0 {
		return "镜像体检为 block 级"
	}
	return strings.Join(reasons, "；")
}

// ensureRangeFree 拒绝与其他分组区间重叠的分组。终端 IP 本身全局唯一，但两段重叠的 dhcp-range
// 会让同一地址拿到的网关、掩码、DNS 取决于 dnsmasq 匹配到哪一段。
func ensureRangeFree(group domain.Group, existing []domain.Group) error {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return err
	}
	gateway, gatewayOK := parseRequiredIPv4(group.Gateway)
	for _, other := range existing {
		if other.ID == group.ID {
			continue
		}
		otherPool, err := groupAddressPool(other.StartIP, other.ClientMax)
		if err != nil {
			continue // 已存的坏区间分组不归这里处理
		}
		if pool.overlaps(otherPool) {
			return ErrGroupRangeOverlap
		}
		// 区间不重叠也可能抢同一地址：本组区间吞掉别组网关，或本组网关落在别组区间。
		// 别组把该地址分给机器后，一个教室就断网，而两组各自看都合法。
		if otherGateway, err := parseRequiredIPv4(other.Gateway); err == nil {
			if pool.contains(ipv4Uint32(otherGateway)) {
				return ErrGroupGatewayConflict
			}
		}
		if gatewayOK == nil && otherPool.contains(ipv4Uint32(gateway)) {
			return ErrGroupGatewayConflict
		}
	}
	return nil
}

// ensureRangeClearOfServers 拒绝吞掉集群自身地址的客户机区间；地址冲突的症状（节点悄悄掉线）离原因很远。
// 两个来源互不覆盖：本机网卡能看到没登记的地址（第二张网卡、别名），
// 节点名册能看到其他节点和 VIP（本机网卡只在持有 VIP 时才看得到）。
func (s GroupService) ensureRangeClearOfServers(ctx context.Context, group domain.Group) error {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return err
	}
	var local []netip.Addr
	if s.LocalAddrs != nil {
		if local, err = s.LocalAddrs(); err != nil {
			// 读不到网卡不算冲突证据，为此拒绝所有分组编辑得不偿失；名册照常检查。
			warnDHCP(s.Logger, "could not read this server's own addresses; the roster is still checked", err)
		}
	}
	for _, a := range serverAddresses(ctx, s.Store, local, s.Logger) {
		if pool.contains(ipv4Uint32(a.Addr)) {
			return GroupRangeCoversServer{Address: a.Addr.String(), Node: a.Label}
		}
	}
	return nil
}

// ServerAddress 是集群应答的一个地址及其归属。
type ServerAddress struct {
	Addr  netip.Addr
	Label string // 本机地址 / 节点 / 集群虚拟 IP / 服务地址
}

// ClusterAddresses 列出名册中的地址：每个节点自身地址，以及与之不同的服务地址（HA 集群中即 VIP）。
func ClusterAddresses(ctx context.Context, st store.Store) ([]ServerAddress, error) {
	servers, err := st.Servers().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []ServerAddress
	for _, srv := range servers {
		if a, err := parseRequiredIPv4(srv.IP); err == nil {
			out = append(out, ServerAddress{a, "节点"})
		}
		if a, err := parseRequiredIPv4(srv.PortalIP); err == nil && strings.TrimSpace(srv.PortalIP) != strings.TrimSpace(srv.IP) {
			// HA 集群中写入者的门户是 VIP；其他情况就是该节点提供磁盘的地址。
			label := "服务地址"
			if srv.HAState != "" {
				label = "集群虚拟 IP"
			}
			out = append(out, ServerAddress{a, label})
		}
	}
	return out, nil
}

// serverAddresses 合并本机与名册地址，每个地址一条并排序；VIP 即使在本机网卡上也保留 VIP 名称。
func serverAddresses(ctx context.Context, st store.Store, local []netip.Addr, logger *slog.Logger) []ServerAddress {
	label := map[netip.Addr]string{}
	for _, a := range local {
		if a.Is4() {
			label[a] = "本机地址"
		}
	}
	roster, err := ClusterAddresses(ctx, st)
	if err != nil {
		warnDHCP(logger, "could not read the roster; the range was checked against this machine only", err)
	}
	for _, a := range roster {
		if a.Label == "集群虚拟 IP" || label[a.Addr] == "" {
			label[a.Addr] = a.Label
		}
	}
	out := make([]ServerAddress, 0, len(label))
	for a, l := range label {
		out = append(out, ServerAddress{a, l})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.Less(out[j].Addr) })
	return out
}

// reserveClusterAddrs 把名册地址标为已占，使区间覆盖了后加节点的分组也不会把该节点地址分出去。
func reserveClusterAddrs(ctx context.Context, st store.Store, occupied map[uint32]bool) {
	addrs, err := ClusterAddresses(ctx, st)
	if err != nil {
		return // 保存时的区间检查已排除它们
	}
	for _, a := range addrs {
		occupied[ipv4Uint32(a.Addr)] = true
	}
}

// GroupRangeCoversServer 点名冲突的地址及其归属：集群里有多台机器和虚拟地址，只说「本机地址」无法定位。
type GroupRangeCoversServer struct {
	Address string
	Node    string
}

func (e GroupRangeCoversServer) Error() string {
	return fmt.Sprintf("客户机 IP 区间包含%s %s，分到这个地址的客户机会和它冲突；请缩小客户机数或上移起始 IP",
		e.Node, e.Address)
}

// Is 同时匹配非法请求类别和旧哨兵错误，按类别判断的调用方不受影响。
func (e GroupRangeCoversServer) Is(target error) bool {
	return target == errs.ErrInvalid || target == ErrGroupRangeCoversServer
}

func (s GroupService) syncDHCP(ctx context.Context) error {
	return syncDHCP(ctx, s.Store, s.DHCP, s.Logger)
}

func (s GroupService) SyncDHCP(ctx context.Context) error {
	return s.syncDHCP(ctx)
}

func buildDHCPConfigs(ctx context.Context, st store.Store) ([]dhcp.GroupConfig, error) {
	groups, err := st.Groups().List(ctx)
	if err != nil {
		return nil, err
	}
	terminals, err := st.Terminals().List(ctx)
	if err != nil {
		return nil, err
	}
	byGroup := make(map[string][]domain.Terminal)
	for _, terminal := range terminals {
		byGroup[terminal.GroupID] = append(byGroup[terminal.GroupID], terminal)
	}
	out := make([]dhcp.GroupConfig, 0, len(groups))
	for _, group := range groups {
		out = append(out, dhcp.GroupConfig{Group: group, Terminals: byGroup[group.ID]})
	}
	return out, nil
}

func syncDHCP(ctx context.Context, st store.Store, syncer DHCPSyncer, logger *slog.Logger) error {
	if syncer == nil {
		return nil
	}
	configs, err := buildDHCPConfigs(ctx, st)
	if err != nil {
		warnDHCP(logger, "build dnsmasq config failed", err)
		return err
	}
	if err := syncer.Sync(ctx, configs); err != nil {
		warnDHCP(logger, "sync dnsmasq config failed", err)
		return err
	}
	return nil
}

func warnDHCP(logger *slog.Logger, message string, err error) {
	if logger != nil {
		logger.Warn(message, "error", err)
	}
}

func (s GroupService) newID() string {
	return fmt.Sprintf("group-%d", s.now().UnixNano())
}

func (s GroupService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validateGroupNetwork(req GroupRequest) error {
	pool, err := groupAddressPool(req.StartIP, req.ClientMax)
	if err != nil {
		return err
	}
	// 网关可选：没有路由器的教室可不填，机器就没有默认路由；填了则必须在客户机子网内且不在区间内。
	gateway, hasGateway := netip.Addr{}, strings.TrimSpace(req.Gateway) != ""
	if hasGateway {
		var err error
		if gateway, err = parseRequiredIPv4(req.Gateway); err != nil {
			return err
		}
	}
	mask, err := parseNetmask(req.Netmask)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.DNS1) != "" {
		if _, err := parseRequiredIPv4(req.DNS1); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.DNS2) != "" {
		if _, err := parseRequiredIPv4(req.DNS2); err != nil {
			return err
		}
	}

	maskN := ipv4Uint32(mask)
	if pool.start&maskN != pool.end&maskN {
		return ErrGroupInvalidNetwork
	}
	if hasGateway {
		if pool.start&maskN != ipv4Uint32(gateway)&maskN {
			return groupNetworkProblem(fmt.Sprintf("网关 %s 和分组网段 %s 不在一个子网", gateway, prefixString(pool.start&maskN, maskN)))
		}
		// 网关必在客户机子网内，但必须排除在区间外，否则迟早被分给终端，整组断网；
		// 症状（一台开机、整个教室掉线）离原因很远，只能在这里拦。
		if pool.contains(ipv4Uint32(gateway)) {
			return ErrGroupGatewayInRange
		}
	}
	if pool.coversReserved(maskN) {
		return ErrGroupRangeReserved
	}
	return nil
}

// groupNetworkProblem 是带具体原因的 ErrGroupInvalidNetwork：按通用错误判断的调用方仍能匹配。
type groupNetworkProblem string

func (e groupNetworkProblem) Error() string { return string(e) }
func (e groupNetworkProblem) Is(target error) bool {
	return target == ErrGroupInvalidNetwork || target == errs.ErrInvalid
}

func prefixString(network, mask uint32) string {
	bits := 0
	for m := mask; m&0x80000000 != 0; m <<= 1 {
		bits++
	}
	return fmt.Sprintf("%s/%d", ipv4String(network), bits)
}

func parseRequiredIPv4(value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, ErrGroupInvalidNetwork
	}
	return addr, nil
}

func parseNetmask(value string) (netip.Addr, error) {
	addr, err := parseRequiredIPv4(value)
	if err != nil {
		return netip.Addr{}, err
	}
	raw := addr.As4()
	mask := net.IPMask(raw[:])
	ones, bits := mask.Size()
	if bits != 32 || ones <= 0 {
		return netip.Addr{}, ErrGroupInvalidNetwork
	}
	return addr, nil
}

func ipv4Uint32(addr netip.Addr) uint32 {
	raw := addr.As4()
	return uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
}

func clearDefaultGroups(ctx context.Context, st store.Store, exceptID string) error {
	groups, err := st.Groups().List(ctx)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.IsDefault && group.ID != exceptID {
			if err := refuseSuperGroupChange(ctx, st, group.ID, "切换默认分组"); err != nil {
				return err
			}
		}
	}
	for _, group := range groups {
		if group.IsDefault && group.ID != exceptID {
			group.IsDefault = false
			if err := st.Groups().Update(ctx, group); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasOtherDefault(groups []domain.Group, id string) bool {
	for _, group := range groups {
		if group.ID != id && group.IsDefault {
			return true
		}
	}
	return false
}
