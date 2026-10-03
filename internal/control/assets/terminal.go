package assets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrTerminalMACExists     = errs.Conflict("终端 MAC 已存在")
	ErrTerminalIPExists      = errs.Conflict("终端 IP 已存在")
	ErrTerminalInvalidMAC    = errs.Invalid("终端 MAC 无效")
	ErrTerminalInvalidIP     = errs.Invalid("终端 IP 无效")
	ErrTerminalGroupFull     = errs.Conflict("分组终端数已满")
	ErrTerminalInvalidState  = errs.Invalid("终端状态无效")
	ErrTerminalNoAvailableIP = errs.Conflict("分组内没有可用 IP")
	ErrTerminalMoveRequired  = errs.Invalid("必须选择终端与目标分组")
	ErrTerminalSuperConflict = errs.Conflict("该配置已启用超管机")
	ErrTerminalSuperRequired = errs.Conflict("该终端不是超管机")
	ErrTerminalOfflineNeeded = errs.Conflict("终端必须处于离线状态")
)

var terminalAllocMu sync.Mutex

type TerminalService struct {
	Store         store.Store
	DHCP          DHCPSyncer
	Storage       storage.StorageAgent
	Logger        *slog.Logger
	Now           func() time.Time
	BackupTrigger func(context.Context) error
	Async         bool
	// QuietWindow 是数据盘发布前须保持无写入的时长；为零用默认值。
	QuietWindow time.Duration
	// Place 非空时，克隆操作路由到实际持有克隆的节点（终端记录的放置），在线统计扩到所有存活节点；为 nil 走单节点路径。
	Place *place.Router
}

// hostFor 返回服务该终端、持有其克隆的 agent。
func (s TerminalService) hostFor(ctx context.Context, t domain.Terminal) storage.ClientHostAgent {
	if s.Place == nil {
		return s.Storage
	}
	return s.Place.ForTerminal(ctx, t).Agent
}

// hosts 列出可能持有客户机的 agent：无放置时只有本机，有放置时为本机加所有心跳新鲜的远端。
func (s TerminalService) hosts(ctx context.Context) []place.Placed {
	if s.Place == nil {
		return []place.Placed{{Agent: s.Storage, Healthy: true}}
	}
	return s.Place.AllHosts(ctx)
}

// activeMACsAllHosts 合并所有 host 的活动会话。任一 host 失败即整体失败：不应答节点上的会话
// 不等于不存在；彻底失联的节点心跳过期后会自行退出统计。
func (s TerminalService) activeMACsAllHosts(ctx context.Context) ([]string, error) {
	var all []string
	for _, h := range s.hosts(ctx) {
		macs, err := h.Agent.ActiveClientMACs(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, macs...)
	}
	return all, nil
}

type TerminalListResult struct {
	Items []domain.Terminal `json:"items"`
	Total int               `json:"total"`
}

type TerminalRequest struct {
	MAC     string               `json:"mac"`
	IP      string               `json:"ip"`
	GroupID string               `json:"group_id"`
	Name    string               `json:"name"`
	IsSuper bool                 `json:"is_super"`
	State   domain.TerminalState `json:"state"`
}

type MoveTerminalsRequest struct {
	TerminalIDs []string `json:"terminal_ids"`
	GroupID     string   `json:"group_id"`
}

type MoveTerminalsResult struct {
	Moved int               `json:"moved"`
	Items []domain.Terminal `json:"items"`
}

// SuperStopDiskRequest 指定超管机停机时某块数据盘存成的还原点名；未列出或名字为空的盘直接丢弃会话。
type SuperStopDiskRequest struct {
	DiskID        string `json:"disk_id"`
	ReductionName string `json:"reduction_name"`
}

type SuperStopRequest struct {
	ReductionName string                 `json:"reduction_name"`
	DataDisks     []SuperStopDiskRequest `json:"data_disks,omitempty"`
}

type SuperStopResult struct {
	TaskID         string             `json:"task_id"`
	Reduction      *domain.Reduction  `json:"reduction,omitempty"`
	DataReductions []domain.Reduction `json:"data_reductions,omitempty"`
}

// superStopDisk 是停机时解析出的一块数据盘：分组盘、导出的 LUN、要存成的还原点。
type superStopDisk struct {
	disk        domain.GroupDisk
	config      domain.Config
	lun         int
	name        string // 快照名，"@..."
	displayName string
}

type TerminalSuperConflict struct {
	ConfigID   string
	TerminalID string
	MAC        string
}

func (e TerminalSuperConflict) Error() string {
	if e.MAC != "" {
		return fmt.Sprintf("配置 %s 已有超管机 %s（%s）在用；先让它停机存盘或取消超管，再设置这台", e.ConfigID, e.TerminalID, e.MAC)
	}
	return ErrTerminalSuperConflict.Error()
}

func (e TerminalSuperConflict) Is(target error) bool {
	return target == ErrTerminalSuperConflict || target == errs.ErrConflict
}

func (s TerminalService) List(ctx context.Context, groupID string) (TerminalListResult, error) {
	items, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return TerminalListResult{}, err
	}
	groupID = strings.TrimSpace(groupID)
	out := make([]domain.Terminal, 0, len(items))
	for _, item := range items {
		if groupID == "" || item.GroupID == groupID {
			out = append(out, item)
		}
	}
	if out == nil {
		out = []domain.Terminal{}
	}
	return TerminalListResult{Items: out, Total: len(out)}, nil
}

func (s TerminalService) Get(ctx context.Context, id string) (domain.Terminal, error) {
	return s.Store.Terminals().Get(ctx, id)
}

func (s TerminalService) Create(ctx context.Context, req TerminalRequest) (domain.Terminal, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	terminal, err := terminalFromRequest(domain.Terminal{}, req)
	if err != nil {
		return domain.Terminal{}, err
	}
	terminal.ID = "terminal-" + terminal.MAC
	if terminal.IP == "" {
		ip, err := s.allocateIP(ctx, terminal.GroupID)
		if err != nil {
			return domain.Terminal{}, err
		}
		terminal.IP = ip
	}
	if err := s.validateTerminal(ctx, terminal, ""); err != nil {
		return domain.Terminal{}, err
	}
	if terminal.IsSuper {
		if err := EnsureSuperAvailable(ctx, s.Store, terminal); err != nil {
			return domain.Terminal{}, err
		}
	}
	if err := s.Store.Terminals().Create(ctx, terminal); err != nil {
		return domain.Terminal{}, err
	}
	if err := syncDHCP(ctx, s.Store, s.DHCP, s.Logger); err != nil {
		return domain.Terminal{}, err
	}
	return terminal, nil
}

func (s TerminalService) Update(ctx context.Context, id string, req TerminalRequest) (domain.Terminal, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	current, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return domain.Terminal{}, err
	}
	terminal, err := terminalFromRequest(current, req)
	if err != nil {
		return domain.Terminal{}, err
	}
	if current.IsSuper && terminal.GroupID != current.GroupID {
		return domain.Terminal{}, superModeRefusal(current, "移动客户机到其他分组")
	}
	// 留空本意是「不指定」：同组保持原地址，换组时原地址多半不在新组范围内，改为在新组里分配。
	if strings.TrimSpace(req.IP) == "" && terminal.GroupID != current.GroupID {
		ip, err := s.allocateIP(ctx, terminal.GroupID)
		if err != nil {
			return domain.Terminal{}, err
		}
		terminal.IP = ip
	}
	if err := s.validateTerminal(ctx, terminal, id); err != nil {
		return domain.Terminal{}, err
	}
	if err := refuseAddressChangeWhileRunning(current, terminal.IP, terminal.GroupID); err != nil {
		return domain.Terminal{}, err
	}
	if terminal.IsSuper {
		if err := EnsureSuperAvailable(ctx, s.Store, terminal); err != nil {
			return domain.Terminal{}, err
		}
	}
	if err := s.prepareSuperSwitch(ctx, current, terminal.IsSuper); err != nil {
		return domain.Terminal{}, err
	}
	// 经编辑取消超管时也要清掉暂存驱动包，与 DisableSuper 一致；否则普通机上残留的 PendingBundleID 会显示矛盾的「待固化」。
	if current.IsSuper && !terminal.IsSuper {
		terminal.PendingBundleID = nil
		terminal.PendingBundleAt = nil
	}
	if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
		return domain.Terminal{}, err
	}
	if current.IP != "" && current.IP != terminal.IP {
		releaseLease(ctx, s.DHCP, s.Logger, current.IP, current.MAC)
	}
	if err := syncDHCP(ctx, s.Store, s.DHCP, s.Logger); err != nil {
		return domain.Terminal{}, err
	}
	return terminal, nil
}

// releaseLease 通知 dnsmasq 某地址已易主，尽力而为：dhcp_release 不论是否送达都返回 0，
// 没装 dnsmasq-utils 的节点根本没有该命令。清理失败只会让地址暂时显示不对，不能因此让删除或移动失败。
func releaseLease(ctx context.Context, syncer DHCPSyncer, logger *slog.Logger, ip, mac string) {
	if syncer == nil || strings.TrimSpace(ip) == "" || strings.TrimSpace(mac) == "" {
		return
	}
	if err := syncer.ReleaseLease(ctx, ip, mac); err != nil && logger != nil {
		logger.Warn("could not release the DHCP lease on an address that changed hands; the machine that takes it next may be served a different address until the lease expires",
			"ip", ip, "mac", mac, "error", err)
	}
}

// refuseAddressChangeWhileRunning 拒绝修改运行中机器的地址或分组。改的当下不会出错（机器持有租约），
// 但续租时 dnsmasq 拒绝旧地址，IP 一变运行中系统的 iSCSI 会话就断，故障数小时后才出现且像存储问题。
// 「运行中」指有 iSCSI 会话，机器真正关机后由对账解除。其它字段仍可修改。
func refuseAddressChangeWhileRunning(current domain.Terminal, newIP, newGroupID string) error {
	if current.State != domain.TerminalStateOnline {
		return nil
	}
	if newIP == current.IP && newGroupID == current.GroupID {
		return nil
	}
	return TerminalRunningAddressChange{Name: current.Name, MAC: current.MAC}
}

// TerminalRunningAddressChange 是 refuseAddressChangeWhileRunning 给操作者的拒绝信息。
type TerminalRunningAddressChange struct {
	Name string
	MAC  string
}

func (e TerminalRunningAddressChange) Error() string {
	who := e.Name
	if who == "" {
		who = e.MAC
	}
	return fmt.Sprintf("%s 正在运行，不能改它的 IP 或分组：地址会在租约续期时变，届时这台机器会掉线重启。请先关机再改", who)
}

func (e TerminalRunningAddressChange) Is(target error) bool { return target == errs.ErrConflict }

// TerminalRunningSuperChange 拒绝在运行中的机器上切换超管。超管标志下次开机才生效，运行中切换会让
// 控制台与机器实际不符：新设超管的仍在临时盘上、下次开机即丢；新取消的仍在写超管盘，
// 而按标志判断的「每配置一台超管机」会放第二台启动。
type TerminalRunningSuperChange struct {
	Name   string
	MAC    string
	Enable bool
}

func (e TerminalRunningSuperChange) Error() string {
	who := e.Name
	if who == "" {
		who = e.MAC
	}
	if e.Enable {
		return fmt.Sprintf("%s 运行中，请先关机再设为超管", who)
	}
	return fmt.Sprintf("%s 运行中，请先关机再取消超管", who)
}

func (e TerminalRunningSuperChange) Is(target error) bool { return target == errs.ErrConflict }

func (s TerminalService) allocateIP(ctx context.Context, groupID string) (string, error) {
	group, err := s.Store.Groups().Get(ctx, groupID)
	if err != nil {
		return "", err
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return "", err
	}
	occupied := map[uint32]bool{}
	for _, terminal := range terminals {
		ip, err := parseTerminalIP(terminal.IP)
		if err != nil {
			return "", err
		}
		occupied[ipv4Uint32(ip)] = true
	}
	reserveClusterAddrs(ctx, s.Store, occupied)
	return allocateIPFromGroup(group, occupied)
}

func (s TerminalService) Delete(ctx context.Context, id string) error {
	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return err
	}
	// 立即回收克隆而不等离线回收器：宽限期是因为会话断开不能证明关机，而删除终端是明确意图；
	// 克隆不删会挡住配置合并和镜像删除。克隆删不掉（机器仍在运行、zvol 忙）时删除失败，而不是静默留下孤儿。
	if s.Storage != nil {
		host := s.hostFor(ctx, terminal)
		if err := host.CleanupClientClones(ctx, terminal.MAC, nil); err != nil {
			return err
		}
		// 超管机会话是持久的、不属于客户机克隆，也要不保存地丢弃（系统盘与数据盘），否则它会一直钉住来源配置。
		if terminal.IsSuper {
			if _, err := host.SuperStop(ctx, storage.SuperStopReq{MAC: terminal.MAC}); err != nil {
				return err
			}
		}
	}
	// client_clones 行通过 ON DELETE CASCADE 随终端删除。
	if err := s.Store.Terminals().Delete(ctx, id); err != nil {
		return err
	}
	releaseLease(ctx, s.DHCP, s.Logger, terminal.IP, terminal.MAC)
	return syncDHCP(ctx, s.Store, s.DHCP, s.Logger)
}

func (s TerminalService) Heartbeat(ctx context.Context, mac string) (domain.Terminal, error) {
	normalized, err := normalizeTerminalMAC(mac)
	if err != nil {
		return domain.Terminal{}, err
	}
	terminal, err := s.Store.Terminals().GetByMAC(ctx, normalized)
	if err != nil {
		return domain.Terminal{}, err
	}
	now := s.now()
	if terminal.State != domain.TerminalStateOnline {
		terminal.OnlineSince = &now
	}
	terminal.LastHeartbeatAt = &now
	terminal.OfflineAt = nil
	terminal.State = domain.TerminalStateOnline
	if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
		return domain.Terminal{}, err
	}
	return terminal, nil
}

func (s TerminalService) SweepOffline(ctx context.Context, timeout time.Duration) (int, error) {
	now := s.now()
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, terminal := range terminals {
		if terminal.State != domain.TerminalStateOnline {
			continue
		}
		if terminal.LastHeartbeatAt != nil && now.Sub(*terminal.LastHeartbeatAt) <= timeout {
			continue
		}
		terminal.State = domain.TerminalStateOffline
		terminal.OfflineAt = &now
		if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

func (s TerminalService) RunOfflineSweeper(ctx context.Context, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.SweepOffline(ctx, timeout); err != nil && s.Logger != nil {
				s.Logger.Error("terminal offline sweep failed", "error", err)
			}
		}
	}
}

// offlineConfirmations 是标记离线前须连续无会话的扫描次数。会话拆除时 LIO 的 dynamic_sessions
// 会短暂在空与非空间抖动，所以单次空读不可信；上线仍立即生效。
const offlineConfirmations = 2

// ReconcileOnline 以 iSCSI 会话为准更新终端在线状态：客户机持有存储节点上的活动会话即在线，
// 不需要客户机内心跳代理（无盘客户机开机后必然保持系统盘会话）。返回状态变化的终端数。
// offlineMisses 跨轮记录每 MAC 连续空扫次数（归 RunOnlineReconciler 所有），不能为 nil。
// 会话查询出错时返回错误，调用方跳过本轮，不能把所有人标离线。
func (s TerminalService) ReconcileOnline(ctx context.Context, offlineMisses map[string]int) (int, error) {
	macs, err := s.activeMACsAllHosts(ctx)
	if err != nil {
		return 0, err
	}
	online := make(map[string]struct{}, len(macs))
	for _, mac := range macs {
		online[storage.NormalizeMAC(mac)] = struct{}{}
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	changed := 0
	for _, terminal := range terminals {
		key := storage.NormalizeMAC(terminal.MAC)
		_, hasSession := online[key]
		if hasSession {
			delete(offlineMisses, key) // 有活动会话即取消待离线计数
			if terminal.State != domain.TerminalStateOnline {
				terminal.OnlineSince = &now
				terminal.LastHeartbeatAt = &now
				terminal.OfflineAt = nil
				terminal.State = domain.TerminalStateOnline
				if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
					return changed, err
				}
				changed++
			}
			continue
		}
		if terminal.State != domain.TerminalStateOnline {
			delete(offlineMisses, key) // 已离线，无需确认
			continue
		}
		// 在线但无会话：对抖动的空读去抖。
		offlineMisses[key]++
		if offlineMisses[key] < offlineConfirmations {
			continue
		}
		delete(offlineMisses, key)
		terminal.OfflineAt = &now
		terminal.State = domain.TerminalStateOffline
		if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// RunOnlineReconciler 定期按 iSCSI 会话对账在线状态。存储节点报不出会话（如 LIO/targetcli 不可用）时跳过本轮，状态不动。
func (s TerminalService) RunOnlineReconciler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	offlineMisses := map[string]int{} // 每 MAC 连续空扫计数
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ReconcileOnline(ctx, offlineMisses); err != nil && s.Logger != nil {
				s.Logger.Error("terminal online reconcile skipped", "error", err)
			}
		}
	}
}

func (s TerminalService) Move(ctx context.Context, req MoveTerminalsRequest) (MoveTerminalsResult, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	targetID := strings.TrimSpace(req.GroupID)
	ids := uniqueStrings(req.TerminalIDs)
	if targetID == "" || len(ids) == 0 {
		return MoveTerminalsResult{}, ErrTerminalMoveRequired
	}
	target, err := s.Store.Groups().Get(ctx, targetID)
	if err != nil {
		return MoveTerminalsResult{}, err
	}
	allTerminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return MoveTerminalsResult{}, err
	}
	terminalsByID := map[string]domain.Terminal{}
	moveSet := map[string]bool{}
	for _, id := range ids {
		moveSet[id] = true
	}
	// 本就在目标组的机器（无论是否被选中）都占名额；下面只给新移入的加一。
	targetCount := 0
	for _, terminal := range allTerminals {
		terminalsByID[terminal.ID] = terminal
		if terminal.GroupID == targetID {
			targetCount++
		}
	}
	for _, id := range ids {
		if terminal, ok := terminalsByID[id]; ok && terminal.IsSuper {
			return MoveTerminalsResult{}, superModeRefusal(terminal, "移动客户机到其他分组")
		}
	}
	// 每个分组有自己的 IP 段，来自别组的终端地址不属于目标组，所以从目标组段内重新分配；
	// 已在目标段内的保留原地址，来回移动不能给机房重新编号。保留的地址要在分配前就标为已占，
	// 否则排在前面的新机器会分到它，整批撞唯一约束。
	occupied := map[uint32]bool{}
	for _, terminal := range allTerminals {
		if moveSet[terminal.ID] && ensureTerminalIPInGroup(terminal.IP, target) != nil {
			continue // 旧地址随移动释放
		}
		ip, err := parseTerminalIP(terminal.IP)
		if err != nil {
			return MoveTerminalsResult{}, err
		}
		occupied[ipv4Uint32(ip)] = true
	}
	reserveClusterAddrs(ctx, s.Store, occupied)
	moved := make([]domain.Terminal, 0, len(ids))
	for _, id := range ids {
		terminal, ok := terminalsByID[id]
		if !ok {
			return MoveTerminalsResult{}, fmt.Errorf("terminal %s: %w", id, errs.ErrNotFound)
		}
		if err := ensureTerminalIPInGroup(terminal.IP, target); err != nil {
			ip, allocErr := allocateIPFromGroup(target, occupied)
			if allocErr != nil {
				return MoveTerminalsResult{}, allocErr
			}
			terminal.IP = ip
		}
		parsed, err := parseTerminalIP(terminal.IP)
		if err != nil {
			return MoveTerminalsResult{}, err
		}
		occupied[ipv4Uint32(parsed)] = true
		// 换组会换地址，运行中的机器不能换，理由同 Update；批量里混进一台开着的，事后难以分辨是哪台。
		if err := refuseAddressChangeWhileRunning(terminalsByID[id], terminal.IP, targetID); err != nil {
			return MoveTerminalsResult{}, err
		}
		if terminal.GroupID != targetID {
			targetCount++
		}
		terminal.GroupID = targetID
		moved = append(moved, terminal)
	}
	if targetCount > target.ClientMax {
		return MoveTerminalsResult{}, ErrTerminalGroupFull
	}

	if s.Storage != nil {
		for _, terminal := range moved {
			if terminalsByID[terminal.ID].GroupID == targetID {
				continue
			}
			if err := s.hostFor(ctx, terminalsByID[terminal.ID]).CleanupClientClones(ctx, terminal.MAC, nil); err != nil {
				return MoveTerminalsResult{}, err
			}
		}
	}
	clones, err := s.Store.ClientClones().List(ctx)
	if err != nil {
		return MoveTerminalsResult{}, err
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		for _, terminal := range moved {
			if err := tx.Terminals().Update(ctx, terminal); err != nil {
				return err
			}
			for _, clone := range clonesOfMAC(clones, terminal.MAC) {
				if err := tx.ClientClones().Delete(ctx, clone.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return MoveTerminalsResult{}, err
	}
	// 租约等事务提交后再放：事务失败时机器仍用旧地址，不能先把它的租约收走。
	for _, terminal := range moved {
		if before := terminalsByID[terminal.ID]; before.IP != "" && before.IP != terminal.IP {
			releaseLease(ctx, s.DHCP, s.Logger, before.IP, before.MAC)
		}
	}
	if err := syncDHCP(ctx, s.Store, s.DHCP, s.Logger); err != nil {
		return MoveTerminalsResult{}, err
	}
	return MoveTerminalsResult{Moved: len(moved), Items: moved}, nil
}

// prepareSuperSwitch 是切换超管在改标志之外要做的事，两个按钮和编辑表单共用。
// 运行中拒绝：记录的状态比刚开机滞后一轮统计，以实时会话为准；删正在用的超管盘会让机器蓝屏。
// 真正切换时不保存地丢弃超管盘：取消超管本就如此；设为超管时残留的盘只能是先前丢弃的遗留。
// 在本节点丢弃：超管机位于写入者上，前写入者降级时已丢自己的（quiesceStandby），切换也在写入者上执行。
func (s TerminalService) prepareSuperSwitch(ctx context.Context, current domain.Terminal, enable bool) error {
	if current.IsSuper == enable {
		return nil
	}
	refusal := TerminalRunningSuperChange{Name: current.Name, MAC: current.MAC, Enable: enable}
	if current.State == domain.TerminalStateOnline {
		return refusal
	}
	if s.Storage == nil {
		return nil
	}
	live, err := s.activeMACsAllHosts(ctx)
	if err != nil {
		return err
	}
	mac := storage.NormalizeMAC(current.MAC)
	for _, m := range live {
		if storage.NormalizeMAC(m) == mac {
			return refusal
		}
	}
	_, err = s.Storage.SuperStop(ctx, storage.SuperStopReq{MAC: current.MAC})
	return err
}

func (s TerminalService) EnableSuper(ctx context.Context, id string) (domain.Terminal, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return domain.Terminal{}, err
	}
	if err := EnsureSuperAvailable(ctx, s.Store, terminal); err != nil {
		return domain.Terminal{}, err
	}
	if err := s.prepareSuperSwitch(ctx, terminal, true); err != nil {
		return domain.Terminal{}, err
	}
	terminal.IsSuper = true
	if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
		return domain.Terminal{}, err
	}
	return terminal, nil
}

func (s TerminalService) DisableSuper(ctx context.Context, id string) (domain.Terminal, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return domain.Terminal{}, err
	}
	if err := s.prepareSuperSwitch(ctx, terminal, false); err != nil {
		return domain.Terminal{}, err
	}
	terminal.IsSuper = false
	terminal.PendingBundleID = nil
	terminal.PendingBundleAt = nil
	if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
		return domain.Terminal{}, err
	}
	return terminal, nil
}

func (s TerminalService) StopSuper(ctx context.Context, id string, req SuperStopRequest) (SuperStopResult, error) {
	terminal, err := s.Store.Terminals().Get(ctx, id)
	if err != nil {
		return SuperStopResult{}, err
	}
	if !terminal.IsSuper {
		return SuperStopResult{}, ErrTerminalSuperRequired
	}
	if terminal.State != domain.TerminalStateOffline && !s.Async {
		return SuperStopResult{}, ErrTerminalOfflineNeeded
	}
	group, err := s.Store.Groups().Get(ctx, terminal.GroupID)
	if err != nil {
		return SuperStopResult{}, err
	}
	cfg, err := s.Store.Configs().Get(ctx, group.SystemConfigID)
	if err != nil {
		return SuperStopResult{}, err
	}
	images, err := s.superStopImages(ctx, group, cfg, req.DataDisks)
	if err != nil {
		return SuperStopResult{}, err
	}
	// 在检查名字之前就占住镜像，并一直持有到等关机结束：期间在这些镜像上启动的任何操作都会与保存冲突。
	claim, err := claimImages(ctx, s.Store, domain.TaskTypeSuperStop, images...)
	if err != nil {
		return SuperStopResult{}, err
	}
	defer claim.Drop()
	// 名字为空表示丢弃超管机的修改；否则会成为快照，须遵守与手工建还原点相同的命名规则。
	reductionName, displayName := "", ""
	if strings.TrimSpace(req.ReductionName) != "" {
		reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
		if err != nil {
			return SuperStopResult{}, err
		}
		if displayName, reductionName, err = validateReductionName(req.ReductionName, reductions); err != nil {
			return SuperStopResult{}, err
		}
	}
	dataDisks, err := s.resolveSuperStopDisks(ctx, group, req.DataDisks)
	if err != nil {
		return SuperStopResult{}, err
	}
	storageReq := storage.SuperStopReq{MAC: terminal.MAC, ConfigID: cfg.ID, ReductionName: reductionName}
	for _, disk := range dataDisks {
		storageReq.DataDisks = append(storageReq.DataDisks, storage.SuperStopDisk{LUN: disk.lun, ConfigID: disk.config.ID, ReductionName: disk.name})
	}
	var result SuperStopResult
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeSuperStop, terminal.MAC, func(ctx context.Context, task domain.Task) error {
		var err error
		result, err = s.executeStopSuper(ctx, task, terminal, cfg, storageReq, reductionName, displayName, dataDisks)
		return err
	})
	if err != nil {
		return SuperStopResult{}, err
	}
	if s.Async {
		return SuperStopResult{TaskID: task.ID}, nil
	}
	result.TaskID = task.ID
	return result, nil
}

// superStopImages 列出保存涉及的镜像：系统配置的镜像和要保存的数据盘的镜像。未知盘交给 resolveSuperStopDisks 拒绝。
func (s TerminalService) superStopImages(ctx context.Context, group domain.Group, cfg domain.Config, requests []SuperStopDiskRequest) ([]string, error) {
	images := []string{cfg.ImageID}
	if len(requests) == 0 {
		return images, nil
	}
	disks, err := GroupDataDisks(ctx, s.Store, group.ID)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, req := range requests {
		if strings.TrimSpace(req.ReductionName) != "" {
			wanted[req.DiskID] = true
		}
	}
	for _, disk := range disks {
		if !wanted[disk.ID] {
			continue
		}
		dataCfg, err := s.Store.Configs().Get(ctx, disk.ConfigID)
		if err != nil {
			return nil, err
		}
		images = append(images, dataCfg.ImageID)
	}
	return images, nil
}

// resolveSuperStopDisks 把操作者逐盘的选择解析为分组盘及其 LUN，名字按该数据配置自身的还原点校验，与手工建还原点一致。
func (s TerminalService) resolveSuperStopDisks(ctx context.Context, group domain.Group, requests []SuperStopDiskRequest) ([]superStopDisk, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	disks, err := GroupDataDisks(ctx, s.Store, group.ID)
	if err != nil {
		return nil, err
	}
	lunByDisk := make(map[string]int, len(disks))
	diskByID := make(map[string]domain.GroupDisk, len(disks))
	for i, disk := range disks {
		lunByDisk[disk.ID] = DataDiskLUN(i)
		diskByID[disk.ID] = disk
	}
	seen := make(map[string]bool, len(requests))
	resolved := make([]superStopDisk, 0, len(requests))
	for _, req := range requests {
		if strings.TrimSpace(req.ReductionName) == "" {
			continue
		}
		disk, ok := diskByID[req.DiskID]
		if !ok {
			return nil, errs.Invalid("分组 " + group.Name + " 没有数据盘 " + req.DiskID + "，请刷新后重试")
		}
		if seen[disk.ID] {
			return nil, errs.Invalid("数据盘 " + disk.MountTarget + " 重复")
		}
		seen[disk.ID] = true
		cfg, err := s.Store.Configs().Get(ctx, disk.ConfigID)
		if err != nil {
			return nil, err
		}
		reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
		if err != nil {
			return nil, err
		}
		displayName, name, err := validateReductionName(req.ReductionName, reductions)
		if err != nil {
			return nil, fmt.Errorf("数据盘 %s：%w", disk.MountTarget, err)
		}
		resolved = append(resolved, superStopDisk{disk: disk, config: cfg, lun: lunByDisk[disk.ID], name: name, displayName: displayName})
	}
	return resolved, nil
}

func (s TerminalService) executeStopSuper(ctx context.Context, task domain.Task, terminal domain.Terminal, cfg domain.Config, storageReq storage.SuperStopReq, reductionName, displayName string, dataDisks []superStopDisk) (SuperStopResult, error) {
	if err := s.waitTerminalOffline(ctx, terminal.ID, 2*time.Minute); err != nil {
		return SuperStopResult{}, err
	}
	// 从换盘到写还原点行，对复制而言是一次变更；不包括上面的等待，那可能要几分钟。
	defer storage.ChangeCatalogue()()
	saved, err := s.hostFor(ctx, terminal).SuperStop(ctx, storageReq)
	if err != nil {
		var missing storage.SuperSessionMissing
		if errors.As(err, &missing) {
			return SuperStopResult{}, superSessionMissingError(terminal, missing.LUN, dataDisks)
		}
		return SuperStopResult{}, err
	}
	// 存还原点不改超管状态：机器仍是超管机，开机可接着装；结束编辑走「取消超管」，那里会连盘一起丢弃。
	// 因此在取消超管前，它仍占着这些配置：别人不能在同配置开超管，镜像页也不能直接给这些配置建还原点。
	terminal.PendingBundleID = nil // 驱动包已随本次保存固化
	terminal.PendingBundleAt = nil
	var result SuperStopResult
	if reductionName != "" {
		reduction := saved.System
		reduction.ConfigID = cfg.ID
		reduction.Name = reductionName
		reduction.DisplayName = displayName
		result.Reduction = &reduction
	}
	// 每块保存的数据盘成为其所属配置的还原点，按 LUN 与盘对应，以存储层报告的实际保存结果为准。
	savedByLUN := make(map[int]domain.Reduction, len(saved.DataDisks))
	for _, item := range saved.DataDisks {
		savedByLUN[item.LUN] = item.Reduction
	}
	type dataSave struct {
		config    domain.Config
		reduction domain.Reduction
	}
	dataSaves := make([]dataSave, 0, len(dataDisks))
	for _, disk := range dataDisks {
		reduction, ok := savedByLUN[disk.lun]
		if !ok {
			return SuperStopResult{}, fmt.Errorf("数据盘 %s 未保存：存储层没有返回它的还原点", disk.disk.MountTarget)
		}
		reduction.ConfigID = disk.config.ID
		reduction.Name = disk.name
		reduction.DisplayName = disk.displayName
		dataSaves = append(dataSaves, dataSave{config: disk.config, reduction: reduction})
		result.DataReductions = append(result.DataReductions, reduction)
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if result.Reduction != nil {
			if err := tx.Reductions().Create(ctx, *result.Reduction); err != nil {
				return err
			}
			// 保存的点成为当前点，并像「设为当前」一样下发到跟随该配置的分组。
			if err := setConfigCurrent(ctx, tx, cfg.ID, result.Reduction.ID); err != nil {
				return err
			}
		}
		for _, item := range dataSaves {
			if err := tx.Reductions().Create(ctx, item.reduction); err != nil {
				return err
			}
			if err := setConfigCurrent(ctx, tx, item.config.ID, item.reduction.ID); err != nil {
				return err
			}
		}
		if err := tx.Terminals().Update(ctx, terminal); err != nil {
			return err
		}
		taskResult := terminal.MAC
		if result.Reduction != nil {
			taskResult = result.Reduction.ID
		} else if len(result.DataReductions) > 0 {
			taskResult = result.DataReductions[0].ID
		}
		return s.runner().Finish(ctx, tx, task, taskResult)
	}); err != nil {
		return SuperStopResult{}, err
	}
	if (reductionName != "" || len(dataSaves) > 0) && s.BackupTrigger != nil {
		go func() {
			if err := s.BackupTrigger(context.Background()); err != nil && s.Logger != nil {
				s.Logger.Error("backup trigger failed", "error", err)
			}
		}()
	}
	return result, nil
}

// superSessionMissingError 说明勾选的哪块盘机器从未持有（盘分给分组后它还没以超管身份开过机）以及该怎么做。
func superSessionMissingError(terminal domain.Terminal, lun int, dataDisks []superStopDisk) error {
	disk := "系统盘"
	for _, d := range dataDisks {
		if d.lun == lun {
			disk = "数据盘 " + d.disk.MountTarget
		}
	}
	return errs.Invalid(fmt.Sprintf("超管机 %s 没有%s的会话可保存：它还没有带着这块盘以超管机开机过。去掉这块盘的勾选，或先开机再停机", terminal.MAC, disk))
}

func (s TerminalService) waitTerminalOffline(ctx context.Context, id string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for {
		terminal, err := s.Store.Terminals().Get(ctx, id)
		if err != nil {
			return err
		}
		if terminal.State == domain.TerminalStateOffline {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrTerminalOfflineNeeded
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s TerminalService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now, Async: s.Async}
}

func (s TerminalService) validateTerminal(ctx context.Context, terminal domain.Terminal, currentID string) error {
	group, err := s.Store.Groups().Get(ctx, terminal.GroupID)
	if err != nil {
		return err
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return err
	}
	countInGroup := 0
	for _, item := range terminals {
		if item.ID == currentID {
			continue
		}
		if item.MAC == terminal.MAC {
			return ErrTerminalMACExists
		}
		if item.IP == terminal.IP {
			return ErrTerminalIPExists
		}
		if item.GroupID == terminal.GroupID {
			countInGroup++
		}
	}
	if countInGroup >= group.ClientMax {
		return ErrTerminalGroupFull
	}
	if err := ensureTerminalIPInGroup(terminal.IP, group); err != nil {
		return err
	}
	return nil
}

func terminalFromRequest(current domain.Terminal, req TerminalRequest) (domain.Terminal, error) {
	mac, err := normalizeTerminalMAC(req.MAC)
	if err != nil {
		return domain.Terminal{}, err
	}
	ip := strings.TrimSpace(req.IP)
	if ip == "" {
		ip = current.IP
	}
	if ip != "" {
		if _, err := parseTerminalIP(ip); err != nil {
			return domain.Terminal{}, err
		}
	}
	if ip == "" && current.ID != "" {
		return domain.Terminal{}, ErrTerminalInvalidIP
	}
	groupID := strings.TrimSpace(req.GroupID)
	if groupID == "" {
		return domain.Terminal{}, ErrTerminalInvalidIP
	}
	state, err := normalizeTerminalState(req.State, current.State)
	if err != nil {
		return domain.Terminal{}, err
	}
	current.MAC = mac
	current.IP = ip
	current.GroupID = groupID
	current.IsSuper = req.IsSuper
	current.State = state
	current.Name = strings.TrimSpace(req.Name)
	return current, nil
}

func normalizeTerminalMAC(value string) (string, error) {
	mac := storage.NormalizeMAC(value)
	if len(mac) != 12 {
		return "", ErrTerminalInvalidMAC
	}
	for _, r := range mac {
		if (r < '0' || r > '9') && (r < 'A' || r > 'F') {
			return "", ErrTerminalInvalidMAC
		}
	}
	return mac, nil
}

func parseTerminalIP(value string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !ip.Is4() {
		return netip.Addr{}, ErrTerminalInvalidIP
	}
	return ip, nil
}

func ensureTerminalIPInGroup(value string, group domain.Group) error {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return err
	}
	ip, err := parseTerminalIP(value)
	if err != nil {
		return err
	}
	if !pool.contains(ipv4Uint32(ip)) {
		return terminalIPOutsideGroup(fmt.Sprintf("%s 不在分组 %s 的范围 %s–%s 内；留空自动分配", ip, group.Name, ipv4String(pool.start), ipv4String(pool.end)))
	}
	return nil
}

// terminalIPOutsideGroup 是带上地址范围的 ErrTerminalInvalidIP：判断通用错误的调用方仍能匹配，操作者也能看到地址没落在哪个窗口。
type terminalIPOutsideGroup string

func (e terminalIPOutsideGroup) Error() string { return string(e) }
func (e terminalIPOutsideGroup) Is(target error) bool {
	return target == ErrTerminalInvalidIP || target == errs.ErrInvalid
}

func ipv4String(value uint32) string {
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}).String()
}

// normalizeTerminalState：从未连接过的机器视为离线，控制台只显示在线和离线；仍接受 "unknown"（旧导出文件）。
func normalizeTerminalState(value, fallback domain.TerminalState) (domain.TerminalState, error) {
	if value == "" {
		if fallback == "" {
			return domain.TerminalStateOffline, nil
		}
		return fallback, nil
	}
	switch value {
	case domain.TerminalStateUnknown:
		return domain.TerminalStateOffline, nil
	case domain.TerminalStateOnline, domain.TerminalStateOffline:
		return value, nil
	default:
		return "", ErrTerminalInvalidState
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s TerminalService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
