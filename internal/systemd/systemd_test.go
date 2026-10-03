package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	out  []byte
	err  error
	name string
	args []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = args
	return r.out, r.err
}

func TestShowParsesActiveUnit(t *testing.T) {
	fr := &fakeRunner{out: []byte("LoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=1234\nActiveEnterTimestamp=Thu 2026-07-03 09:00:00 UTC\n")}
	c := Client{Runner: fr}
	st, err := c.Show(context.Background(), "dnsmasq.service")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Installed() || !st.Enabled() {
		t.Fatalf("expected installed+enabled, got %+v", st)
	}
	if st.Active != "active" || st.Sub != "running" || st.MainPID != 1234 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st.ActiveSince.IsZero() {
		t.Fatalf("expected parsed ActiveSince")
	}
	if fr.name != "systemctl" || fr.args[0] != "show" || fr.args[1] != "dnsmasq.service" {
		t.Fatalf("unexpected command: %s %v", fr.name, fr.args)
	}
}

func TestShowNotFoundIsNotError(t *testing.T) {
	fr := &fakeRunner{out: []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\n"), err: errors.New("exit 4")}
	st, err := Client{Runner: fr}.Show(context.Background(), "missing.service")
	if err != nil {
		t.Fatalf("not-found should not error: %v", err)
	}
	if st.Installed() {
		t.Fatalf("expected not installed")
	}
}

func TestShowHardErrorWhenNoOutput(t *testing.T) {
	fr := &fakeRunner{err: errors.New(`exec: "systemctl": not found`)}
	if _, err := (Client{Runner: fr}).Show(context.Background(), "x.service"); err == nil {
		t.Fatalf("expected error when systemctl missing")
	}
}

func TestActionRunsCorrectCommand(t *testing.T) {
	fr := &fakeRunner{}
	if err := (Client{Runner: fr}).Action(context.Background(), "target.service", "restart"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fr.name != "systemctl" || fr.args[0] != "restart" || fr.args[1] != "target.service" {
		t.Fatalf("unexpected command: %s %v", fr.name, fr.args)
	}
}

func TestLogsDefaultLines(t *testing.T) {
	fr := &fakeRunner{out: []byte("line1\nline2\n")}
	out, err := (Client{Runner: fr}).Logs(context.Background(), "dnsmasq.service", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "line1") {
		t.Fatalf("unexpected logs: %q", out)
	}
	if fr.args[0] != "-u" || fr.args[1] != "dnsmasq.service" || fr.args[2] != "-n" || fr.args[3] != "100" {
		t.Fatalf("unexpected journalctl args: %v", fr.args)
	}
}

func TestParseSystemdTimeDurationConsistent(t *testing.T) {
	// 几分钟前的时间戳应得到一个较小的正时长。
	past := time.Now().Add(-5 * time.Minute)
	ts := "Thu " + past.Format("2006-01-02 15:04:05") + " UTC"
	got := parseSystemdTime(ts)
	if got.IsZero() {
		t.Fatalf("failed to parse %q", ts)
	}
	d := time.Since(got)
	if d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("unexpected duration %v", d)
	}
}
