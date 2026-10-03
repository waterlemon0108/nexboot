package config

import (
	"os"
	"strings"
	"testing"
)

// 节点身份与 portal 地址是两回事：id 是本机在库里的名字（稳定，改地址不变），portal 是客户机访问磁盘的地址（HA 下为 VIP）。
// 未设置时 portal 回落到启动 URL 的主机名，id 回落到 machine-id。
func TestLoadSplitsNodeIdentityFromPortal(t *testing.T) {
	for _, key := range []string{"NDISKLESS_NODE_ID", "NDISKLESS_PORTAL_ADDR", "NDISKLESS_ROLE", "NDISKLESS_BOOT_URL"} {
		t.Setenv(key, "")
	}
	t.Setenv("NDISKLESS_BOOT_URL", "http://192.168.50.1:8080/boot?mac=${net0/mac}")

	cfg := Load(nil)
	if cfg.PortalAddr != "192.168.50.1" {
		t.Fatalf("portal = %q, want the boot URL host", cfg.PortalAddr)
	}
	if cfg.NodeID == "" || cfg.NodeID == "192.168.50.1" {
		t.Fatalf("node id = %q, want a machine-derived identity", cfg.NodeID)
	}
	if cfg.Role != "all" {
		t.Fatalf("role = %q", cfg.Role)
	}

	t.Setenv("NDISKLESS_NODE_ID", "node-a")
	t.Setenv("NDISKLESS_PORTAL_ADDR", "192.168.50.250")
	t.Setenv("NDISKLESS_ROLE", "all")
	cfg = Load(nil)
	if cfg.NodeID != "node-a" || cfg.PortalAddr != "192.168.50.250" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// machineNodeID 多次调用必须一致，且没有 /etc/machine-id 时也不为空（回落到主机名）。
func TestMachineNodeIDStableAndNonEmpty(t *testing.T) {
	a, b := machineNodeID(), machineNodeID()
	if a == "" || a != b {
		t.Fatalf("ids = %q, %q", a, b)
	}
	if strings.ContainsAny(a, " \t\n") {
		t.Fatalf("id has whitespace: %q", a)
	}
	if host, _ := os.Hostname(); host == "" && a == "" {
		t.Fatal("unreachable")
	}
}
