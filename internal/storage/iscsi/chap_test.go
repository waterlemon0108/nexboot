package iscsi

import (
	"strings"
	"testing"
)

// 凭据按 MAC 派生：不同 MAC 得到不同密码，MAC 写法不同结果相同。
func TestCHAPCredentialsAreDerivedPerMAC(t *testing.T) {
	a := CHAPCredentials("secret-x", "aa:bb:cc:dd:ee:ff")
	b := CHAPCredentials("secret-x", "11:22:33:44:55:66")
	if a.Password == b.Password {
		t.Fatalf("两台不同 MAC 派生出了同一个密码：%q", a.Password)
	}
	// dnsmasq 与 iPXE 的 MAC 写法未必统一，大小写和分隔符不能影响结果。
	if got := CHAPCredentials("secret-x", "AABBCCDDEEFF"); got.Password != a.Password {
		t.Fatalf("MAC 归一化没做：%q vs %q", got.Password, a.Password)
	}
}

// 换 secret 后凭据必须变，否则轮换无效。
func TestCHAPPasswordDependsOnSecret(t *testing.T) {
	a := CHAPCredentials("secret-x", "aa:bb:cc:dd:ee:ff")
	b := CHAPCredentials("secret-y", "aa:bb:cc:dd:ee:ff")
	if a.Password == b.Password {
		t.Fatalf("换了 secret 密码没变：%q", a.Password)
	}
}

// 密码长度必须在 12–16 之间（RFC 3720 下限 12，Windows initiator 上限 16），否则登录失败。
func TestCHAPPasswordLengthFitsInitiators(t *testing.T) {
	c := CHAPCredentials("secret-x", "aa:bb:cc:dd:ee:ff")
	if n := len(c.Password); n < 12 || n > 16 {
		t.Fatalf("密码长度 %d 落在 12–16 之外：%q", n, c.Password)
	}
	if c.Username == "" {
		t.Fatalf("用户名为空")
	}
	// 用户名带冒号会导致 "CHAP_N values do not match"。
	if strings.ContainsAny(c.Username, ": ") {
		t.Fatalf("用户名含冒号或空格，会触发 CHAP_N mismatch：%q", c.Username)
	}
}

// 空 secret 不启用 CHAP，不能用空 secret 派生人人可算的假凭据。
func TestCHAPEmptySecretMeansNoCredentials(t *testing.T) {
	if CHAPEnabled("") {
		t.Fatalf("空 secret 不该算启用 CHAP")
	}
	if !CHAPEnabled("something") {
		t.Fatalf("有 secret 就该启用")
	}
}
