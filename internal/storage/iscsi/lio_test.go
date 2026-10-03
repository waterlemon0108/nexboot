package iscsi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

func newTestLIO(t *testing.T) *LIO {
	t.Helper()
	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.configfsBase = filepath.Join(t.TempDir(), "iscsi")
	return l
}

// makeDev 创建替身设备文件：ExportBlocks 要等 zvol 路径出现才配置 backstore。
func makeDev(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAttr(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func TestExportBlocksLaysOutConfigfs(t *testing.T) {
	l := newTestLIO(t)
	target := "iqn.2026-06.local.ndiskless:client-aabbccddeeff"
	devs := []string{makeDev(t, "CLIENT-AABBCCDDEEFF"), makeDev(t, "CLIENT-AABBCCDDEEFF-DATA-1")}

	luns, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: devs[0], LUN: 0},
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: devs[1], LUN: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(luns, []storage.LUN{
		{Target: target, LUN: 0, VolPath: devs[0]},
		{Target: target, LUN: 1, VolPath: devs[1]},
	}) {
		t.Fatalf("luns = %#v", luns)
	}

	tpg := filepath.Join(l.configfsBase, target, "tpgt_1")
	if got := readAttr(t, filepath.Join(tpg, "attrib", "generate_node_acls")); got != "1" {
		t.Fatalf("generate_node_acls = %q", got)
	}
	if got := readAttr(t, filepath.Join(tpg, "attrib", "demo_mode_write_protect")); got != "0" {
		t.Fatalf("demo_mode_write_protect = %q", got)
	}
	if got := readAttr(t, filepath.Join(tpg, "attrib", "authentication")); got != "0" {
		t.Fatalf("authentication = %q, want 0 (CHAP off for demo-mode login)", got)
	}
	if got := readAttr(t, filepath.Join(tpg, "enable")); got != "1" {
		t.Fatalf("tpg enable = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(tpg, "np", portalAddr)); err != nil || !fi.IsDir() {
		t.Fatalf("portal missing: %v", err)
	}
	for i, dev := range devs {
		name := fmt.Sprintf("iqn-2026-06-local-ndiskless-client-aabbccddeeff-lun%d", i)
		so, err := os.Readlink(filepath.Join(tpg, "lun", fmt.Sprintf("lun_%d", i), name))
		if err != nil {
			t.Fatalf("lun %d mapping: %v", i, err)
		}
		if !strings.HasPrefix(so, l.corePath()+string(os.PathSeparator)+"iblock_") {
			t.Fatalf("lun %d maps outside core: %s", i, so)
		}
		if got := readAttr(t, filepath.Join(so, "control")); got != "udev_path="+dev {
			t.Fatalf("lun %d control = %q", i, got)
		}
		if got := readAttr(t, filepath.Join(so, "udev_path")); got != dev {
			t.Fatalf("lun %d udev_path = %q", i, got)
		}
		if got := readAttr(t, filepath.Join(so, "enable")); got != "1" {
			t.Fatalf("lun %d enable = %q", i, got)
		}
		if got := readAttr(t, filepath.Join(so, "attrib", "emulate_model_alias")); got != "1" {
			t.Fatalf("lun %d emulate_model_alias = %q", i, got)
		}
		if got := readAttr(t, filepath.Join(so, "attrib", "emulate_tpu")); got != "1" {
			t.Fatalf("lun %d emulate_tpu = %q", i, got)
		}
		if got := readAttr(t, filepath.Join(so, "wwn", "vpd_unit_serial")); len(got) != 36 {
			t.Fatalf("lun %d serial = %q, want uuid shape", i, got)
		}
	}
}

func TestExportBlocksIdempotentReExport(t *testing.T) {
	l := newTestLIO(t)
	reqs := []ExportRequest{{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF"), LUN: 0}}

	if _, err := l.ExportBlocks(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ExportBlocks(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	hbas, err := filepath.Glob(filepath.Join(l.corePath(), "iblock_*"))
	if err != nil || len(hbas) != 1 {
		t.Fatalf("hbas = %v (err %v), want exactly one after re-export", hbas, err)
	}
}

func TestExportBlocksReusesExistingBackstore(t *testing.T) {
	// 超管机重复导出时 backstore 已存在，应原样复用。
	l := newTestLIO(t)
	name := "iqn-2026-06-local-ndiskless-client-aabbccddeeff-lun0"
	so := filepath.Join(l.corePath(), "iblock_7", name)
	if err := os.MkdirAll(so, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(so, "control"), []byte("udev_path=/dev/zvol/tank/OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "NEW"), LUN: 0}}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l.configfsBase, "iqn.2026-06.local.ndiskless:client-aabbccddeeff", "tpgt_1", "lun", "lun_0", name)
	if got, err := os.Readlink(link); err != nil || got != so {
		t.Fatalf("link = %q (err %v), want %q", got, err, so)
	}
	if got := readAttr(t, filepath.Join(so, "control")); got != "udev_path=/dev/zvol/tank/OLD" {
		t.Fatalf("existing backstore was reconfigured: control = %q", got)
	}
	if hbas, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*")); len(hbas) != 1 {
		t.Fatalf("hbas = %v, want only the pre-existing one", hbas)
	}
}

func TestExportBlocksValidates(t *testing.T) {
	l := newTestLIO(t)
	if luns, err := l.ExportBlocks(context.Background(), nil); err != nil || luns != nil {
		t.Fatalf("empty reqs: luns = %v, err = %v", luns, err)
	}
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{{MAC: "", VolPath: "/dev/zvol/tank/X", LUN: 0}}); err == nil {
		t.Fatal("missing MAC accepted")
	}
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{{MAC: "aa", VolPath: "", LUN: 0}}); err == nil {
		t.Fatal("missing VolPath accepted")
	}
}

func TestExportBlocksWaitsForMissingDevice(t *testing.T) {
	// 设备一直不出现时导出必须失败，不能永久挂起。
	l := newTestLIO(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.ExportBlocks(ctx, []ExportRequest{{MAC: "aa", VolPath: filepath.Join(t.TempDir(), "never-appears"), LUN: 0}})
	if err == nil {
		t.Fatal("export succeeded without a device node")
	}
}

func TestExportBlocksConcurrentClients(t *testing.T) {
	// 不同客户机并发分配 HBA 序号不能冲突。
	l := newTestLIO(t)
	const n = 16
	devs := make([]string, n)
	for i := range devs {
		devs[i] = makeDev(t, fmt.Sprintf("CLIENT-%02x", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.ExportBlocks(context.Background(), []ExportRequest{{
				MAC:     fmt.Sprintf("52:54:00:00:00:%02x", i),
				VolPath: devs[i],
				LUN:     0,
			}})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hbas, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*")); len(hbas) != n {
		t.Fatalf("hbas = %d, want %d distinct", len(hbas), n)
	}
	if sos, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", "*")); len(sos) != n {
		t.Fatalf("storage objects = %d, want %d", len(sos), n)
	}
}

func TestExportThenTeardownRemovesEverything(t *testing.T) {
	l := newTestLIO(t)
	target := "iqn.2026-06.local.ndiskless:client-aabbccddeeff"
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF"), LUN: 0},
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF-DATA-1"), LUN: 1},
	}); err != nil {
		t.Fatal(err)
	}

	if err := l.TeardownTarget(context.Background(), target, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.configfsBase, target)); !os.IsNotExist(err) {
		t.Fatalf("target dir still present (err %v)", err)
	}
	if hbas, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*")); len(hbas) != 0 {
		t.Fatalf("backstores remain: %v", hbas)
	}
}

func TestTeardownRemovesCHAPTargetIncludingACLs(t *testing.T) {
	l := newTestLIO(t)
	target := "iqn.2026-06.local.ndiskless:client-aabbccddeeff"
	creds := &CHAPCreds{Username: "nds-aabbccddeeff", Password: "s3cr3tpassw0rd12"}
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "d0"), LUN: 0, CHAP: creds},
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "d1"), LUN: 1, CHAP: creds},
	}); err != nil {
		t.Fatal(err)
	}
	// 拆 CHAP target 必须连 ACL 一起清干净（真机上 ACL 不先清会 EBUSY）；假 configfs 没有 EBUSY，顺序只能靠真机验证。
	if err := l.TeardownTarget(context.Background(), target, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.configfsBase, target)); !os.IsNotExist(err) {
		t.Fatalf("CHAP target dir 没删干净（err %v）", err)
	}
}

func TestTeardownTargetMissingIsNoop(t *testing.T) {
	l := newTestLIO(t)
	if err := l.TeardownTarget(context.Background(), TargetForMAC("aa:bb"), []int{0}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteLUNsLeavesTargetAndOtherLUNs(t *testing.T) {
	l := newTestLIO(t)
	target := "iqn.2026-06.local.ndiskless:client-aabbccddeeff"
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF"), LUN: 0},
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF-DATA-1"), LUN: 1},
	}); err != nil {
		t.Fatal(err)
	}

	if err := l.DeleteLUNs(context.Background(), target, []int{1}); err != nil {
		t.Fatal(err)
	}
	tpg := filepath.Join(l.configfsBase, target, "tpgt_1")
	if _, err := os.Stat(filepath.Join(tpg, "lun", "lun_1")); !os.IsNotExist(err) {
		t.Fatalf("lun_1 still present (err %v)", err)
	}
	if sos, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", "*-lun1")); len(sos) != 0 {
		t.Fatalf("lun1 backstore remains: %v", sos)
	}
	name0 := "iqn-2026-06-local-ndiskless-client-aabbccddeeff-lun0"
	if _, err := os.Readlink(filepath.Join(tpg, "lun", "lun_0", name0)); err != nil {
		t.Fatalf("lun_0 mapping lost: %v", err)
	}

	// 空 LUN 列表是空操作。
	if err := l.DeleteLUNs(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteLUNMissingIsNoop(t *testing.T) {
	l := newTestLIO(t)
	if err := l.DeleteLUNs(context.Background(), TargetForMAC("aa:bb"), []int{0}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTargetLeavesBackstores(t *testing.T) {
	l := newTestLIO(t)
	target := "iqn.2026-06.local.ndiskless:client-aabbccddeeff"
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "CLIENT-AABBCCDDEEFF"), LUN: 0}}); err != nil {
		t.Fatal(err)
	}

	if err := l.DeleteTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.configfsBase, target)); !os.IsNotExist(err) {
		t.Fatalf("target dir still present (err %v)", err)
	}
	if sos, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", "*")); len(sos) != 1 {
		t.Fatalf("storage objects = %v, want the backstore kept", sos)
	}
}

// writeConfigfsTarget 在 base 下伪造一个 target，dynamic_sessions 为空表示无会话。
func writeConfigfsTarget(t *testing.T, base, iqn, dynamicSessions string) {
	t.Helper()
	dir := filepath.Join(base, iqn, "tpgt_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dynamic_sessions"), []byte(dynamicSessions), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActiveSessionsFromConfigfs(t *testing.T) {
	base := t.TempDir()
	// 在线客户机：dynamic_sessions 里是 initiator IQN（与真机布局一致）。
	writeConfigfsTarget(t, base, "iqn.2026-06.local.ndiskless:client-00505625442c",
		"iqn.2026-06.local.ndiskless:initiator-00505625442c")
	// 离线客户机：dynamic_sessions 为空。
	writeConfigfsTarget(t, base, "iqn.2026-06.local.ndiskless:client-00505638bb2c", "")
	// 非客户机条目：discovery_auth 是没有 tpgt_1 的普通目录。
	if err := os.MkdirAll(filepath.Join(base, "discovery_auth"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.configfsBase = base
	macs, err := l.ActiveSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(macs, []string{"00505625442C"}) {
		t.Fatalf("macs = %v, want [00505625442C] (only the connected client)", macs)
	}
}

func TestActiveSessionsNoTargets(t *testing.T) {
	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.configfsBase = t.TempDir()
	macs, err := l.ActiveSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(macs) != 0 {
		t.Fatalf("macs = %v, want empty", macs)
	}
}

func TestActiveSessionsMissingConfigfsErrors(t *testing.T) {
	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.configfsBase = filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := l.ActiveSessions(context.Background()); err == nil {
		t.Fatal("expected an error when configfs is absent (so reconcile skips the cycle)")
	}
}

// target 与 initiator IQN 必须出自同一前缀，否则 iPXE 登录对不上。
func TestTargetAndInitiatorShareTheSameAuthority(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:ff"
	target, initiator := TargetForMAC(mac), InitiatorIQN(mac)
	if target == initiator {
		t.Fatal("the two ends must be distinguishable")
	}
	if target != "iqn.2026-06.local.ndiskless:client-aabbccddeeff" {
		t.Fatalf("target = %q", target)
	}
	if initiator != "iqn.2026-06.local.ndiskless:initiator-aabbccddeeff" {
		t.Fatalf("initiator = %q", initiator)
	}
}

// 序列号由 (target, LUN) 确定：Windows 见到序列号变化会当成换盘蓝屏，接管时必须各节点一致。
func TestUnitSerialIsDeterministicPerTargetAndLUN(t *testing.T) {
	a1 := unitSerial("iqn.2026-06.local.ndiskless:client-aabbcc112233", 0)
	a2 := unitSerial("iqn.2026-06.local.ndiskless:client-aabbcc112233", 0)
	if a1 != a2 {
		t.Fatalf("same disk, different serials: %q vs %q", a1, a2)
	}
	b := unitSerial("iqn.2026-06.local.ndiskless:client-aabbcc112233", 1)
	c := unitSerial("iqn.2026-06.local.ndiskless:client-aabbcc445566", 0)
	if a1 == b || a1 == c {
		t.Fatalf("distinct disks share a serial: %q %q %q", a1, b, c)
	}
	// 保持 rtslib 格式（uuid4、小写十六进制），依赖 targetcli 格式的工具照常可用。
	if len(a1) != 36 || a1[14] != '4' {
		t.Fatalf("serial not uuid4-shaped: %q", a1)
	}
}

// 降为备机时必须删光 ndiskless 的 target 和 backstore，但不能碰其他 LIO 使用者的对象。
func TestTeardownAllRemovesOnlyNdisklessTargets(t *testing.T) {
	l := newTestLIO(t)
	dev := makeDev(t, "zd0")
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:00:00:01", VolPath: dev, LUN: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:00:00:02", VolPath: dev, LUN: 0},
	}); err != nil {
		t.Fatal(err)
	}
	// 同一 fabric 下的外部 target 保持不动
	foreign := filepath.Join(l.configfsBase, "iqn.2000-01.com.example:other", "tpgt_1")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := l.TeardownAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(l.configfsBase)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 1 || left[0] != "iqn.2000-01.com.example:other" {
		t.Fatalf("targets left = %v", left)
	}
	// backstore 也要删掉，它们占着备机必须释放的 zvol
	core, _ := filepath.Glob(filepath.Join(l.corePath(), "iblock_*", "*"))
	var stores []string
	for _, c := range core {
		if b := filepath.Base(c); b != "hba_info" && b != "hba_mode" {
			stores = append(stores, b)
		}
	}
	if len(stores) != 0 {
		t.Fatalf("backstores left = %v", stores)
	}
}

// 新装节点从未导出过，fabric 目录尚未注册；Available 必须用 mkdir 探测，否则健康检查永远让 keepalived 停在 FAULT。
func TestAvailableRegistersFabricByMkdir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "target", "iscsi")
	if !availableAt(base) {
		t.Fatal("mkdir-able fabric dir must read as available")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("probe must leave the fabric dir registered: %v", err)
	}
	// 第二次调用：目录已存在
	if !availableAt(base) {
		t.Fatal("existing fabric dir must read as available")
	}
	// 父路径是文件、无法创建时才算真的不可用
	f := filepath.Join(t.TempDir(), "flat")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if availableAt(filepath.Join(f, "iscsi")) {
		t.Fatal("uncreatable path must read as unavailable")
	}
}

func TestExportBlocksSetsCHAPWhenCredentialsGiven(t *testing.T) {
	l := newTestLIO(t)
	creds := &CHAPCreds{Username: "iqn.initiator", Password: "s3cr3tpassw0rd12"}
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "d"), LUN: 0, CHAP: creds},
	}); err != nil {
		t.Fatal(err)
	}
	tpg := filepath.Join(l.configfsBase, "iqn.2026-06.local.ndiskless:client-aabbccddeeff", "tpgt_1")
	// demo mode 的动态 ACL 不认 TPG 级 auth；CHAP 必须关掉自动 ACL，为 initiator IQN 建显式 ACL 并把 auth 设在 ACL 上。
	if got := readAttr(t, filepath.Join(tpg, "attrib", "generate_node_acls")); got != "0" {
		t.Fatalf("generate_node_acls = %q, want 0（CHAP 下不自动放行）", got)
	}
	if got := readAttr(t, filepath.Join(tpg, "attrib", "authentication")); got != "1" {
		t.Fatalf("authentication = %q, want 1（CHAP 要求认证开）", got)
	}
	// ACL 名必须等于 iPXE 脚本里 set initiator-iqn 的值。
	acl := filepath.Join(tpg, "acls", InitiatorIQN("aabbccddeeff"))
	if got := readAttr(t, filepath.Join(acl, "auth", "userid")); got != creds.Username {
		t.Fatalf("acl userid = %q, want %q", got, creds.Username)
	}
	if got := readAttr(t, filepath.Join(acl, "auth", "password")); got != creds.Password {
		t.Fatalf("acl password = %q, want %q", got, creds.Password)
	}
	// ACL 还必须映射到 LUN，否则认证阶段报 CHAP_N mismatch。
	mlun := filepath.Join(acl, "lun_0")
	entries, err := os.ReadDir(mlun)
	if err != nil || len(entries) == 0 {
		t.Fatalf("ACL 没有 mapped LUN（err=%v）——认证会失败", err)
	}
	// CHAP 逐字节比对，auth 值不能带结尾换行。
	if raw, _ := os.ReadFile(filepath.Join(acl, "auth", "userid")); string(raw) != creds.Username {
		t.Fatalf("userid 有尾随字符 %q，会触发 CHAP_N mismatch", string(raw))
	}
	if raw, _ := os.ReadFile(filepath.Join(acl, "auth", "password")); string(raw) != creds.Password {
		t.Fatalf("password 有尾随字符 %q", string(raw))
	}
}

func TestExportBlocksStaysDemoModeWithoutCHAP(t *testing.T) {
	l := newTestLIO(t)
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: "aa:bb:cc:dd:ee:ff", VolPath: makeDev(t, "d"), LUN: 0}, // CHAP 为 nil
	}); err != nil {
		t.Fatal(err)
	}
	tpg := filepath.Join(l.configfsBase, "iqn.2026-06.local.ndiskless:client-aabbccddeeff", "tpgt_1")
	// 无凭据时保持 demo mode：自动 ACL、关闭认证，升级不影响现有部署。
	if got := readAttr(t, filepath.Join(tpg, "attrib", "generate_node_acls")); got != "1" {
		t.Fatalf("generate_node_acls = %q, want 1（demo mode 自动放行）", got)
	}
	if got := readAttr(t, filepath.Join(tpg, "attrib", "authentication")); got != "0" {
		t.Fatalf("authentication = %q, want 0（无凭据保持 demo mode）", got)
	}
}

// CHAP 的会话不在 dynamic_sessions，而在 acls/<initiator-iqn>/info；漏看会把在线客户机判为离线，
// 进而被离线克隆回收删掉系统盘。info 无会话时以 "No active iSCSI Session" 开头。
func writeCHAPTarget(t *testing.T, base, target, initiator, info string) {
	t.Helper()
	acl := filepath.Join(base, target, "tpgt_1", "acls", initiator)
	if err := os.MkdirAll(acl, 0o755); err != nil {
		t.Fatal(err)
	}
	// CHAP 模式下这个文件为空
	if err := os.WriteFile(filepath.Join(base, target, "tpgt_1", "dynamic_sessions"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acl, "info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActiveSessionsSeesCHAPSessionsUnderExplicitACLs(t *testing.T) {
	base := t.TempDir()
	// 已登录：info 里是会话详情
	writeCHAPTarget(t, base, "iqn.2026-06.local.ndiskless:client-0050562ef64f",
		"iqn.2026-06.local.ndiskless:initiator-0050562ef64f",
		"InitiatorName: iqn.2026-06.local.ndiskless:initiator-0050562ef64f\nInitiatorAlias: win11\nLIO Session ID: 3  ISID: 0x00023d000001  TSIH: 3  SessionType: Normal\nSession State: TARG_SESS_STATE_LOGGED_IN\n")
	// 未登录：info 明说没有会话
	writeCHAPTarget(t, base, "iqn.2026-06.local.ndiskless:client-00505638bb2c",
		"iqn.2026-06.local.ndiskless:initiator-00505638bb2c",
		"No active iSCSI Session for Initiator Endpoint: iqn.2026-06.local.ndiskless:initiator-00505638bb2c\n")
	// demo 模式的老路径仍然要认
	writeConfigfsTarget(t, base, "iqn.2026-06.local.ndiskless:client-00505625442c",
		"iqn.2026-06.local.ndiskless:initiator-00505625442c")

	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.configfsBase = base
	macs, err := l.ActiveSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(macs)
	want := []string{"0050562EF64F", "00505625442C"}
	sort.Strings(want)
	if !reflect.DeepEqual(macs, want) {
		t.Fatalf("macs = %v, want %v（CHAP 的已登录客户机必须被认出来）", macs, want)
	}
}

// 摘一块数据盘的 LUN 只能解绑该 LUN 在 ACL 里的映射，不能删 ACL：
// CHAP 下删 ACL 会断开系统盘会话，在线发布时客户机会蓝屏。
func TestDeleteLUNsKeepsTheACLSoTheSessionSurvives(t *testing.T) {
	l := newTestLIO(t)
	mac := "aa:bb:cc:dd:ee:ff"
	target := TargetForMAC(mac)
	creds := CHAPCredentials("s3cret", mac)
	if _, err := l.ExportBlocks(context.Background(), []ExportRequest{
		{MAC: mac, VolPath: makeDev(t, "SCLIENT-AABBCCDDEEFF"), LUN: 0, CHAP: &creds},
		{MAC: mac, VolPath: makeDev(t, "SCLIENT-AABBCCDDEEFF-DATA-1"), LUN: 1, CHAP: &creds},
	}); err != nil {
		t.Fatal(err)
	}
	acl := filepath.Join(l.configfsBase, target, "tpgt_1", "acls", InitiatorIQN(mac))
	if _, err := os.Stat(filepath.Join(acl, "lun_1")); err != nil {
		t.Fatalf("前置：ACL 里应当有 lun_1 的映射：%v", err)
	}

	if err := l.DeleteLUNs(context.Background(), target, []int{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acl); err != nil {
		t.Fatalf("ACL 被清掉了，会话会断：%v", err)
	}
	if _, err := os.Stat(filepath.Join(acl, "lun_0")); err != nil {
		t.Fatalf("系统盘的映射被牵连删掉了：%v", err)
	}
	if _, err := os.Stat(filepath.Join(acl, "lun_1")); !os.IsNotExist(err) {
		t.Fatalf("要摘的 lun_1 映射还在（err %v）", err)
	}
}
