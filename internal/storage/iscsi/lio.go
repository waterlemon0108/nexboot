// Package iscsi 直接通过 configfs 驱动内核 LIO iSCSI target：mkdir/rmdir 建删对象，
// 写属性文件设参数，LUN 映射是 LUN 组指向 backstore 的符号链接。
// 不走 targetcli：它每次约 250ms 的 Python 启动只能串行，开机风暴会全卡在这里。
//
// 从不写 /etc/target/saveconfig.json：服务端重启后客户机本就要重连，/boot 会幂等重建 LIO 状态。
package iscsi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// lioConfigfsBase 是 LIO iSCSI fabric 的 configfs 根目录。
const lioConfigfsBase = "/sys/kernel/config/target/iscsi"

// portalAddr 与 targetcli `/iscsi create` 自动建的通配 portal 一致，保持客户机熟悉的布局。
const portalAddr = "0.0.0.0:3260"

type LIO struct {
	logger *slog.Logger
	// configfsBase 可在测试中替换。
	configfsBase string
	// locks 每个 target IQN 一把锁：同一 MAC 的拆除与重建互斥，不同客户机并行。
	locks sync.Map
	// allocMu 保护分配 HBA 序号时“扫描后 mkdir”的窗口。
	allocMu sync.Mutex
}

type ExportRequest struct {
	MAC     string
	VolPath string
	LUN     int
	// CHAP 非空时 target 要求登录认证；为 nil 时走 demo mode（无认证），未配置密钥的部署行为不变。
	// 同一次导出的所有请求携带相同凭据（一个 MAC 一个 target）。
	CHAP *CHAPCreds
}

// Available 报告 LIO 能否创建 target。iscsi fabric 目录要首次 mkdir 才注册，
// 只 Stat 会把从未导出过的节点误判为不可用（健康检查变红、keepalived 进 FAULT），所以用 mkdir 探测。
func Available() bool { return availableAt(lioConfigfsBase) }

func availableAt(base string) bool {
	if _, err := os.Stat(base); err == nil {
		return true
	}
	return os.MkdirAll(base, 0o755) == nil
}

func New(logger *slog.Logger) *LIO {
	if logger == nil {
		logger = slog.Default()
	}
	return &LIO{logger: logger, configfsBase: lioConfigfsBase}
}

// corePath 是 backstore 的 configfs 根目录，与 iscsi fabric 目录同级。
func (l *LIO) corePath() string {
	return filepath.Join(filepath.Dir(l.configfsBase), "core")
}

func (l *LIO) lockTarget(target string) func() {
	mu, _ := l.locks.LoadOrStore(target, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// ExportBlocks 把同一客户机的所有盘导出到同一个 target 的多个 LUN。
func (l *LIO) ExportBlocks(ctx context.Context, reqs []ExportRequest) ([]storage.LUN, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	for _, req := range reqs {
		if req.MAC == "" || req.VolPath == "" {
			return nil, fmt.Errorf("mac and volpath are required")
		}
	}
	target := TargetForMAC(reqs[0].MAC)
	defer l.lockTarget(target)()

	tpg := filepath.Join(l.configfsBase, target, "tpgt_1")
	if err := os.MkdirAll(tpg, 0o755); err != nil {
		return nil, fmt.Errorf("configfs create target %s: %w", target, err)
	}
	if err := writeAttr(filepath.Join(tpg, "attrib", "demo_mode_write_protect"), "0"); err != nil {
		return nil, err
	}
	// 内核默认 authentication=1，demo mode 客户机无法登录，必须显式设置，见 applyCHAP。
	if err := applyCHAP(tpg, reqs[0].MAC, reqs[0].CHAP); err != nil {
		return nil, err
	}
	if err := createPortal(tpg); err != nil {
		return nil, err
	}
	luns := make([]storage.LUN, 0, len(reqs))
	for _, req := range reqs {
		so, err := l.ensureBackstore(ctx, backstoreName(target, req.LUN), req.VolPath, unitSerial(target, req.LUN))
		if err != nil {
			return nil, err
		}
		lunDir := filepath.Join(tpg, "lun", fmt.Sprintf("lun_%d", req.LUN))
		if err := os.MkdirAll(lunDir, 0o755); err != nil {
			return nil, fmt.Errorf("configfs create lun %d: %w", req.LUN, err)
		}
		if err := os.Symlink(so, filepath.Join(lunDir, filepath.Base(so))); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("configfs map lun %d -> %s: %w", req.LUN, so, err)
		}
		// CHAP 下没有自动 ACL，必须手工把显式 ACL 映射到每个 LUN；
		// 否则凭据正确也会在认证阶段被拒（"CHAP_N values do not match"）。
		if req.CHAP != nil {
			macl := filepath.Join(tpg, "acls", InitiatorIQN(req.MAC), fmt.Sprintf("lun_%d", req.LUN))
			if err := os.MkdirAll(macl, 0o755); err != nil {
				return nil, fmt.Errorf("configfs create mapped lun %d: %w", req.LUN, err)
			}
			if err := os.Symlink(lunDir, filepath.Join(macl, filepath.Base(so))); err != nil && !os.IsExist(err) {
				return nil, fmt.Errorf("configfs map lun %d to acl: %w", req.LUN, err)
			}
		}
		luns = append(luns, storage.LUN{Target: target, LUN: req.LUN, VolPath: req.VolPath})
	}
	// 最后才 enable，客户机不会登录到半成品 target；重复导出时 writeAttr 读到 "1" 不会再写。
	if err := writeAttr(filepath.Join(tpg, "enable"), "1"); err != nil {
		return nil, err
	}
	l.logger.Info("lio export", "target", target, "luns", len(luns))
	return luns, nil
}

// ensureBackstore 返回指定 iblock 存储对象的 configfs 目录，不存在时创建；已存在的原样复用。
func (l *LIO) ensureBackstore(ctx context.Context, name, dev, serial string) (string, error) {
	if matches, err := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", name)); err == nil && len(matches) > 0 {
		return matches[0], nil
	}
	node, err := zfs.ResolveNode(ctx, dev)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		so, err := l.allocBackstore(name)
		if err != nil {
			return "", err
		}
		if err := configureBackstore(so, dev, node, serial); err != nil {
			// 清掉半配置的对象，免得重试或下次开机复用它。
			_ = removeTree(so)
			_ = os.Remove(filepath.Dir(so))
			lastErr = err
			continue
		}
		return so, nil
	}
	return "", lastErr
}

// allocBackstore 占用一个空闲 iblock HBA 序号并在其下建存储对象，一个对象独占一个 HBA（同 rtslib-fb）。
func (l *LIO) allocBackstore(name string) (string, error) {
	l.allocMu.Lock()
	defer l.allocMu.Unlock()
	core := l.corePath()
	next := 0
	if entries, err := os.ReadDir(core); err == nil {
		for _, e := range entries {
			if s, ok := strings.CutPrefix(e.Name(), "iblock_"); ok {
				if n, err := strconv.Atoi(s); err == nil && n >= next {
					next = n + 1
				}
			}
		}
	}
	hba := filepath.Join(core, fmt.Sprintf("iblock_%d", next))
	if err := os.MkdirAll(hba, 0o755); err != nil {
		return "", fmt.Errorf("configfs create hba %s: %w", hba, err)
	}
	so := filepath.Join(hba, name)
	if err := os.Mkdir(so, 0o755); err != nil {
		return "", fmt.Errorf("configfs create backstore %s: %w", so, err)
	}
	return so, nil
}

// configureBackstore 用解析出的 /dev/zdN 节点（而非 udev 符号链接）打开盘并启用对象。
// 顺序是内核约束：udev_path 经 control 写入后才能 enable；映射到 LUN 后序列号和 emulate_tpu 不可再改，
// 所以必须在 ExportBlocks 建链接之前全部写完。
func configureBackstore(so, volPath, node, serial string) error {
	if err := writeAttr(filepath.Join(so, "control"), "udev_path="+node); err != nil {
		return err
	}
	// 顶层 udev_path 只用于展示（targetcli ls），写稳定的 /dev/zvol 路径方便运维查看。
	if err := writeAttr(filepath.Join(so, "udev_path"), volPath); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(so, "enable"), "1"); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(so, "wwn", "vpd_unit_serial"), serial); err != nil {
		return err
	}
	// 与 rtslib 默认一致：INQUIRY 型号用 backstore 名而非 "IBLOCK"，Windows 以此显示盘名。
	if err := writeAttr(filepath.Join(so, "attrib", "emulate_model_alias"), "1"); err != nil {
		return err
	}
	return writeAttr(filepath.Join(so, "attrib", "emulate_tpu"), "1")
}

// applyCHAP 设置 TPG 的认证方式，二选一：
//   - demo mode（creds 为 nil）：generate_node_acls=1、authentication=0，任何人可登录。
//   - CHAP：generate_node_acls=0、authentication=1，凭据写在以 initiator IQN 命名的显式 ACL 上。
//     generate_node_acls=1 时 TPG 级认证会拒绝所有 initiator，只有这种布局可用；
//     ACL 名必须等于启动脚本里的 `set initiator-iqn`。
func applyCHAP(tpg, mac string, creds *CHAPCreds) error {
	if creds == nil {
		if err := writeAttr(filepath.Join(tpg, "attrib", "generate_node_acls"), "1"); err != nil {
			return err
		}
		return writeAttr(filepath.Join(tpg, "attrib", "authentication"), "0")
	}
	if err := writeAttr(filepath.Join(tpg, "attrib", "generate_node_acls"), "0"); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(tpg, "attrib", "authentication"), "1"); err != nil {
		return err
	}
	acl := filepath.Join(tpg, "acls", InitiatorIQN(mac))
	if err := os.MkdirAll(acl, 0o755); err != nil {
		return fmt.Errorf("configfs create acl %s: %w", acl, err)
	}
	if err := writeAttrExact(filepath.Join(acl, "auth", "userid"), creds.Username); err != nil {
		return err
	}
	return writeAttrExact(filepath.Join(acl, "auth", "password"), creds.Password)
}

// createPortal 建通配监听 portal。所有 target 共用一个引用计数的 0.0.0.0:3260 监听，
// 并发拆除释放最后一个引用时，这里的 mkdir 会偶发 EINVAL/EBUSY，只对这两种错误重试，其他错误立即返回。
func createPortal(tpg string) error {
	path := filepath.Join(tpg, "np", portalAddr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.MkdirAll(path, 0o755)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("configfs create portal %s: %w", portalAddr, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("configfs create portal %s: %w", portalAddr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// iblockDevRe 从存储对象的 info 属性（"... iBlock device: zd112 ..."）取内核设备名。
var iblockDevRe = regexp.MustCompile(`iBlock device: (zd\d+)`)

// unitSerial 由 (target, LUN) 确定性地生成 VPD 序列号，不用随机值：序列号是盘在 initiator 侧的身份，
// Windows 见到变化会当成换盘（系统盘 0x7A/F4）。各节点、每次开机都一致，是接管时客户机不掉盘的前提。
func unitSerial(target string, lun int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("ndiskless-vpd:%s:%d", target, lun)))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40 // 与 rtslib 一样采用 uuid4 格式
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TeardownTarget 删除客户机的整个 target 及给定 LUN 的 backstore，与 ExportBlocks 配对。
// 不存在视为成功：/boot 每次请求都会重跑拆除。
func (l *LIO) TeardownTarget(_ context.Context, target string, luns []int) error {
	defer l.lockTarget(target)()
	if err := l.removeTarget(target); err != nil {
		return err
	}
	nodes := make([]string, 0, len(luns))
	for _, lun := range luns {
		n, err := l.removeBackstoreTree(backstoreName(target, lun))
		if err != nil {
			return err
		}
		nodes = append(nodes, n...)
	}
	if err := waitNodesReleased(nodes); err != nil {
		return err
	}
	l.logger.Info("lio teardown", "target", target, "luns", len(luns))
	return nil
}

// DeleteLUNs 删除指定 LUN 的映射和 backstore，保留 target 本身。
func (l *LIO) DeleteLUNs(_ context.Context, target string, luns []int) error {
	if len(luns) == 0 {
		return nil
	}
	defer l.lockTarget(target)()
	// ACL 的 mapped LUN 会占住 TPG LUN，所以先删映射；只删映射不删 ACL，
	// 删 ACL 会断掉会话，在线发布时客户机的系统盘也跟着掉。
	unmapLUNs(filepath.Join(l.configfsBase, target, "tpgt_1"), luns)
	var nodes []string
	for _, lun := range luns {
		lunDir := filepath.Join(l.configfsBase, target, "tpgt_1", "lun", fmt.Sprintf("lun_%d", lun))
		if err := removeTree(lunDir); err != nil {
			return err
		}
		n, err := l.removeBackstoreTree(backstoreName(target, lun))
		if err != nil {
			return err
		}
		nodes = append(nodes, n...)
	}
	return waitNodesReleased(nodes)
}

func (l *LIO) DeleteTarget(_ context.Context, target string) error {
	defer l.lockTarget(target)()
	return l.removeTarget(target)
}

// removeTarget 删除 target 的 configfs 树；先 disable TPG，让内核在对象消失前关闭会话。
func (l *LIO) removeTarget(target string) error {
	dir := filepath.Join(l.configfsBase, target)
	enable := filepath.Join(dir, "tpgt_1", "enable")
	if cur, err := os.ReadFile(enable); err == nil && strings.TrimSpace(string(cur)) == "1" {
		_ = os.WriteFile(enable, []byte("0\n"), 0o644)
	}
	// CHAP target 的 ACL 映射占住 LUN，必须先于 LUN 删除，否则内核报 EBUSY；
	// removeTree 的遍历顺序不保证这一点，所以显式先清 ACL。
	clearACLs(filepath.Join(dir, "tpgt_1"))
	return removeTree(dir)
}

// unmapLUNs 只从各 ACL 中删除这些 LUN 的映射，ACL 及其会话保留。
func unmapLUNs(tpg string, luns []int) {
	acls := filepath.Join(tpg, "acls")
	entries, err := os.ReadDir(acls)
	if err != nil {
		return
	}
	for _, e := range entries {
		for _, lun := range luns {
			_ = removeTree(filepath.Join(acls, e.Name(), fmt.Sprintf("lun_%d", lun)))
		}
	}
}

// clearACLs 删除 TPG 下所有 ACL；demo mode 没有 ACL，此时为空操作。
func clearACLs(tpg string) {
	acls := filepath.Join(tpg, "acls")
	entries, err := os.ReadDir(acls)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = removeTree(filepath.Join(acls, e.Name()))
	}
}

// removeBackstoreTree 删除存储对象及其独占 HBA，返回内核占用的 /dev/zdN 节点；不存在视为成功。
// rmdir 只是开始异步释放独占，调用方在全部删除后统一 waitNodesReleased，让多盘的等待重叠。
func (l *LIO) removeBackstoreTree(name string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", name))
	if err != nil || len(matches) == 0 {
		return nil, nil
	}
	var nodes []string
	for _, so := range matches {
		// 等 info 里记录的 zd 节点，而不是 /dev/zvol 符号链接（风暴中可能滞后或指向复用的 minor）；
		// 拆除返回后启动流程会立即 zfs destroy。
		node := ""
		if data, err := os.ReadFile(filepath.Join(so, "info")); err == nil {
			if m := iblockDevRe.FindStringSubmatch(string(data)); m != nil {
				node = "/dev/" + m[1]
			}
		}
		if err := removeTree(so); err != nil {
			return nil, err
		}
		_ = os.Remove(filepath.Dir(so))
		if node != "" {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func waitNodesReleased(nodes []string) error {
	for _, node := range nodes {
		if err := zfs.WaitReleased(node); err != nil {
			return err
		}
	}
	return nil
}

// removeTree 删除 configfs 组及其中用户创建的内容。内核生成的属性和默认组不可删（EPERM），
// 会随父目录 rmdir 消失，所以忽略单项错误，只以顶层目录是否消失判断成功。
func removeTree(top string) error {
	if _, err := os.Lstat(top); os.IsNotExist(err) {
		return nil
	}
	var walk func(string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(p)
			}
			_ = os.Remove(p)
		}
	}
	walk(top)
	if err := os.Remove(top); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("configfs remove %s: %w", top, err)
	}
	return nil
}

// writeAttr 在属性值不同时才写入：tpg enable、generate_node_acls 等对重复写会报 EINVAL。
// MkdirAll 在真实 configfs 上是空操作，只为让单测能在普通临时目录上跑。
func writeAttr(path, value string) error {
	if cur, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(cur)) == value {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("configfs write %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("configfs write %s = %s: %w", path, value, err)
	}
	return nil
}

// writeAttrExact 写入不带结尾换行的属性值。CHAP 的 auth/userid、auth/password 按字节比较，
// 带换行时凭据正确也会报 "CHAP_N values do not match"。
func writeAttrExact(path, value string) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == value {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("configfs write %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("configfs write %s: %w", path, err)
	}
	return nil
}

// iqnPrefix 是 target 与 initiator IQN 的共同前缀，两端只能从这里取，否则会对不上。
const iqnPrefix = "iqn.2026-06.local.ndiskless:"

// TargetForMAC 返回客户机的 target IQN。
func TargetForMAC(mac string) string {
	return iqnPrefix + "client-" + strings.ToLower(storage.NormalizeMAC(mac))
}

// InitiatorIQN 返回客户机端的 initiator IQN，iPXE 脚本用它 `set initiator-iqn`。
func InitiatorIQN(mac string) string {
	return iqnPrefix + "initiator-" + strings.ToLower(storage.NormalizeMAC(mac))
}

// targetMACRe 从 target IQN 目录名中取出 MAC。
var targetMACRe = regexp.MustCompile(`:client-([0-9a-fA-F]{12})`)

// noSessionMarker 是显式 ACL 无人登录时 info 文件的内容。
const noSessionMarker = "No active iSCSI Session"

// hasLiveSession 报告 target 当前是否有客户机登录。两种模式要分别看：
// demo mode 的会话在 tpgt_1/dynamic_sessions；CHAP 的会话不是动态的，只在 acls/<initiator-iqn>/info。
// 漏看 CHAP 会把在线客户机全判为离线，离线克隆回收随后会删掉正在运行的系统盘。
func (l *LIO) hasLiveSession(target string) bool {
	dyn, err := os.ReadFile(filepath.Join(l.configfsBase, target, "tpgt_1", "dynamic_sessions"))
	if err == nil && strings.TrimSpace(string(dyn)) != "" {
		return true
	}
	acls, err := os.ReadDir(filepath.Join(l.configfsBase, target, "tpgt_1", "acls"))
	if err != nil {
		return false
	}
	for _, acl := range acls {
		info, err := os.ReadFile(filepath.Join(l.configfsBase, target, "tpgt_1", "acls", acl.Name(), "info"))
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(info))
		if text != "" && !strings.HasPrefix(text, noSessionMarker) {
			return true
		}
	}
	return false
}

// ActiveSessions 从 configfs 读出有 iSCSI 会话的客户机 MAC；开机的无盘客户机必然保持系统盘会话，无需客户机代理。
func (l *LIO) ActiveSessions(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(l.configfsBase)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", l.configfsBase, err)
	}
	macs := []string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		m := targetMACRe.FindStringSubmatch(entry.Name())
		if m == nil {
			continue // 不是本系统的客户机 target（如 discovery_auth）
		}
		// 读不到视为无会话，跳过而不是让整次扫描失败。
		if !l.hasLiveSession(entry.Name()) {
			continue
		}
		mac := strings.ToUpper(m[1])
		if !seen[mac] {
			seen[mac] = true
			macs = append(macs, mac)
		}
	}
	return macs, nil
}

func backstoreName(target string, lun int) string {
	name := strings.NewReplacer(".", "-", ":", "-", "_", "-", "/", "-").Replace(target)
	return fmt.Sprintf("%s-lun%d", name, lun)
}

// TeardownAll 删除所有 ndiskless 的 target 和 backstore，不碰别人建的对象。
// 节点降为备机时调用：仍在供盘的备机等于第二个写入者。
func (l *LIO) TeardownAll(ctx context.Context) error {
	entries, err := os.ReadDir(l.configfsBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // configfs 不存在即没有任何导出
		}
		return err
	}
	var nodes []string
	for _, e := range entries {
		target := e.Name()
		if !strings.HasPrefix(target, iqnPrefix) {
			continue
		}
		unlock := l.lockTarget(target)
		err := l.removeTarget(target)
		if err == nil {
			// backstore 名由 target 名派生，用通配匹配全部 LUN。
			prefix := strings.NewReplacer(".", "-", ":", "-", "_", "-", "/", "-").Replace(target)
			matches, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", prefix+"-lun*"))
			for _, so := range matches {
				released, rmErr := l.removeBackstoreTree(filepath.Base(so))
				if rmErr == nil {
					nodes = append(nodes, released...)
				}
			}
		}
		unlock()
		if err != nil {
			return fmt.Errorf("teardown %s: %w", target, err)
		}
	}
	_ = ctx
	return waitNodesReleased(nodes)
}
