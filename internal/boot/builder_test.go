package boot

import (
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
)

func TestBuildWindowsScriptOrder(t *testing.T) {
	script, err := (Builder{}).Build(
		domain.Terminal{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10"},
		domain.Group{Netmask: "255.255.255.0"},
		domain.Image{OSType: domain.OSTypeWindows},
		storage.LUNInfo{
			Server: "10.0.0.2",
			Target: "iqn.2026-06.local.ndiskless:client-aabbccddeeff",
			System: storage.LUN{LUN: 0},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		"#!ipxe",
		"set keep-san 1",
		"set initiator-iqn iqn.2026-06.local.ndiskless:initiator-aabbccddeeff",
		"set nd-gateway ${netX/gateway}",
		"set netX/gateway 0.0.0.0",
		"sanhook --drive 0x80 iscsi:10.0.0.2:::0:iqn.2026-06.local.ndiskless:client-aabbccddeeff || goto nd-hook-failed",
		"sanboot --drive 0x80 || goto nd-boot-failed",
		"exit",
		":nd-hook-failed",
		"set nd-errno ${errno}",
		"set nd-stage sanhook",
		"goto nd-failed",
		":nd-boot-failed",
		"set nd-errno ${errno}",
		"set nd-stage sanboot",
		":nd-failed",
		"set netX/gateway ${nd-gateway}",
		"echo Boot failed at ${nd-stage} (error ${nd-errno}), reported to the server",
		"imgfetch /boot/failed?mac=${net0/mac}&stage=${nd-stage}&err=${nd-errno}&platform=${platform} || echo Could not reach the server",
		"sleep 30",
		"exit",
	}
	if got := strings.TrimSpace(script); got != strings.Join(wantLines, "\n") {
		t.Fatalf("script:\n%s", script)
	}

	gateway := strings.Index(script, "set netX/gateway 0.0.0.0")
	sanhook := strings.Index(script, "sanhook --drive 0x80")
	if gateway < 0 || sanhook < 0 || gateway > sanhook {
		t.Fatalf("gateway index=%d sanhook index=%d", gateway, sanhook)
	}
	if strings.Count(script, "sanhook --drive 0x80") != 1 {
		t.Fatalf("sanhook count = %d", strings.Count(script, "sanhook --drive 0x80"))
	}
	if strings.Contains(script, "purenic") {
		t.Fatalf("standard iPXE script must not use purenic:\n%s", script)
	}
}

func TestBuildLinuxScriptSkipsGatewayClear(t *testing.T) {
	script, err := (Builder{}).Build(
		domain.Terminal{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10"},
		domain.Group{Netmask: "255.255.255.0"},
		domain.Image{OSType: domain.OSTypeLinux},
		storage.LUNInfo{Server: "10.0.0.2", Target: "target", System: storage.LUN{LUN: 0}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "set netX/gateway 0.0.0.0") {
		t.Fatalf("linux script cleared gateway:\n%s", script)
	}
	if strings.Count(script, "sanhook --drive 0x80") != 1 {
		t.Fatalf("sanhook count = %d", strings.Count(script, "sanhook --drive 0x80"))
	}
	if !strings.Contains(script, "sanhook --drive 0x80 iscsi:10.0.0.2:::0:target") {
		t.Fatalf("script = %s", script)
	}
	if strings.Contains(script, "gateway") {
		t.Fatalf("linux script touched the gateway:\n%s", script)
	}
	if !strings.Contains(script, "sanboot --drive 0x80 || goto nd-boot-failed") ||
		!strings.Contains(script, "imgfetch /boot/failed?mac=${net0/mac}&stage=${nd-stage}") {
		t.Fatalf("linux script has no failure report:\n%s", script)
	}
}

func TestBuildEmitsCHAPWhenSecretSet(t *testing.T) {
	b := Builder{CHAPSecret: "deployment-secret"}
	script, err := b.Build(
		domain.Terminal{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10"},
		domain.Group{Netmask: "255.255.255.0"},
		domain.Image{OSType: domain.OSTypeWindows},
		storage.LUNInfo{Server: "10.0.0.2", Target: "iqn.t:client-aabbccddeeff", System: storage.LUN{LUN: 0}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 脚本里的凭据必须与导出端由同一 secret 和 MAC 派生，否则登录被拒。
	want := iscsi.CHAPCredentials("deployment-secret", "AA:BB:CC:DD:EE:FF")
	if !strings.Contains(script, "set username "+want.Username) {
		t.Fatalf("脚本缺 username：\n%s", script)
	}
	if !strings.Contains(script, "set password "+want.Password) {
		t.Fatalf("脚本缺 password：\n%s", script)
	}
	// 认证必须在 sanhook 之前设好。
	if u := strings.Index(script, "set username"); u < 0 || u > strings.Index(script, "sanhook") {
		t.Fatalf("username 没排在 sanhook 之前：\n%s", script)
	}
}

func TestBuildNoCHAPLinesWithoutSecret(t *testing.T) {
	script, err := (Builder{}).Build( // 没有 secret 即 demo mode
		domain.Terminal{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10"},
		domain.Group{Netmask: "255.255.255.0"},
		domain.Image{OSType: domain.OSTypeWindows},
		storage.LUNInfo{Server: "10.0.0.2", Target: "iqn.t:client-aabbccddeeff", System: storage.LUN{LUN: 0}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "set username") || strings.Contains(script, "set password") {
		t.Fatalf("demo mode 不该有 CHAP 行：\n%s", script)
	}
}
