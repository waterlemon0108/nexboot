package ha

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

// JoinParams 的所有值都会进入一个以 root 运行、写 env 文件和 keepalived 配置的脚本的命令行。
// 脚本中途失败时集群设置已写了一半，节点会以没有 keepalived 的备机回来、控制台又拒绝再次加入，
// 所以在动任何东西之前先在这里校验。

var (
	poolNameOK = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*$`)
	tokenOK    = regexp.MustCompile(`^[A-Za-z0-9._:+=@-]+$`)
)

func validIPv4(s string) bool {
	addr, err := netip.ParseAddr(s)
	return err == nil && addr.Is4()
}

// validateVIP 单独先校验：其余参数还没得到之前就要先连 VIP。
func validateVIP(vip string) error {
	if !validIPv4(vip) {
		return fmt.Errorf("虚 IP「%s」不是合法的 IPv4 地址，请只填地址本身（不带掩码和端口），例如 192.168.10.250", shown(vip))
	}
	return nil
}

func (p JoinParams) validate() error {
	if err := validateVIP(p.VIP); err != nil {
		return err
	}
	if !validIPv4(p.NodeAddr) {
		return fmt.Errorf("本机地址「%s」不是合法的 IPv4 地址，请检查本机在客户机网段上的网卡配置", shown(p.NodeAddr))
	}
	for _, peer := range p.Peers {
		if !validIPv4(peer) {
			return fmt.Errorf("集群返回的节点地址「%s」不是合法的 IPv4 地址，本机未做任何改动；"+
				"请在集群的节点列表里核对这台节点的地址", shown(peer))
		}
	}
	if p.Pool != "" && !poolNameOK.MatchString(p.Pool) {
		return fmt.Errorf("池名「%s」不可用：只能用字母、数字和 _ - . :，并以字母开头", shown(p.Pool))
	}
	if !tokenOK.MatchString(p.Token) {
		return fmt.Errorf("集群令牌含有不允许的字符（只能用字母、数字和 . _ - : + = @），请核对是否多复制了内容")
	}
	for name, secret := range map[string]string{"登录签名密钥": p.JWTSecret, "CHAP 密钥": p.CHAPSecret} {
		if strings.IndexFunc(secret, unicode.IsControl) >= 0 {
			return fmt.Errorf("集群返回的%s里含有换行等控制字符，本机未做任何改动；请在主机上检查该密钥的配置", name)
		}
	}
	return nil
}

// shown 把被拒绝的值处理成可安全放进单行提示的形式。
func shown(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '␣'
		}
		return r
	}, s)
}
