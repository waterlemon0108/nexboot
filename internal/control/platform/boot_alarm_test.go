package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var bootAlarmNow = time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)

func seedBootAlarm(t *testing.T, terminal domain.Terminal, failure domain.BootFailure) store.Store {
	t.Helper()
	ctx := context.Background()
	st := newAlarmStore(t)
	img := domain.Image{ID: "ubuntu", Name: "ubuntu", OSType: domain.OSTypeLinux, State: domain.ImageStateNormal, CreatedAt: bootAlarmNow}
	cfg := domain.Config{ID: "ubuntu_default", ImageID: img.ID, Name: "default", CreatedAt: bootAlarmNow}
	red := domain.Reduction{ID: "ubuntu_0", ConfigID: cfg.ID, Name: "@0", Status: domain.ReductionStatusReady, CreatedAt: bootAlarmNow}
	group := domain.Group{ID: "g-ubuntu", Name: "ubuntu", StartIP: "192.168.10.40", ClientMax: 30, Netmask: "255.255.255.0",
		SystemImageID: img.ID, SystemConfigID: cfg.ID, SystemReductionID: red.ID}
	for _, err := range []error{
		st.Images().Create(ctx, img),
		st.Configs().Create(ctx, cfg),
		st.Reductions().Create(ctx, red),
		st.Groups().Create(ctx, group),
		st.Terminals().Create(ctx, terminal),
		st.BootFailures().Upsert(ctx, failure),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func frontDesk() domain.Terminal {
	return domain.Terminal{ID: "terminal-0050562EF64E", Name: "前台01", MAC: "0050562EF64E", IP: "192.168.10.40", GroupID: "g-ubuntu", State: domain.TerminalStateOffline}
}

func evalBootAlarms(t *testing.T, st store.Store) []AlarmCondition {
	t.Helper()
	conds, err := BootFailureAlarmRule(st, func() time.Time { return bootAlarmNow }).Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return conds
}

func TestBootModeMismatchAlarmSaysWhatToChange(t *testing.T) {
	st := seedBootAlarm(t, frontDesk(), domain.BootFailure{
		MAC: "0050562EF64E", Stage: domain.BootStageBootMode, Platform: "efi", ImageID: "ubuntu", At: bootAlarmNow.Add(-time.Minute),
	})
	conds := evalBootAlarms(t, st)
	if len(conds) != 1 {
		t.Fatalf("conds = %#v", conds)
	}
	c := conds[0]
	if c.Severity != "error" || c.Type != "开机失败" || c.Resource != "前台01" {
		t.Fatalf("cond = %#v", c)
	}
	for _, want := range []string{"前台01", "UEFI", "ubuntu", "把客户机固件改成 BIOS", "支持 UEFI 引导的镜像"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message %q lacks %q", c.Message, want)
		}
	}
}

func TestSanhookAndSanbootAlarmsPointAtDifferentThings(t *testing.T) {
	hook := evalBootAlarms(t, seedBootAlarm(t, frontDesk(), domain.BootFailure{
		MAC: "0050562EF64E", Stage: domain.BootStageSanhook, Code: "0x3f102003", Platform: "efi", ImageID: "ubuntu", At: bootAlarmNow.Add(-time.Minute),
	}))
	boot := evalBootAlarms(t, seedBootAlarm(t, frontDesk(), domain.BootFailure{
		MAC: "0050562EF64E", Stage: domain.BootStageSanboot, Code: "0x7f048283", Platform: "efi", ImageID: "ubuntu", At: bootAlarmNow.Add(-time.Minute),
	}))
	if len(hook) != 1 || len(boot) != 1 {
		t.Fatalf("hook=%#v boot=%#v", hook, boot)
	}
	if !strings.Contains(hook[0].Message, "连不上系统盘") || !strings.Contains(hook[0].Message, "0x3f102003") {
		t.Fatalf("sanhook message = %q", hook[0].Message)
	}
	if !strings.Contains(boot[0].Message, "没能从盘上引导") || !strings.Contains(boot[0].Message, "体检") {
		t.Fatalf("sanboot message = %q", boot[0].Message)
	}
}

func TestBootFailureAlarmClears(t *testing.T) {
	failedAt := bootAlarmNow.Add(-5 * time.Minute)
	failure := domain.BootFailure{MAC: "0050562EF64E", Stage: domain.BootStageSanboot, Platform: "efi", ImageID: "ubuntu", At: failedAt}

	booted := frontDesk()
	booted.State = domain.TerminalStateOnline
	since := failedAt.Add(time.Minute) // 失败后又开机，会话保持了 4 分钟
	booted.OnlineSince = &since
	if conds := evalBootAlarms(t, seedBootAlarm(t, booted, failure)); len(conds) != 0 {
		t.Fatalf("stayed online after the failure, still alarmed: %#v", conds)
	}

	flicker := frontDesk()
	flicker.State = domain.TerminalStateOnline
	recent := bootAlarmNow.Add(-time.Minute) // 刚挂上盘，还说明不了什么
	flicker.OnlineSince = &recent
	if conds := evalBootAlarms(t, seedBootAlarm(t, flicker, failure)); len(conds) != 1 {
		t.Fatalf("a fresh session cleared the alarm: %#v", conds)
	}

	old := failure
	old.At = bootAlarmNow.Add(-25 * time.Hour)
	if conds := evalBootAlarms(t, seedBootAlarm(t, frontDesk(), old)); len(conds) != 0 {
		t.Fatalf("a day-old failure still alarmed: %#v", conds)
	}

	st := seedBootAlarm(t, frontDesk(), failure)
	if err := st.Terminals().Delete(context.Background(), "terminal-0050562EF64E"); err != nil {
		t.Fatal(err)
	}
	if conds := evalBootAlarms(t, st); len(conds) != 0 {
		t.Fatalf("a deleted terminal still alarmed: %#v", conds)
	}
}
