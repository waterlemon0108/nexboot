// Package systemd 薄封装 systemctl/journalctl，用于查看和控制固定的几个主机服务；Runner 可在测试中替换。
package systemd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner 执行命令并返回合并后的输出。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner 通过 os/exec 执行命令。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// UnitStatus 是 `systemctl show` 中对外展示的字段。
type UnitStatus struct {
	Load        string // LoadState：loaded / not-found / masked
	Active      string // ActiveState：active / inactive / failed / activating
	Sub         string // SubState：running / dead / exited / failed
	UnitFile    string // UnitFileState：enabled / disabled / static / ""
	MainPID     int
	ActiveSince time.Time // ActiveEnterTimestamp；未激活或无法解析时为零值
}

func (u UnitStatus) Installed() bool { return u.Load != "not-found" && u.Load != "" }
func (u UnitStatus) Enabled() bool   { return u.UnitFile == "enabled" || u.UnitFile == "enabled-runtime" }

const showProps = "LoadState,ActiveState,SubState,UnitFileState,MainPID,ActiveEnterTimestamp"

// Client 封装本机的 systemctl / journalctl。
type Client struct {
	Runner     Runner
	Systemctl  string // 默认 "systemctl"
	Journalctl string // 默认 "journalctl"
}

func (c Client) systemctl() string {
	if c.Systemctl != "" {
		return c.Systemctl
	}
	return "systemctl"
}
func (c Client) journalctl() string {
	if c.Journalctl != "" {
		return c.Journalctl
	}
	return "journalctl"
}
func (c Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

// Show 返回 unit 状态。unit 不存在不算错误（LoadState=not-found，调用方显示为未安装）；
// systemctl 本身不可用等硬错误照常返回。
func (c Client) Show(ctx context.Context, unit string) (UnitStatus, error) {
	out, err := c.runner().Run(ctx, c.systemctl(), "show", unit, "--property="+showProps)
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return UnitStatus{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return parseShow(out), nil
}

func parseShow(out []byte) UnitStatus {
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, '=')
		if i < 0 {
			continue
		}
		kv[line[:i]] = line[i+1:]
	}
	st := UnitStatus{
		Load:     kv["LoadState"],
		Active:   kv["ActiveState"],
		Sub:      kv["SubState"],
		UnitFile: kv["UnitFileState"],
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(kv["MainPID"])); err == nil {
		st.MainPID = pid
	}
	st.ActiveSince = parseSystemdTime(kv["ActiveEnterTimestamp"])
	return st
}

// parseSystemdTime 解析 "Thu 2026-07-03 09:00:00 UTC" 格式。Go 解析时区缩写不可靠，
// 而结果只用于和本机时钟算时长，所以按 Local 解析并忽略缩写。
func parseSystemdTime(v string) time.Time {
	f := strings.Fields(strings.TrimSpace(v))
	if len(f) < 3 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", f[1]+" "+f[2], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Action 对 unit 执行 start/stop/restart，由调用方校验 action。
func (c Client) Action(ctx context.Context, unit, action string) error {
	out, err := c.runner().Run(ctx, c.systemctl(), action, unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Logs 返回 unit 最近 n 行日志。
func (c Client) Logs(ctx context.Context, unit string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}
	out, err := c.runner().Run(ctx, c.journalctl(), "-u", unit, "-n", strconv.Itoa(lines), "--no-pager")
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return "", fmt.Errorf("journalctl -u %s: %w", unit, err)
	}
	return string(out), nil
}
