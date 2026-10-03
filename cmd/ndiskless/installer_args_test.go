package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runInstaller(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash，跳过安装脚本的参数校验")
	}
	var out bytes.Buffer
	cmd := exec.Command(bash, append([]string{"../../scripts/install-go.sh"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

// 安装脚本先写集群配置、后生成 keepalived 配置；参数必须在检查 root、写任何文件之前就校验，否则地址错时半截配置已落盘。
func TestInstallerRefusesMalformedClusterArguments(t *testing.T) {
	base := []string{"--bootstrap-password", "x", "--cluster-token", "tok-1"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"虚 IP 带掩码", []string{"--vip", "192.168.10.250/24"}, "--vip"},
		{"虚 IP 某段超过 255", []string{"--vip", "192.168.10.256"}, "--vip"},
		{"对端不是地址", []string{"--vip", "192.168.10.250", "--peer", "192.168.10.3,192.168.10.4|e"}, "--peer"},
		{"对端带掩码", []string{"--vip", "192.168.10.250", "--peer", "192.168.10.3/24"}, "--peer"},
		{"本机地址不是地址", []string{"--vip", "192.168.10.250", "--node-addr", "node-a"}, "--node-addr"},
		{"池名带空格", []string{"--vip", "192.168.10.250", "--pool", "nd pool"}, "--pool"},
		{"令牌带斜杠", []string{"--vip", "192.168.10.250", "--cluster-token", "abc/def"}, "--cluster-token"},
		{"密钥带换行", []string{"--vip", "192.168.10.250", "--jwt-secret", "a\nNDISKLESS_ROLE=active"}, "--jwt-secret"},
		{"角色写错", []string{"--vip", "192.168.10.250", "--ha-role", "master"}, "--ha-role"},
	}
	for _, c := range cases {
		out, err := runInstaller(t, append(append([]string{}, base...), c.args...)...)
		if err == nil || !strings.Contains(out, c.want) || strings.Contains(out, "run as root") {
			t.Errorf("%s：应在动手之前拒绝并点名 %s，得到 err=%v 输出=%q", c.name, c.want, err, out)
		}
	}
}

// 合法参数不能被误拒：应通过校验走到下一步（非 root 时停在权限检查）。
func TestInstallerAcceptsWellFormedClusterArguments(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时脚本会真的开始安装，跳过")
	}
	out, err := runInstaller(t, "--bootstrap-password", "x", "--vip", "192.168.10.250",
		"--peer", "192.168.10.3, 192.168.10.4", "--node-addr", "192.168.10.5", "--pool", "nd-pool_1",
		"--cluster-token", "nd-e2e-cluster-2026", "--jwt-secret", "S3cret!@#$%^&*()", "--ha-role", "standby")
	if err == nil || !strings.Contains(out, "run as root") {
		t.Fatalf("合法参数应通过校验、停在权限检查，得到 err=%v 输出=%q", err, out)
	}
}
