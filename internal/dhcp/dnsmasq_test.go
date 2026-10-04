package dhcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
)

func TestRenderDnsmasqConfig(t *testing.T) {
	got, err := testRenderer().Render([]GroupConfig{testGroupConfig()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"enable-tftp",
		"tftp-root=/srv/tftp",
		"dhcp-match=set:bios,option:client-arch,0",
		"dhcp-match=set:efi-x86_64,option:client-arch,7",
		"dhcp-match=set:efi-x86_64,option:client-arch,9",
		"dhcp-boot=tag:bios,undionly.kpxe",
		"dhcp-boot=tag:!bios,snponly.efi",
		"dhcp-range=set:grp-1,192.168.1.10,192.168.1.19,255.255.255.0,12h",
		"dhcp-option=tag:grp-1,option:router,192.168.1.1",
		"dhcp-option=tag:grp-1,option:dns-server,8.8.8.8,1.1.1.1",
		"dhcp-host=AA:BB:CC:DD:EE:FF,set:grp-1,192.168.1.10",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
}

func TestRenderWithoutBootURLUsesArchitectureBootFiles(t *testing.T) {
	got, err := testRenderer().Render([]GroupConfig{testGroupConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "dhcp-userclass") || strings.Contains(got, "tag:ipxe") {
		t.Fatalf("unexpected iPXE chainload directives:\n%s", got)
	}
}

func TestRenderWithBootURLAddsIPXEChainload(t *testing.T) {
	renderer := testRenderer()
	renderer.BootURL = "http://192.168.1.5:8080/boot?mac=${net0/mac}"

	got, err := renderer.Render([]GroupConfig{testGroupConfig()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dhcp-userclass=set:ipxe,iPXE",
		"dhcp-boot=tag:ipxe,http://192.168.1.5:8080/boot?mac=${net0/mac}",
		"dhcp-boot=tag:!ipxe,tag:bios,undionly.kpxe",
		"dhcp-boot=tag:!ipxe,tag:!bios,snponly.efi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dhcp-boot=undionly.kpxe\n") {
		t.Fatalf("plain dhcp-boot line should not remain when BootURL is set:\n%s", got)
	}
}

func TestRenderRejectsTerminalOutsideRange(t *testing.T) {
	cfg := testGroupConfig()
	cfg.Terminals[0].IP = "192.168.1.30"

	if _, err := testRenderer().Render([]GroupConfig{cfg}); err == nil {
		t.Fatal("outside-range terminal accepted")
	}
}

func TestSyncDoesNotOverwriteWhenValidationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dnsmasq.conf")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{errAt: 1, err: errors.New("bad config"), out: []byte("syntax error")}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}

	err := manager.Sync(context.Background(), []GroupConfig{testGroupConfig()})
	if err == nil {
		t.Fatal("sync succeeded")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "old" {
		t.Fatalf("config overwritten: %q", data)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "dnsmasq" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

// 配置变更必须 restart：reload 即 SIGHUP，dnsmasq 不会重读配置文件（man：「SIGHUP does NOT re-read the configuration file」）。
func TestSyncRestartsDnsmasqBecauseReloadDoesNotRereadTheConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	runner := &fakeRunner{}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}

	if err := manager.Sync(context.Background(), []GroupConfig{testGroupConfig()}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "dhcp-host=AA:BB:CC:DD:EE:FF") {
		t.Fatalf("written config = %s", data)
	}
	// 先清计数再重启：systemd 默认 10 秒内最多启动 5 次，超限后 dnsmasq 拒绝启动。
	want := [][]string{{"reset-failed", "dnsmasq"}, {"restart", "dnsmasq"}}
	if len(runner.calls) != 1+len(want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for i, args := range want {
		got := runner.calls[i+1]
		if got.name != "systemctl" || len(got.args) != 2 || got.args[0] != args[0] || got.args[1] != args[1] {
			t.Fatalf("第 %d 条想要 systemctl %v，得到 %s %v", i+1, args, got.name, got.args)
		}
	}
}

// 清计数失败（如系统没有该子命令）不能挡住重启。
func TestSyncRestartsEvenIfClearingTheFailureCounterFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	runner := &fakeRunner{errAt: 2, err: errors.New("unknown command"), out: []byte("nope")}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}

	if err := manager.Sync(context.Background(), []GroupConfig{testGroupConfig()}); err != nil {
		t.Fatalf("清计数失败不该让整次同步失败：%v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if last.name != "systemctl" || last.args[0] != "restart" {
		t.Fatalf("重启没有照常发出：%#v", runner.calls)
	}
}

// 内容没变就不重启 dnsmasq，免得无故打断正在 PXE 的机器。
func TestSyncLeavesDnsmasqAloneWhenNothingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	runner := &fakeRunner{}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}
	groups := []GroupConfig{testGroupConfig()}

	if err := manager.Sync(context.Background(), groups); err != nil {
		t.Fatal(err)
	}
	first := len(runner.calls)
	if err := manager.Sync(context.Background(), groups); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != first {
		t.Fatalf("内容没变却又动了 dnsmasq：%#v", runner.calls[first:])
	}
	// 跳过的只是重写和重启，文件内容仍须正确。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "dhcp-host=AA:BB:CC:DD:EE:FF") {
		t.Fatalf("config = %s", data)
	}
}

func testRenderer() Renderer {
	return Renderer{TFTPRoot: "/srv/tftp", BootFile: "undionly.kpxe", EFIBootFile: "snponly.efi"}
}

func testGroupConfig() GroupConfig {
	return GroupConfig{
		Group: domain.Group{
			ID:        "grp-1",
			Name:      "default",
			StartIP:   "192.168.1.10",
			ClientMax: 10,
			Gateway:   "192.168.1.1",
			Netmask:   "255.255.255.0",
			DNS1:      "8.8.8.8",
			DNS2:      "1.1.1.1",
		},
		Terminals: []domain.Terminal{
			{MAC: "AABBCCDDEEFF", IP: "192.168.1.10"},
		},
	}
}

type fakeRunner struct {
	calls []fakeCall
	errAt int
	out   []byte
	err   error
	// outputs 按 "name arg arg" 指定某次调用的输出，供依赖探测命令输出的用例使用。
	outputs map[string][]byte
}

// ran 报告是否发生过指定的 "name arg arg" 调用。
func (r *fakeRunner) ran(want string) bool {
	for _, c := range r.calls {
		if strings.Join(append([]string{c.name}, c.args...), " ") == want {
			return true
		}
	}
	return false
}

type fakeCall struct {
	name string
	args []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
	if r.errAt == len(r.calls) {
		return r.out, r.err
	}
	if out, ok := r.outputs[strings.Join(append([]string{name}, args...), " ")]; ok {
		return out, nil
	}
	return nil, nil
}

// 只有有 dhcp-host 保留的 MAC 能拿到地址，未登记机器不能抢占之后要分给客户机的地址。
// 用 dhcp-ignore=tag:!known 而不是 dhcp-range 的 `static`（static 会替代结束地址，两者并列是语法错误）。
// 语法是否合法只有真 dnsmasq 能判，见 tools/e2e/ip_matrix.py。
func TestRenderServesOnlyRegisteredMACs(t *testing.T) {
	out, err := Renderer{TFTPRoot: "/srv/tftp", BootFile: "undionly.kpxe"}.Render([]GroupConfig{{
		Group: domain.Group{
			ID: "g-1", Name: "一班", StartIP: "192.168.10.10", ClientMax: 30,
			Gateway: "192.168.10.1", Netmask: "255.255.255.0", DNS1: "223.5.5.5",
		},
		Terminals: []domain.Terminal{{MAC: "AABBCCDDEE01", IP: "192.168.10.11"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var rangeLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "dhcp-range=") {
			rangeLine = line
		}
	}
	if rangeLine == "" {
		t.Fatalf("no dhcp-range in:\n%s", out)
	}
	if !strings.Contains(out, "dhcp-ignore=tag:!known") {
		t.Fatalf("unregistered machines can still take a lease over the reservations:\n%s", out)
	}
	// 区间和逐台保留都必须保留下来。
	if !strings.Contains(rangeLine, "192.168.10.10,192.168.10.39") {
		t.Fatalf("range = %q", rangeLine)
	}
	if !strings.Contains(out, "dhcp-host=AA:BB:CC:DD:EE:01,set:") {
		t.Fatalf("reservation missing:\n%s", out)
	}
}

// 备机上配置文件仍跟随数据库更新，但不重启 dnsmasq（两台权威 DHCP 会互相 NAK）：RestartGate 拒绝时只写文件。
func TestSyncSkipsRestartWhenGateRefuses(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeRunner{}
	m := Manager{
		Path:        filepath.Join(dir, "ndiskless.conf"),
		Runner:      runner,
		Renderer:    Renderer{TFTPRoot: "/srv/tftp", BootFile: "undionly.kpxe", EFIBootFile: "snponly.efi", BootURL: "http://x/boot"},
		RestartGate: func() error { return errors.New("本节点为备机") },
	}
	if err := m.Sync(context.Background(), []GroupConfig{testGroupConfig()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Path); err != nil {
		t.Fatalf("config file must still be written: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.args, " "), "restart") {
			t.Fatalf("restart ran through a refusing gate: %#v", runner.calls)
		}
	}

	// 被跳过的重启由激活流程负责；这里只要求闸门放开后，下一次真实变更会重启。
	m.RestartGate = nil
	changed := testGroupConfig()
	changed.Group.ClientMax = 11
	if err := m.Sync(context.Background(), []GroupConfig{changed}); err != nil {
		t.Fatal(err)
	}
	restarted := false
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.args, " "), "restart") {
			restarted = true
		}
	}
	if !restarted {
		t.Fatal("restart must run once the gate lifts")
	}
}

// HA 下 dnsmasq 被 disable，开机不自启，必须由主节点启动，否则主节点重启后客户机拿不到 DHCP。
func TestEnsureRunningStartsDnsmasqWhenItIsDown(t *testing.T) {
	r := &fakeRunner{outputs: map[string][]byte{"systemctl is-active dnsmasq": []byte("inactive\n")}}
	m := Manager{Runner: r, Logger: nil}
	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.ran("systemctl start dnsmasq") && !r.ran("systemctl restart dnsmasq") {
		t.Fatalf("dnsmasq was never started: %v", r.calls)
	}
}

// 已在运行时不重启，免得打断进行中的 DHCP 交互。
func TestEnsureRunningLeavesARunningDnsmasqAlone(t *testing.T) {
	r := &fakeRunner{outputs: map[string][]byte{"systemctl is-active dnsmasq": []byte("active\n")}}
	m := Manager{Runner: r}
	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.ran("systemctl start dnsmasq") || r.ran("systemctl restart dnsmasq") {
		t.Fatalf("running dnsmasq must not be bounced: %v", r.calls)
	}
}

// 备机绝不能启动 dnsmasq。
func TestEnsureRunningRefusesWhenGateIsClosed(t *testing.T) {
	r := &fakeRunner{outputs: map[string][]byte{"systemctl is-active dnsmasq": []byte("inactive\n")}}
	m := Manager{Runner: r, RestartGate: func() error { return errors.New("本节点为备机") }}
	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.ran("systemctl start dnsmasq") || r.ran("systemctl restart dnsmasq") {
		t.Fatalf("standby started DHCP — two authoritative servers: %v", r.calls)
	}
}

// 重启失败时文件不能停在新内容上：否则下次同步判定「内容没变」直接返回，dnsmasq 一直跑旧配置或一直停着。
func TestSyncRetriesTheRestartAfterAFailedOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	if err := os.WriteFile(path, []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{errAt: 3, err: errors.New("exit status 1"), out: []byte("Job for dnsmasq.service failed")}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}
	groups := []GroupConfig{testGroupConfig()}

	if err := manager.Sync(context.Background(), groups); err == nil {
		t.Fatal("重启失败必须报错")
	}
	if data, _ := os.ReadFile(path); string(data) != "# old\n" {
		t.Fatalf("重启失败后应恢复旧文件，实际：%s", data)
	}
	// restart 失败时 dnsmasq 已经停了；按旧配置再拉起来，否则 DHCP 一直停到下次变更。
	if len(runner.calls) != 4 || strings.Join(append([]string{runner.calls[3].name}, runner.calls[3].args...), " ") != "systemctl restart dnsmasq" {
		t.Fatalf("恢复旧文件后应再重启一次：%#v", runner.calls)
	}
	before := len(runner.calls)
	if err := manager.Sync(context.Background(), groups); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("systemctl restart dnsmasq") || len(runner.calls) == before {
		t.Fatalf("下次同步应补上重启：%#v", runner.calls[before:])
	}
	last := runner.calls[len(runner.calls)-1]
	if last.name != "systemctl" || last.args[0] != "restart" {
		t.Fatalf("下次同步没有重启：%#v", runner.calls[before:])
	}
}

// 原来没有配置文件时，重启失败要删掉新写的文件，下次同步同样会重试。
func TestSyncRemovesTheNewFileWhenTheFirstRestartFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	runner := &fakeRunner{errAt: 3, err: errors.New("exit status 1")}
	manager := Manager{Path: path, Renderer: testRenderer(), Runner: runner}

	if err := manager.Sync(context.Background(), []GroupConfig{testGroupConfig()}); err == nil {
		t.Fatal("重启失败必须报错")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("新写的文件应被删掉：%v", err)
	}
}
