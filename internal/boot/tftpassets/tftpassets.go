// Package tftpassets 内嵌下发给 PXE 客户机的 iPXE/TFTP 启动文件，使单个二进制自给自足。
//
// 不能用发行版 `ipxe` 包的文件：其 snponly.efi 内嵌了 `chain http://${next-server}/menu.ipxe?mac=${mac}`，
// 会无视 DHCP option 67 下发的启动文件名。这里的文件不含内嵌脚本。
// undionly.kpxe 是压缩的，grep 证明不了什么；用 scripts/build-undionly.sh 重建。
package tftpassets

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed files/snponly.efi files/undionly.kpxe files/wimboot
var files embed.FS

// Names 列出内嵌文件，与 dnsmasq Renderer 在 TFTPRoot 下期望的一致
// （BIOS 用 undionly.kpxe，UEFI 用 snponly.efi，wimboot 用于 WinPE 离线适配）。
var Names = []string{"snponly.efi", "undionly.kpxe", "wimboot"}

// WriteTo 把内嵌启动文件写到 dir 并覆盖已有文件。每次启动都调用，防止同路径的发行版文件悄悄顶替。
func WriteTo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tftpassets: create %s: %w", dir, err)
	}
	for _, name := range Names {
		data, err := fs.ReadFile(files, "files/"+name)
		if err != nil {
			return fmt.Errorf("tftpassets: read embedded %s: %w", name, err)
		}
		dst := filepath.Join(dir, name)
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return fmt.Errorf("tftpassets: write %s: %w", dst, err)
		}
	}
	return nil
}
