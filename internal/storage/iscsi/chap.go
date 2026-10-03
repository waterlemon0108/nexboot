package iscsi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// CHAPCreds 是一台客户机的 iSCSI CHAP 登录凭据，只派生不存储，见 CHAPCredentials。
type CHAPCreds struct {
	Username string
	Password string
}

// chapPasswordLen：RFC 3720 要求至少 12 字节，Windows 自带 initiator 上限 16。
const chapPasswordLen = 16

// CHAPCredentials 用部署密钥对 MAC 做 HMAC 派生凭据：MAC 是公开的，HMAC 防止反推密钥；
// 每 MAC 一份，泄露只影响一台。无盘客户机拿到盘前没有任何存储，凭据只能由服务端按 MAC 重算、经 /boot 下发。
func CHAPCredentials(secret, mac string) CHAPCreds {
	norm := storage.NormalizeMAC(mac)
	mac12 := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(norm, ":", ""), "-", ""))
	sum := hmac.New(sha256.New, []byte(secret))
	sum.Write([]byte("chap-password:" + mac12))
	// 用无填充 base32：避免 base64 的特殊字符破坏 iPXE 脚本行。
	pw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil))
	if len(pw) > chapPasswordLen {
		pw = pw[:chapPasswordLen]
	}
	// 用户名无需保密，但两端必须逐字节一致。不能用 initiator IQN：冒号和长度会导致
	// "CHAP_N values do not match"，所以用不含冒号的短名。
	return CHAPCreds{Username: "nds-" + mac12, Password: pw}
}

// CHAPEnabled 报告部署是否启用 iSCSI 登录认证。没有密钥就走 demo mode，不能用空密钥派生人人可算的凭据。
func CHAPEnabled(secret string) bool { return secret != "" }
