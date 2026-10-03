package ha

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VRRP 走单播（机房交换机常过滤组播），MASTER 只向 keepalived.conf 里列出的 peer 发通告。
// 不在列表里的节点收不到通告，超时后自选为 master，VIP 落到两台机器上。安装器用 --peer
// 写初始列表，之后由这里保持与集群名册一致；界面加入的节点要等写入者列出它才能启动 keepalived。

var unicastPeerBlock = regexp.MustCompile(`(?s)(unicast_peer\s*\{)(.*?)(\n\s*\})`)

// 只匹配数字：安装器模板在行尾带了说明注释，按行尾锚定会匹配不到。
var priorityLine = regexp.MustCompile(`(?m)^(\s*priority\s+)(\d+)`)

// writerPriority 是写入者所在的优先级档位，必须一直胜出，下面的备机排名不碰带这个值的配置。
const writerPriority = 150

// standbyBase 是备机的最低档位，排名从这里往上加。
const standbyBase = 100

// StandbyPriority 返回 self 在整个名册中的 VRRP 优先级：基准加上它在排序后地址中的位置。
// 排名是为了不让两台备机平票：平票时两边都会短暂宣告 MASTER、各自触发激活，
// 集群出现两个持有相同 epoch 的写入者。
func StandbyPriority(roster []string, self string) int {
	sorted := desiredPeers(roster, "")
	for i, addr := range sorted {
		if addr == self {
			return standbyBase + i
		}
	}
	return standbyBase
}

// WithStandbyPriority 把 conf 的 priority 行设为 want，写入者的配置不动。返回是否有改动。
func WithStandbyPriority(conf string, want int) (string, bool) {
	m := priorityLine.FindStringSubmatchIndex(conf)
	if m == nil {
		return conf, false
	}
	current := conf[m[4]:m[5]]
	if current == strconv.Itoa(writerPriority) || current == strconv.Itoa(want) {
		return conf, false
	}
	return conf[:m[4]] + strconv.Itoa(want) + conf[m[5]:], true
}

// UnicastPeers 按文件顺序列出 unicast_peer 块中的地址，没有该块时返回 nil。
func UnicastPeers(conf string) []string {
	m := unicastPeerBlock.FindStringSubmatch(conf)
	if m == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(m[2], "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// WithUnicastPeers 让 conf 的 unicast_peer 块恰好列出 peers。没有该块的配置直接拒绝而不猜：
// 它不是安装器写的，盲改可能破坏手工配置的 VRRP。
func WithUnicastPeers(conf string, peers []string) (string, error) {
	m := unicastPeerBlock.FindStringSubmatchIndex(conf)
	if m == nil {
		return "", errors.New("keepalived.conf has no unicast_peer block")
	}
	var body strings.Builder
	for _, p := range peers {
		body.WriteString("\n        ")
		body.WriteString(p)
	}
	// m[4]:m[5] 是块体（第 2 组），开闭括号保持原样。
	return conf[:m[4]] + body.String() + conf[m[5]:], nil
}

// PeerReconciler 让 keepalived 的单播 peer 列表与名册一致：其他各节点的真实地址，排序、不含自己。
// 只在文件真正变化时 Reload；keepalived 收到 SIGHUP 重读配置不会丢掉已持有的 VIP。
type PeerReconciler struct {
	// ConfPath 是 keepalived.conf；文件不存在表示本节点无 VRRP 角色（单机，或加入中尚未写配置），此时不做任何事。
	ConfPath string
	// Self 是本节点自己的地址，不会列为自己的 peer。
	Self string
	// Roster 返回集群各节点的真实地址（包含自己也可以）。
	Roster func(ctx context.Context) ([]string, error)
	// Reload 让 keepalived 重读配置。
	Reload func(ctx context.Context) error
	Logger *slog.Logger
}

// Once 比较文件与名册，有偏差时重写并重载。空名册视为错误而非「没有 peer」：
// 一次 DB 抖动不能把正常集群的 VRRP peer 清空。
func (r PeerReconciler) Once(ctx context.Context) (bool, error) {
	data, err := os.ReadFile(r.ConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	roster, err := r.Roster(ctx)
	if err != nil {
		return false, err
	}
	want := desiredPeers(roster, r.Self)
	if len(want) == 0 {
		return false, errors.New("roster lists no other node; leaving the peer list alone")
	}
	// 顺序由安装器决定，不算偏差：按集合比较，避免仅为排序就重写并重载。
	// 优先级与 peer 列表同源，也会因同样原因漂移：安装器按当时的 --peer 排名，名册还不全时
	// 加入的节点排名偏高（实测两台都落在 101），所以在这里一并对齐。
	have := UnicastPeers(string(data))
	conf, prioChanged := WithStandbyPriority(string(data), StandbyPriority(roster, r.Self))
	if equalStrings(desiredPeers(have, ""), want) && !prioChanged {
		return false, nil
	}
	out, err := WithUnicastPeers(conf, want)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(r.ConfPath, []byte(out), 0o600); err != nil {
		return false, err
	}
	if r.Logger != nil {
		r.Logger.Info("keepalived peers follow the roster", "was", have, "now", want)
	}
	if r.Reload != nil {
		if err := r.Reload(ctx); err != nil {
			return true, fmt.Errorf("keepalived reload: %w", err)
		}
	}
	return true, nil
}

// Run 定时对账，直到 ctx 结束。
func (r PeerReconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.Once(ctx); err != nil && r.Logger != nil {
				r.Logger.Warn("keepalived peer reconcile failed", "error", err)
			}
		}
	}
}

// desiredPeers 返回去掉自己、去重并按地址排序的名册。
func desiredPeers(roster []string, self string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ip := range roster {
		ip = strings.TrimSpace(ip)
		if ip == "" || ip == self || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return lessIP(out[i], out[j]) })
	return out
}

// lessIP 按数值排序地址（10.0.0.9 在 10.0.0.10 之前），解析不了的按字符串排序。
func lessIP(a, b string) bool {
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	if ia == nil || ib == nil {
		return a < b
	}
	if ia4, ib4 := ia.To4(), ib.To4(); ia4 != nil && ib4 != nil {
		ia, ib = ia4, ib4
	}
	return string(ia) < string(ib)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeFileAtomic 先写同目录临时文件再改名：keepalived 重载时绝不能读到写了一半的配置。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// JoinFinisher 完成界面发起的加入。安装器已写好 keepalived 配置，但让守护进程保持停止并留下 Marker；
// 要等写入者自己的 peer 列表包含本节点后才能启动 VRRP，提前启动收不到通告会与真 master 抢 VIP。
// 标记跨重启保留，中断的加入会接着完成。
type JoinFinisher struct {
	Marker string
	// Self 是本节点的地址，与写入者列出的形式一致。
	Self string
	// Writer 经 VIP 探测当前写入者的 HA 状态。
	Writer func(ctx context.Context) (PeerStatus, error)
	// Enable 启动 keepalived（systemctl enable --now）。
	Enable func(ctx context.Context) error
	Logger *slog.Logger
}

// Once 在没有待办（无标记，或刚启动了 keepalived）时返回 done=true。
// 没有写入者已列出本节点的确切证据，绝不启动 keepalived。
func (f JoinFinisher) Once(ctx context.Context) (bool, error) {
	if _, err := os.Stat(f.Marker); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	st, err := f.Writer(ctx)
	if err != nil {
		return false, fmt.Errorf("writer unreachable, keepalived stays off: %w", err)
	}
	listed := false
	for _, p := range st.VRRPPeers {
		if p == f.Self {
			listed = true
			break
		}
	}
	if !listed {
		return false, nil
	}
	if err := f.Enable(ctx); err != nil {
		return false, fmt.Errorf("start keepalived: %w", err)
	}
	if err := os.Remove(f.Marker); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if f.Logger != nil {
		f.Logger.Info("join complete: writer lists this node as a VRRP peer, keepalived started", "self", f.Self)
	}
	return true, nil
}

// Run 轮询直到加入完成或 ctx 结束。
func (f JoinFinisher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		done, err := f.Once(ctx)
		if err != nil && f.Logger != nil {
			f.Logger.Warn("join not finished yet", "error", err)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
