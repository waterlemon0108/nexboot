package boot

import (
	"fmt"
	"strings"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
)

type BootBuilder interface {
	Build(domain.Terminal, domain.Group, domain.Image, storage.LUNInfo) (string, error)
}

// Builder 生成 iPXE 启动脚本。CHAPSecret 非空时写入按 MAC 派生的 iSCSI 登录凭据，
// 与导出端用同一密钥派生；为空即 demo mode，不写登录行。
type Builder struct{ CHAPSecret string }

func (b Builder) Build(t domain.Terminal, g domain.Group, img domain.Image, lun storage.LUNInfo) (string, error) {
	if img.OSType != domain.OSTypeWindows && img.OSType != domain.OSTypeLinux {
		return "", fmt.Errorf("unsupported os type %q", img.OSType)
	}
	target := lun.Target
	if target == "" {
		target = lun.System.Target
	}
	if t.MAC == "" || t.IP == "" || g.Netmask == "" || lun.Server == "" || target == "" {
		return "", fmt.Errorf("terminal mac/ip, group netmask, server and target are required")
	}

	lines := []string{
		"#!ipxe",
		"set keep-san 1",
		"set initiator-iqn " + iscsi.InitiatorIQN(t.MAC),
	}
	windows := img.OSType == domain.OSTypeWindows
	if windows {
		// 失败回报要走网关才能到跨网段的服务器，先记下再清。
		lines = append(lines, "set nd-gateway ${netX/gateway}", "set netX/gateway 0.0.0.0")
	}
	// CHAP 必须在 sanhook 之前设置：iPXE 在 sanhook 登录时读取 username/password。
	if iscsi.CHAPEnabled(b.CHAPSecret) {
		creds := iscsi.CHAPCredentials(b.CHAPSecret, t.MAC)
		lines = append(lines, "set username "+creds.Username, "set password "+creds.Password)
	}
	lines = append(lines,
		fmt.Sprintf("sanhook --drive 0x80 iscsi:%s:::%d:%s || goto nd-hook-failed", lun.Server, lun.System.LUN, target),
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
	)
	if windows {
		lines = append(lines, "set netX/gateway ${nd-gateway}")
	}
	// 相对地址落回下发本脚本的那台服务器。
	lines = append(lines,
		"echo Boot failed at ${nd-stage} (error ${nd-errno}), reported to the server",
		"imgfetch /boot/failed?mac=${net0/mac}&stage=${nd-stage}&err=${nd-errno}&platform=${platform} || echo Could not reach the server",
		"sleep 30",
		"exit",
	)
	return strings.Join(lines, "\n") + "\n", nil
}
