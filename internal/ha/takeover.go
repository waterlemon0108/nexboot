package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// 会越过更合适节点的接任要拒绝，但只拒绝一次。拒绝条件（对端目录更新、健康写入者仍在、本机副本缺数据集）
// 保护他人已确认的改动；不设上限的话，拿不到 VIP 的写入者一直打新轮次，持 VIP 的备机永远落后一轮，VIP 上始终没有写入者。
// 所以只为能接 VIP 的节点让一次；VIP 又回来说明没人能接，就直接从持有最多的节点自己的地址追平后接任；
// 追平也不成就照样接任，并把遗留问题记下来给运维看。

// yieldMemory 是节点记住自己让出过 VIP 的时长。期间 VIP 回来，说明受让节点没接住。
const yieldMemory = 10 * time.Minute

// handoverDeadline 限定接任前从对端追平的时长。
const handoverDeadline = 3 * time.Minute

// handoverAskTimeout 限定交接中对写入者的单次请求。池挂住的写入者会接受连接却永不应答。
const handoverAskTimeout = 30 * time.Second

// HandoverPeer 是能交出写入者角色的对端：关写入、打最后一轮，然后降级。
// 实现了它的 PeerStatusClient 会被请求交接，没实现的只做追平。
type HandoverPeer interface {
	// PrepareHandover 关闭对端写闸门并打最后一轮，返回其标记。to 为接任节点。
	PrepareHandover(ctx context.Context, to string) (marker string, err error)
	// CommitHandover 请对端降为备机。
	CommitHandover(ctx context.Context, to string) error
	// AbortHandover 让对端恢复写入：接任取消。
	AbortHandover(ctx context.Context) error
}

// TakeoverReport 是未能追平的接任留下的记录。
type TakeoverReport struct {
	At time.Time `json:"at"`
	// Peer 是本节点没追平的那台，没人应答时为 ""。
	Peer string `json:"peer,omitempty"`
	// Mine 和 Theirs 是本节点接任时双方各自持有的轮次。
	Mine   string `json:"mine,omitempty"`
	Theirs string `json:"theirs,omitempty"`
	// Missing 列出 DB 副本里记录了、本节点池里却缺少的对象。
	Missing []string `json:"missing,omitempty"`
	// Cause 是追平未完成的原因。
	Cause string `json:"cause,omitempty"`
}

// ReadTakeoverReport 读取接任记录；没有时 ok 为 false。
func ReadTakeoverReport(path string) (TakeoverReport, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return TakeoverReport{}, false
	}
	var r TakeoverReport
	if err := json.Unmarshal(b, &r); err != nil || r.At.IsZero() {
		return TakeoverReport{}, false
	}
	return r, true
}

func writeTakeoverReport(path string, r TakeoverReport) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type answeredPeer struct {
	status PeerStatus
	client PeerStatusClient
}

func (p answeredPeer) who() string {
	if p.status.NodeID != "" {
		return "节点 " + p.status.NodeID
	}
	return "对端"
}

// takeoverPlan 是 settleTakeover 对继续进行的激活所作的决定。
type takeoverPlan struct {
	// report 在接任遗留了问题时设置。
	report *TakeoverReport
	// writer 是被请求交接、仍需降级的对端。
	writer HandoverPeer
}

// settleTakeover 根据已应答的对端做成为写入者前的最后决定：本节点让位时返回错误，继续时返回计划。
func (c *Controller) settleTakeover(ctx context.Context, answered []answeredPeer) (takeoverPlan, error) {
	missing := c.incomplete(ctx)
	mine := c.mark(ctx)
	// 仍在应答的写入者（无论健康与否）持有尚未进入任何轮次的改动；轮次更新的对端比本节点持有更多。
	var better []answeredPeer
	for _, p := range answered {
		if (c.CatalogueMark != nil && markNewer(p.status.Mark, mine)) || p.status.Role == "active" {
			better = append(better, p)
		}
	}
	if len(better) == 0 && len(missing) == 0 {
		return takeoverPlan{}, nil
	}

	if !c.yieldedRecently() {
		if to, ok := yieldCandidate(better, answered, mine, len(missing) > 0); ok {
			err := c.standAside(to, mine, missing)
			if c.yieldVIP() {
				c.rememberYield()
				err = fmt.Errorf("%w；虚 IP 已让出，由它接手", err)
			}
			return takeoverPlan{}, err
		}
	}

	var plan takeoverPlan
	src, ok := catchUpSource(answered, mine, len(missing) > 0)
	theirs, cause := "", error(nil)
	if ok {
		held := c.HoldsVIP != nil && c.HoldsVIP()
		theirs, cause = c.catchUpFrom(ctx, src, mine, len(missing) > 0)
		if errors.Is(cause, ErrWriterServing) {
			// 探测到请求之间写入者收回了 VIP：它活着且在主事，此时接任会出现两个写入者。
			err := fmt.Errorf("%s仍持有虚 IP 并在提供服务，本节点不接任", src.who())
			if c.yieldVIP() {
				c.rememberYield()
				err = fmt.Errorf("%w；虚 IP 已让出，由它接手", err)
			}
			return takeoverPlan{}, err
		}
		if hp, can := src.client.(HandoverPeer); can && src.status.Role == "active" {
			plan.writer = hp
		}
		// 追平要几分钟，VIP 几分钟内就可能移走；没有 VIP 的写入者谁也连不到。
		if held && !c.HoldsVIP() {
			c.callOff(ctx, plan.writer)
			return takeoverPlan{}, errors.New("追平目录期间虚 IP 已移到别的节点，本节点不接任")
		}
		missing, mine = c.incomplete(ctx), c.mark(ctx)
	}
	if cause == nil && len(missing) == 0 && !markNewer(theirs, mine) {
		c.logf("caught up before taking over", "from", src.who(), "round", mine)
		return plan, nil
	}
	report := TakeoverReport{At: c.now(), Mine: mine, Theirs: theirs, Missing: missing}
	if ok {
		report.Peer = src.status.NodeID
	}
	if cause != nil {
		report.Cause = cause.Error()
	}
	plan.report = &report
	c.logf("taking over without having caught up", "peer", report.Peer, "mine", mine, "theirs", theirs,
		"missing", strings.Join(missing, "、"), "cause", report.Cause)
	return plan, nil
}

// standAside 为本节点让位给的节点组织拒绝文案。
func (c *Controller) standAside(to answeredPeer, mine string, missing []string) error {
	switch {
	case len(missing) > 0:
		return fmt.Errorf("本节点的目录不完整（缺少 %s 的数据集），先让%s接任", strings.Join(missing, "、"), to.who())
	case c.CatalogueMark != nil && markNewer(to.status.Mark, mine):
		return fmt.Errorf("本节点的目录落后于%s（本机同步到 %s，对端到 %s），不接任：接任会丢掉它已有的改动",
			to.who(), markClock(mine), markClock(to.status.Mark))
	default:
		return fmt.Errorf("%s仍是健康的主机，本节点不接任，以免丢掉它刚确认的改动", to.who())
	}
}

// yieldCandidate 挑一个值得让出 VIP 的节点：必须健康（才能接住 VIP），且比本节点更合适；
// 本节点副本不完整时，只要不落后于本节点即可。
func yieldCandidate(better, answered []answeredPeer, mine string, incomplete bool) (answeredPeer, bool) {
	for _, p := range better {
		if p.status.Healthy {
			return p, true
		}
	}
	if incomplete {
		for _, p := range answered {
			if p.status.Healthy && !markNewer(mine, p.status.Mark) {
				return p, true
			}
		}
	}
	return answeredPeer{}, false
}

// catchUpSource 选择追平来源：有写入者应答就用写入者（它必然持有最新目录），否则用轮次最新、
// 且领先本节点的对端（本节点副本不完整时，持平也可，对端可能补齐）。
func catchUpSource(answered []answeredPeer, mine string, incomplete bool) (answeredPeer, bool) {
	var best answeredPeer
	found := false
	for _, p := range answered {
		if p.status.Role == "active" {
			return p, true
		}
		if !found || markNewer(p.status.Mark, best.status.Mark) {
			best, found = p, true
		}
	}
	if !found {
		return answeredPeer{}, false
	}
	if markNewer(best.status.Mark, mine) || (incomplete && !markNewer(mine, best.status.Mark)) {
		return best, true
	}
	return answeredPeer{}, false
}

// catchUpFrom 从 src 自己的地址拉取，让本节点与 src 持平，返回目标轮次。
// 返回错误表示本节点可能比 src 少，包括无法确认的情况。
func (c *Controller) catchUpFrom(ctx context.Context, src answeredPeer, mine string, incomplete bool) (string, error) {
	target := src.status.Mark
	var unchecked error
	if hp, ok := src.client.(HandoverPeer); ok && src.status.Role == "active" {
		actx, cancel := context.WithTimeout(ctx, handoverAskTimeout)
		final, err := hp.PrepareHandover(actx, c.NodeID)
		cancel()
		switch {
		case errors.Is(err, ErrWriterServing):
			return target, err
		case err != nil:
			c.logf("the writer could not stamp a final round; catching up to what it holds", "peer", src.who(), "error", err.Error())
			unchecked = fmt.Errorf("%s没能打出最后一轮，它上一轮之后的改动无从核对：%w", src.who(), err)
		case final != "":
			target = final
		}
	}
	if !markNewer(target, mine) && !incomplete {
		// 没有已知需要追的内容，不值得等一个连轮次都打不出的写入者。
		return target, unchecked
	}
	if c.CatchUp == nil {
		return target, errors.New("本节点没有直接拉取目录的通道")
	}
	c.setPhase("catching-up", fmt.Sprintf("正在从%s追平目录，追平后接任（最多 %d 分钟）", src.who(), int(handoverDeadline.Minutes())))
	cctx, cancel := context.WithTimeout(ctx, handoverDeadline)
	defer cancel()
	if err := c.CatchUp(cctx, src.client, target); err != nil {
		return target, err
	}
	return target, unchecked
}

// callOff 通知被请求交接的写入者可以恢复写入。
func (c *Controller) callOff(ctx context.Context, writer HandoverPeer) {
	if writer == nil {
		return
	}
	actx, cancel := context.WithTimeout(ctx, handoverAskTimeout)
	defer cancel()
	if err := writer.AbortHandover(actx); err != nil {
		c.logf("could not tell the writer the handover is off; it resumes on its own in a few minutes", "error", err.Error())
	}
}

func (c *Controller) incomplete(ctx context.Context) []string {
	if c.CatalogueIncomplete == nil {
		return nil
	}
	return c.CatalogueIncomplete(ctx)
}

func (c *Controller) mark(ctx context.Context) string {
	if c.CatalogueMark == nil {
		return ""
	}
	return c.CatalogueMark(ctx)
}

// yieldedRecently 从文件内容而非 mtime 读回时间，读写都用控制器的同一个时钟。
func (c *Controller) yieldedRecently() bool {
	if c.YieldFile == "" {
		return false
	}
	b, err := os.ReadFile(c.YieldFile)
	if err != nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return err == nil && c.now().Sub(at) < yieldMemory
}

func (c *Controller) rememberYield() {
	if c.YieldFile == "" {
		return
	}
	if err := os.WriteFile(c.YieldFile, []byte(c.now().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		c.logf("could not remember giving the VIP up; this node may give it up again", "error", err)
	}
}

// TakeoverState 向在该节点打开控制台的运维说明：持有 VIP 的节点为何还不是写入者。
type TakeoverState struct {
	// Phase 为 "refused" 或 "catching-up"，无可说明时为 ""。
	Phase  string    `json:"phase,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitempty"`
}

// takeoverStateFresh 是拒绝信息值得展示的时长。自愈循环每 15 秒重试，更早的已不代表现状。
const takeoverStateFresh = time.Minute

func (c *Controller) setPhase(phase, reason string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.takeover = TakeoverState{Phase: phase, Reason: reason, Since: c.now()}
}

// Takeover 报告本节点上一次接任尝试的结果。
func (c *Controller) Takeover() TakeoverState {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.takeover.Phase == "refused" && c.now().Sub(c.takeover.Since) > takeoverStateFresh {
		return TakeoverState{}
	}
	return c.takeover
}

// markClock 返回轮次打标的时间，用于运维看的提示。
func markClock(marker string) string {
	n := markTime(marker)
	if n <= 0 {
		return "尚无记录"
	}
	return time.Unix(0, n).Local().Format("15:04:05")
}
