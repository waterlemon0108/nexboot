package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 角色由 keepalived 决定，靠标记文件跨重启保存：不能放 DB（激活时 DB 会被对端副本替换），
// 也不能放内存（切换本身就是重启）。
// 激活与降级都是「写标记再重启」，复用启动路径里已幂等的角色建立逻辑，避免两套逻辑漂移。

// RoleState 是标记文件的内容。Epoch 是栅栏计数，只增不减；epoch 落后的节点不得写入。
type RoleState struct {
	Role  string `json:"role"` // "active" | "standby"
	Epoch int64  `json:"epoch"`
	// SeenEpoch 是本节点在集群里见过的最高 epoch（含自己从未持有过的）。
	// 放在这里而非目录库：备机会整库换成主机副本，副本里只有主机自己的 epoch。
	// 缺了它，对端死后起来的节点会以自己陈旧的值 +1 激活，两台持有同一 epoch，
	// 「对端仍在服务」栅栏（peer.Epoch >= epoch+1）从此失效。
	SeenEpoch int64 `json:"seen_epoch,omitempty"`
	// DemotedAt 是启动时核对把「标记为 active」的本节点降回 standby 的时间。
	// 用来打断分区时的循环：孤立侧持有 VIP 每十秒激活一次、重启后又被降级。
	// 必须落在角色文件里，因为激活就是重启，进程内存留不到下一次尝试。
	DemotedAt *time.Time `json:"demoted_at,omitempty"`
}

// demoteBackoff 是启动时刚被降级的节点再次激活前的等待时间：既不让分区变成重启循环，
// 又能在冷却后一分钟内接管真正死掉的对端。只在没有任何对端应答时生效。
const demoteBackoff = 90 * time.Second

// peerProbeTimeout 限定一次「你还在服务吗」的询问。分区时报文被丢弃而非拒绝，
// TCP 连接会挂到内核放弃（几分钟），激活也跟着挂住；实测曾挂住十五分钟无人写入。
const peerProbeTimeout = 5 * time.Second

// ReadRoleState 读取标记；没有标记（从未切换、角色来自配置）时 ok 为 false。
func ReadRoleState(path string) (RoleState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RoleState{}, false
	}
	var rs RoleState
	if err := json.Unmarshal(b, &rs); err != nil || rs.Role == "" {
		return RoleState{}, false
	}
	return rs, true
}

// WriteRoleState 原子替换标记：切换中途崩溃只会留下旧角色，不会留半个文件。
func WriteRoleState(path string, rs RoleState) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PeerStatus 是 /internal/ha/status 的应答，足以让激活守卫区分对端已死还是分区。
type PeerStatus struct {
	NodeID string `json:"node_id"`
	Role   string `json:"role"`
	Epoch  int64  `json:"epoch"`
	// HoldsVIP 是判断节点是否真在服务的唯一依据：角色文件可能写着 active 而进程卡死、
	// 刚恢复或正在降级，VIP 不会骗人。
	HoldsVIP bool `json:"holds_vip,omitempty"`
	// VRRPPeers 是本节点当前的 keepalived 单播 peer 列表。加入节点要等写入者的列表
	// 包含自己再启动 VRRP，否则收不到通告会自选为 master。
	VRRPPeers []string `json:"vrrp_peers,omitempty"`
	// Mark 是本节点完整持有的最新复制标记。接任前要比较：对端更新，说明接任会丢掉它的改动。
	Mark string `json:"mark,omitempty"`
	// Healthy 是本节点自身的健康检查结果。健康的主机可以收回 VIP 继续服务，不应被接任。
	Healthy bool `json:"healthy,omitempty"`
}

// PeerStatusClient 读取对端角色。守卫把出错视为对端已死：keepalived 只在它自己的检查也失败时才调用激活。
type PeerStatusClient interface {
	Status(ctx context.Context) (PeerStatus, error)
}

// ErrNoDBCopy 表示备机从未收到对端目录库：此时激活会在满是镜像的池旁边启动一个空产品。
// 强制激活解决不了这个问题，运维应先让复制走通，或把仍有数据的那台重新拉起当主机。
var ErrNoDBCopy = errors.New("备机尚未收到主机的数据库副本，激活会丢失全部分组与客户机配置。" +
	"请先让复制走通（确认对端在服务、本机能拉到目录），或把仍有数据的那台重新作为主机启动")

// Controller 承载一次角色切换，从请求到重启。
type Controller struct {
	// NodeID 是本节点请对端交出写入者角色时报的名字。
	NodeID   string
	RoleFile string
	// Now 是冷却计时用的时钟，nil 用真实时钟。
	Now func() time.Time
	// DefaultRole 是从未切换过（尚无标记文件）的节点的配置初始角色，空表示 active（单机默认）。
	DefaultRole string
	Gate        *Gate
	Peer        PeerStatusClient
	// ClusterPeers 列出节点名册中其他每个节点的状态客户端，三节点及以上时使用。
	ClusterPeers func(ctx context.Context) []PeerStatusClient
	// PrepareActiveDB 把复制来的 DB 副本放到活动 DB 路径；从未收到副本时返回 ErrNoDBCopy。
	// 只有备机激活时运行，主机的活动 DB 本身就是准的。
	PrepareActiveDB func(ctx context.Context) error
	// SyncBeforeSwitch 为计划切换排空：标记一轮并等备机拉走，保证切换不丢数据。
	SyncBeforeSwitch func(ctx context.Context) error
	// StepDownFile 降级时打标、激活时清除；健康检查限时读取它，让 keepalived 在降级重启期间放开 VIP。
	StepDownFile string
	// RecordEpoch 在 epoch 变化时立即写入名册，不等下次心跳。否则一个心跳周期内发生的接任
	// 会读到陈旧上限，铸出比被替换主机更低的 epoch，栅栏反向，旧主机会以 active 回来。
	RecordEpoch func(ctx context.Context, epoch int64)
	// KnownEpoch 返回名册里见过的最高 epoch（各节点随心跳上报），作为接任的下限。
	// 主机直接死掉时已无人可问，只当过备机的节点否则会从自己的小数字重新计数，
	// 旧主机回来时 epoch 更高，两台同时 active。nil 表示没有名册。
	KnownEpoch func(ctx context.Context) int64
	// HoldsVIP 回答「VIP 此刻是否在本机」，上报给对端供其激活守卫使用。
	// nil 视为不知道并报 false：报一个看不到的 VIP 会挡住正当的接任。
	HoldsVIP func() bool
	// SwitchFile 在进程为切换角色而退出前打标。重启期间 /healthz 有几秒不应答，
	// keepalived 探测会当成节点已死而放开 VIP，触发降级通知又导致再次重启。
	// 标记让探测知道「马上回来」；它限时有效，真回不来的节点照样失去 VIP。
	SwitchFile string
	// CatalogueMark 返回本节点完整持有的最新复制标记；没有或接收未完成时返回 ""。nil 表示不知道，不做比较。
	CatalogueMark func(ctx context.Context) string
	// Healthy 返回本节点自身健康检查结果，供上报给对端。
	Healthy func(ctx context.Context) bool
	// CatalogueIncomplete 列出复制来的 DB 副本里记录了、本节点池里却没有的对象（nil 或空表示完整）。
	CatalogueIncomplete func(ctx context.Context) []string
	// CatchUp 直接从对端自己的地址（不是 VIP，那时 VIP 正在本机）拉取目录，
	// 直到本节点完整持有 target。见 takeover.go。
	CatchUp func(ctx context.Context, from PeerStatusClient, target string) error
	// StampFinalRound 无视写闸门打一轮复制标记，并在 hold 内暂停周期轮次；ResumeRounds 恢复。
	// 是交接中写入者那一半（handover.go）。
	StampFinalRound func(ctx context.Context, hold time.Duration) (string, error)
	ResumeRounds    func()
	// HandoverHold 覆盖已准备的交接保持关闭写入的时长，零值用默认。
	HandoverHold time.Duration
	// YieldFile 记录本节点已把 VIP 让给更合适的节点。用文件，因为 VIP 回来可能隔着一次重启。
	YieldFile string
	// ReportFile 记录接任时遗留下来的问题；本节点成为写入者后把它读回作为告警。
	ReportFile string
	// Exit 结束进程，由 systemd 以新角色重启。
	Exit   func(code int)
	Logger *slog.Logger
	// strandedRounds 连续观测到「active 但 VIP 在别处」的次数，连续两次才降级。
	strandedRounds int
	// activating 保证同一时间只有一个接任：keepalived 通知和自愈循环都会进 Activate，追平可能要几分钟。
	activating sync.Mutex
	stateMu    sync.Mutex
	takeover   TakeoverState
	// handoverTimer 在没人完成交接时重新打开写入者。
	handoverTimer *time.Timer
}

// State 返回当前角色状态；从未切换的节点读作 epoch 0 的 active（单机默认）。
func (c *Controller) State() RoleState {
	if rs, ok := ReadRoleState(c.RoleFile); ok {
		return rs
	}
	if c.DefaultRole != "" {
		return RoleState{Role: c.DefaultRole, Epoch: 0}
	}
	return RoleState{Role: "active", Epoch: 0}
}

// Activate 让本节点成为写入者：防住仍活着的对端、放好 DB 副本、打更高的 epoch、重启。
// 对已是 active 的节点幂等：keepalived 会反复通知，每次都重启就成了崩溃循环。
func (c *Controller) Activate(ctx context.Context, reason string) error {
	if !c.activating.TryLock() {
		return nil // 已有一次接任在进行
	}
	defer c.activating.Unlock()
	err := c.activate(ctx, reason)
	if err != nil {
		c.setPhase("refused", err.Error())
	}
	return err
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *Controller) activate(ctx context.Context, reason string) error {
	current := c.State()
	if current.Role == "active" {
		return nil
	}
	epoch := current.Epoch
	// 本机记住的最高水位，是对端已死、目录被复制覆盖后仍然存在的唯一下限。
	if current.SeenEpoch > epoch {
		epoch = current.SeenEpoch
	}
	// 栅栏必须在整个集群单调，而不是每节点单调。
	if c.KnownEpoch != nil {
		if floor := c.KnownEpoch(ctx); floor > epoch {
			epoch = floor
		}
	}

	// 名册非空就只用名册：NDISKLESS_PEER_URL 常是 VIP，此刻 VIP 可能就在本机，问它等于问自己。
	var probes []PeerStatusClient
	if c.ClusterPeers != nil {
		probes = c.ClusterPeers(ctx)
	}
	if len(probes) == 0 && c.Peer != nil {
		probes = []PeerStatusClient{c.Peer}
	}
	// 有没有人应答：区分「查过了，没人在服务」和「没法查」，冷却只针对后者。
	askedSomeone := len(probes) > 0
	var answered []answeredPeer
	// 不做多数派计数是有意的：复制是单向整体的 `send -R | recv -F`，没有会分叉的日志，只需防
	// 「已有写入者在服务时出现第二个」，逐个询问即可；多数派还会把不参与 VRRP 的节点算进门槛。
	// 只有 VIP 持有者在服务，而 ReconcileRole 只在本机持有 VIP 时才进 Activate。
	// 对端不可达不阻止激活：keepalived 只在自己的检查失败后才交出 VIP，epoch 栅栏仍然有效。
	for _, p := range probes {
		pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		st, err := p.Status(pctx)
		cancel()
		if err != nil {
			continue
		}
		// 经 VIP 问到的可能是自己，不能当对端。
		if c.NodeID != "" && st.NodeID == c.NodeID {
			continue
		}
		answered = append(answered, answeredPeer{status: st, client: p})
		c.observePeer(st)
		if servingNow(st, epoch) {
			err := servingRefusal(st, epoch)
			c.logf("activation refused", "reason", err.Error())
			return err
		}
		if st.Epoch > epoch {
			epoch = st.Epoch
		}
	}
	peersAnswered := len(answered) > 0
	// 没有任何可达节点拒绝：可能是对端真死了，也可能本节点是孤立侧，从这里看不出区别。
	// 冷却靠等待区分二者：死的对端一分钟后照样被接任，分区则不再每十秒重启一次服务。
	// 只在没人应答时冷却；有人应答且都不在服务就是已核实的接任（计划切换正是这样），
	// 此时等待会让 VIP 滞留在备机上。放在探测之后检查，是为了拒绝时能给出「对端仍在服务」
	// 这种更有用的原因。
	plan, err := c.settleTakeover(ctx, answered)
	if err != nil {
		c.logf("activation refused", "reason", err.Error())
		return err
	}
	if askedSomeone && !peersAnswered && current.DemotedAt != nil {
		if waited := c.now().Sub(*current.DemotedAt); waited < demoteBackoff {
			c.logf("activation deferred", "reason", "刚被降级过，等冷却结束再试",
				"waited_s", int(waited.Seconds()), "backoff_s", int(demoteBackoff.Seconds()))
			return nil
		}
	}
	if err := c.PrepareActiveDB(ctx); err != nil {
		// 本节点最终不接任，原写入者可以继续写。
		c.callOff(ctx, plan.writer)
		return err
	}
	if plan.report != nil {
		if err := writeTakeoverReport(c.ReportFile, *plan.report); err != nil {
			c.logf("could not write the takeover report; what was left behind is only in this log", "error", err)
		}
	}
	if plan.writer != nil {
		// 无论追平是否完成都要提交：两个写入者更糟，旧写入者手里的数据会在它开始跟随时保留下来。
		actx, cancel := context.WithTimeout(ctx, handoverAskTimeout)
		err := plan.writer.CommitHandover(actx, c.NodeID)
		cancel()
		if err != nil {
			c.logf("the writer did not confirm stepping down; it steps down on its own once this node serves", "error", err.Error())
		}
	}
	if err := WriteRoleState(c.RoleFile, RoleState{
		Role: "active", Epoch: epoch + 1, SeenEpoch: maxEpoch(current.SeenEpoch, epoch+1),
	}); err != nil {
		return err
	}
	if c.YieldFile != "" {
		_ = os.Remove(c.YieldFile)
	}
	c.setPhase("", "")
	if c.RecordEpoch != nil {
		c.RecordEpoch(ctx, epoch+1)
	}
	if c.StepDownFile != "" {
		ClearStepDown(c.StepDownFile)
	}
	c.markSwitching()
	c.logf("activating: restarting into the active role", "reason", reason, "epoch", epoch+1)
	c.Exit(0)
	return nil
}

// yieldVIP 在本机持有 VIP 时让出：降级标记让健康检查失败一段时间，keepalived 就会把地址移走。
func (c *Controller) yieldVIP() bool {
	if c.StepDownFile == "" || c.HoldsVIP == nil || !c.HoldsVIP() {
		return false
	}
	return MarkStepDown(c.StepDownFile) == nil
}

// markNewer 判断复制标记 a 是否比 b 新。标记形如 rep-<unix 纳秒>，读不出的视为最旧。
func markNewer(a, b string) bool {
	return markTime(a) > markTime(b)
}

func markTime(m string) int64 {
	_, digits, ok := strings.Cut(m, "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// servingNow 判断刚连上的对端是否真在服务，即在此激活是否会造成双写。
// 两个证据任一即可：epoch 比我们高（更新一代），或持有 VIP（客户机正在连它）。
// 要求二者同时成立，epoch 相撞时就会漏判；只看「角色是 active」又太严，卡死或
// 半恢复的节点会留着角色文件，拦住 VIP 持有者激活就成了没有写入者。
func servingNow(st PeerStatus, epoch int64) bool {
	if st.Role != "active" {
		return false
	}
	return st.Epoch >= epoch+1 || st.HoldsVIP
}

func servingRefusal(st PeerStatus, epoch int64) error {
	who := "对端"
	if st.NodeID != "" {
		who = "节点 " + st.NodeID
	}
	if st.HoldsVIP {
		return fmt.Errorf("%s仍持有虚 IP 并在提供服务，拒绝激活以避免双写。"+
			"若确认它已死，请先在该节点停止服务", who)
	}
	return fmt.Errorf("%s仍在提供服务（epoch %d ≥ %d），拒绝激活以避免双写。"+
		"若确认它已死，请先在该节点停止服务", who, st.Epoch, epoch+1)
}

// ObservePeers 探测每个对端一次，只为记住集群 epoch 爬到多高，不做任何决定。
// 它最有用的时机是进程启动，而那时 keepalived 还没配 VIP，做角色判断最不安全。
func (c *Controller) ObservePeers(ctx context.Context) { c.observeAllPeers(ctx) }

// observeAllPeers 见 ObservePeers；每轮对每个对端一次状态调用。
func (c *Controller) observeAllPeers(ctx context.Context) {
	var probes []PeerStatusClient
	if c.ClusterPeers != nil {
		probes = c.ClusterPeers(ctx)
	}
	if len(probes) == 0 && c.Peer != nil {
		probes = []PeerStatusClient{c.Peer}
	}
	for _, p := range probes {
		pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		st, err := p.Status(pctx)
		cancel()
		if err == nil {
			c.observePeer(st)
		}
	}
}

// observePeer 记住在任何地方见过的最高 epoch，对端消失后栅栏仍有下限。只升不降。
func (c *Controller) observePeer(st PeerStatus) {
	if st.Epoch <= 0 || c.RoleFile == "" {
		return
	}
	current := c.State()
	if st.Epoch <= current.SeenEpoch {
		return
	}
	current.SeenEpoch = st.Epoch
	if err := WriteRoleState(c.RoleFile, current); err != nil {
		c.logf("could not remember the peer epoch; the fence loses its floor if that peer dies", "error", err)
	}
}

func maxEpoch(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ReconcileRole 处理唯一不可能正确的状态：以 standby 运行却持有 VIP。
// keepalived 只在边沿通知一次，那次激活若失败（DB 副本正被复制流替换、探测超时等），
// 就再没人重试，节点一直以只读备机持有 VIP，客户机无法启动。定时调用让节点自己修好。
// active 持有 VIP、standby 不持有 VIP 都是正常状态，不处理。
func (c *Controller) ReconcileRole(ctx context.Context, holdsVIP bool) error {
	rs := c.State()
	role := rs.Role
	if holdsVIP {
		if role == "active" {
			return nil // 正常状态
		}
		c.logf("holding the VIP while running as standby; re-activating")
		return c.Activate(ctx, "self-heal: VIP held while standby")
	}
	if role != "active" {
		// 没有 VIP 的备机无需决定，但它是下一任主机，最需要知道集群 epoch 有多高。
		// 在这里只探测、不据此行动，否则主机死后它会以陈旧值激活。
		c.observeAllPeers(ctx)
		return nil
	}
	// active 却没有 VIP 是脑裂中输的一侧。只在另一节点确实在服务时才让位：
	// 切换期间 VIP 会短暂不在任何节点上，据此降级会让集群没有写入者。
	if !c.peerIsServing(ctx) {
		c.strandedRounds = 0
		return nil
	}
	// 连续两次观测才让位：切换中 VIP 可能还短暂留在旧持有者上，对单帧反应会让角色来回翻。
	c.strandedRounds++
	if c.strandedRounds < 2 {
		return nil
	}
	c.strandedRounds = 0
	c.logf("another node holds the service while this one still runs as active; stepping down")
	return c.Standby(ctx, "self-heal: active without the VIP while a peer serves")
}

// peerIsServing 报告是否有其他节点声称 active。故意不看 epoch：拿不到 VIP 的节点
// epoch 再新也服务不了客户机，持有地址才是关键。
func (c *Controller) peerIsServing(ctx context.Context) bool {
	var probes []PeerStatusClient
	if c.ClusterPeers != nil {
		probes = c.ClusterPeers(ctx)
	}
	if len(probes) == 0 && c.Peer != nil {
		probes = []PeerStatusClient{c.Peer}
	}
	for _, p := range probes {
		pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		st, err := p.Status(pctx)
		cancel()
		if err != nil {
			continue
		}
		if c.NodeID != "" && st.NodeID == c.NodeID {
			continue // 经 VIP 问到了自己，见 activate
		}
		// 自愈循环是学习对端 epoch 最勤快的地方，栅栏下限就靠这些观测积累。
		c.observePeer(st)
		if st.Role == "active" {
			return true
		}
	}
	return false
}

// Standby 让本节点降级：写标记、重启，由备机启动路径静默一切（关闸门、拆导出、清 run/）。
func (c *Controller) Standby(ctx context.Context, reason string) error {
	return c.standby(ctx, reason, 0)
}

// plannedSwitchAnswer 是计划切换降级后到重启前的等待，让 HTTP 应答先发出去。
const plannedSwitchAnswer = 500 * time.Millisecond

func (c *Controller) standby(ctx context.Context, reason string, exitAfter time.Duration) error {
	current := c.State()
	if current.Role == "standby" {
		return nil
	}
	if err := WriteRoleState(c.RoleFile, RoleState{
		Role: "standby", Epoch: current.Epoch, SeenEpoch: maxEpoch(current.SeenEpoch, current.Epoch),
	}); err != nil {
		return err
	}
	if c.StepDownFile != "" {
		if err := MarkStepDown(c.StepDownFile); err != nil {
			c.logf("step-down marker write failed; the VIP may not move", "error", err)
		}
	}
	c.markSwitching()
	c.logf("demoting: restarting into the standby role", "reason", reason, "epoch", current.Epoch)
	if exitAfter > 0 {
		go func() {
			time.Sleep(exitAfter)
			c.Exit(0)
		}()
		return nil
	}
	c.Exit(0)
	return nil
}

// NotifyBackup 处理 keepalived 的 backup/fault/stop 通知。与无条件的 Standby 不同，
// 该通知在启动和 FAULT→BACKUP→MASTER 过程中也会触发，此时节点正要升为 master；
// 在这里降级会让它永远爬不回来（降级标记让健康变红，又没有 DB 副本满足备机健康检查），
// 集群只剩一台拒绝当写入者的节点。所以只在确有对端在服务时降级，与 StartingRole、
// ReconcileRole 规则一致；漏掉的真降级几秒内会被 ReconcileRole 补上。
func (c *Controller) NotifyBackup(ctx context.Context, reason string) error {
	if c.State().Role != "active" {
		return nil
	}
	if !c.peerIsServing(ctx) {
		c.logf("keepalived backup/fault notify ignored: no peer is serving, staying active", "reason", reason)
		return nil
	}
	return c.Standby(ctx, reason)
}

func (c *Controller) logf(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Info(msg, args...)
	}
}

// DefaultRoleFile 返回角色标记路径，放在 DB 旁边。
func DefaultRoleFile(stateDir string) string { return filepath.Join(stateDir, "ha-role") }

// ErrNotActive 表示「问错了机器」。单独设哨兵是因为另一种拒绝（排空未完成）需要相反的处理：
// 在本机等待重试而不是去别处重发；合成一个状态码运维就分不清。
var ErrNotActive = errors.New("只有主机可以发起计划切换，请到主机上操作")

// IsNotActive 判断拒绝原因是否为「本节点不是主机」。
func IsNotActive(err error) bool { return errors.Is(err, ErrNotActive) }

// PlannedSwitch 主动让主机降级：先排空复制再降级。keepalived 看到健康标记变红（或进程重启）
// 后移走 VIP，对端收到通知后激活。
func (c *Controller) PlannedSwitch(ctx context.Context) error {
	if c.State().Role != "active" {
		return ErrNotActive
	}
	if c.SyncBeforeSwitch != nil {
		// 先关闸再打最后一轮：排空要几分钟，这期间落地的改动不在最后一轮里，切换后会丢。
		if c.Gate != nil {
			c.Gate.Close("正在切换主机，请稍候再操作", "")
		}
		if err := c.SyncBeforeSwitch(ctx); err != nil {
			if c.Gate != nil && c.State().Role == "active" {
				c.Gate.Open()
			}
			return fmt.Errorf("切换前同步未完成，已保持主机身份: %w", err)
		}
	}
	return c.standby(ctx, "planned switch", plannedSwitchAnswer)
}

// APIStatus 是面向运维的 HA 视图（/api/ha）。
type APIStatus struct {
	NodeID        string `json:"node_id"`
	Role          string `json:"role"`
	Epoch         int64  `json:"epoch"`
	PeerURL       string `json:"peer_url"`
	PeerReachable bool   `json:"peer_reachable"`
	PeerRole      string `json:"peer_role"`
	PeerEpoch     int64  `json:"peer_epoch"`
	// RateMBPS 是生效中的大流量限速（0 为不限）；RateSource 说明来自保存的设置还是部署默认值。
	RateMBPS   int    `json:"rate_mbps"`
	RateSource string `json:"rate_source"`
	// HoldsVIP 与 Takeover 给经 VIP 打开控制台却落到备机的运维看：本机持有 VIP，以及为何还不是写入者。
	HoldsVIP bool           `json:"holds_vip"`
	Takeover *TakeoverState `json:"takeover,omitempty"`
}

// StatusService 用控制器加尽力而为的对端探测提供 /api/ha；对端宕机时卡片也必须能渲染。
type StatusService struct {
	Controller *Controller
	NodeID     string
	PeerURL    string
	Peer       PeerStatusClient
	// Rate 读取生效限速；SaveRate 保存新值（0 为不限）。
	Rate     func(ctx context.Context) (mbps int, source string, err error)
	SaveRate func(ctx context.Context, mbps int) error
}

func (s StatusService) Status(ctx context.Context) (APIStatus, error) {
	rs := s.Controller.State()
	out := APIStatus{NodeID: s.NodeID, Role: rs.Role, Epoch: rs.Epoch, PeerURL: s.PeerURL}
	out.HoldsVIP = s.Controller.HoldsVIP != nil && s.Controller.HoldsVIP()
	if st := s.Controller.Takeover(); st.Phase != "" && rs.Role != "active" {
		out.Takeover = &st
	}
	if s.Peer != nil {
		pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		peer, err := s.Peer.Status(pctx)
		cancel()
		if err == nil {
			out.PeerReachable, out.PeerRole, out.PeerEpoch = true, peer.Role, peer.Epoch
		}
	}
	if s.Rate != nil {
		if mbps, source, err := s.Rate(ctx); err == nil {
			out.RateMBPS, out.RateSource = mbps, source
		}
	}
	return out, nil
}

// SetRate 保存大流量限速，下一条流生效。
func (s StatusService) SetRate(ctx context.Context, mbps int) error {
	if s.SaveRate == nil {
		return fmt.Errorf("限速设置在本节点不可用")
	}
	return s.SaveRate(ctx, mbps)
}

func (s StatusService) PlannedSwitch(ctx context.Context) error {
	return s.Controller.PlannedSwitch(ctx)
}

// ReplaceLiveDB 用复制副本替换活动 SQLite 文件，先删 WAL 旁路文件：
// 新主文件叠加旧节点的 -wal 打开会是损坏的数据库。
func ReplaceLiveDB(livePath string, data []byte) error {
	for _, sidecar := range []string{livePath + "-wal", livePath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	tmp := livePath + ".switch"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, livePath)
}

// 降级标记解决时序问题：降级节点约 2 秒就重启完，比 keepalived 的 fall 窗口还快，
// VIP 还没移走健康就恢复绿色，计划切换什么也没动。标记让健康在有限时间内保持红色
// （足够对端接任），之后自行过期，无需跨节点协调。

// MarkStepDown 打降级标记（mtime 为当前时间）。
func MarkStepDown(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// StepDownActive 判断窗口内是否发生过降级。
func StepDownActive(path string, window time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < window
}

// ClearStepDown 删除标记，变为 active 的节点立即重新有资格持有 VIP。
func ClearStepDown(path string) {
	_ = os.Remove(path)
}

// DefaultSwitchWindow 是「进程正在以新角色重启」作为探测无应答理由的可信时长。
// 一次切换是打标退出加 systemd 的 RestartSec，几秒而已。
const DefaultSwitchWindow = 15 * time.Second

// MarkSwitching 宣告本进程即将为切换角色而退出。
func MarkSwitching(path string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// SwitchInProgress 判断窗口内是否宣告过角色切换，健康探测据此等待而不判死。
// 标记会过期，崩溃循环的节点不能靠陈旧标记占住 VIP。
func SwitchInProgress(path string, window time.Duration) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < window
}

// ClearSwitching 在进程重新服务后删除标记。
func ClearSwitching(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

func (c *Controller) markSwitching() {
	if err := MarkSwitching(c.SwitchFile); err != nil {
		c.logf("could not announce the role switch; keepalived may treat the restart as a failure", "error", err)
	}
}

// StartingRole 用集群实际状态核对磁盘标记，决定进程以什么角色启动。
// 标记只记得「死时我是主机」，崩溃的主机回来会立即开闸，而接任者正在服务：两个写入者
// 加两个 dnsmasq；自愈循环要三四十秒才发现，在这里核对一次即可关上这个窗口。
// 只在有对端确实在服务时降级。对端不可达不算证据：最常见的启动原因是整个机房断电恢复，
// 此时让位会让集群没有写入者。
func StartingRole(ctx context.Context, marker RoleState, holdsVIP bool, peers []PeerStatusClient) string {
	if marker.Role != "active" || holdsVIP || len(peers) == 0 {
		return marker.Role
	}
	for _, p := range peers {
		pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		st, err := p.Status(pctx)
		cancel()
		if err != nil {
			continue
		}
		if st.Role == "active" {
			return "standby"
		}
	}
	return marker.Role
}
