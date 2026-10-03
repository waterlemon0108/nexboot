package ops

import (
	"testing"
	"time"
)

// 计划按「每天/每周 + 钟点」解析：不用 cron，也不用会逐轮漂移的间隔。
func TestBackupScheduleParsesDailyAndWeekly(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		desc    string
	}{
		{"daily@03:00", false, "每天 03:00"},
		{"daily@23:30", false, "每天 23:30"},
		{"weekly@0@04:00", false, "每周日 04:00"},
		{"weekly@6@04:00", false, "每周六 04:00"},
		{"", false, "未配置"},
		{"daily@24:00", true, "小时越界"},
		{"daily@03:60", true, "分钟越界"},
		{"weekly@7@04:00", true, "星期越界"},
		{"0 3 * * *", true, "cron 表达式不是这里的语法"},
		{"nonsense", true, "看不懂"},
	}
	for _, c := range cases {
		_, err := ParseBackupSchedule(c.in)
		if c.wantErr && err == nil {
			t.Errorf("%s: %q 应当被拒", c.desc, c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: %q 被拒了：%v", c.desc, c.in, err)
		}
	}
}

func TestBackupScheduleNextRun(t *testing.T) {
	// 周三 2026-08-19 10:00
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)

	daily, err := ParseBackupSchedule("daily@03:00")
	if err != nil {
		t.Fatal(err)
	}
	got := daily.Next(now)
	want := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("每天 03:00 的下一次 = %v，想要 %v", got, want)
	}
	// 当天还没到点，就是今天。
	got = daily.Next(time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC))
	want = time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("当天未到点 = %v，想要 %v", got, want)
	}

	// 周三算「每周日 04:00」，答案是本周日。
	weekly, err := ParseBackupSchedule("weekly@0@04:00")
	if err != nil {
		t.Fatal(err)
	}
	got = weekly.Next(now)
	want = time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("每周日 04:00 的下一次 = %v，想要 %v", got, want)
	}
}

// 旧配置里的 24h 这类间隔按每天处理，判为非法会让按时备份的机器悄悄停掉。
func TestBackupScheduleAdoptsLegacyDuration(t *testing.T) {
	s, err := ParseBackupSchedule("24h")
	if err != nil {
		t.Fatalf("老的间隔值应当被接纳：%v", err)
	}
	if s.String() != "daily@03:00" {
		t.Fatalf("老间隔被换成了 %q", s.String())
	}
}

// 计划按本机时区解释：按 UTC 算会让 UTC+8 机房里 03:00 的备份在 11 点开跑。
func TestBackupScheduleRunsAtLocalWallClock(t *testing.T) {
	zone := time.FixedZone("CST", 8*3600)
	s, err := ParseBackupSchedule("daily@03:00")
	if err != nil {
		t.Fatal(err)
	}
	// 本地时间 8 月 23 日 22:41（UTC 是 14:41，已经过了当天 UTC 的 03:00）
	now := time.Date(2026, 8, 23, 22, 41, 0, 0, zone)
	got := s.Next(now)
	if got.In(zone).Hour() != 3 || got.In(zone).Minute() != 0 {
		t.Fatalf("下一次落在本地 %02d:%02d，而计划写的是 03:00", got.In(zone).Hour(), got.In(zone).Minute())
	}
	want := time.Date(2026, 8, 24, 3, 0, 0, 0, zone)
	if !got.Equal(want) {
		t.Fatalf("下一次 = %v，想要 %v", got, want)
	}
}
