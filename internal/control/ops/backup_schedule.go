package ops

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BackupSchedule 是备份开始的时刻。刻意不用 cron（语法没人记得住），也不用
// 间隔（会逐轮漂移到上课时间）。
// 格式：「daily@HH:MM」或「weekly@<0-6>@HH:MM」（0 为周日）；空串表示不定时。
type BackupSchedule struct {
	Weekly  bool
	Weekday time.Weekday
	Hour    int
	Minute  int
	Empty   bool
}

// legacyScheduleHour 是旧的间隔写法（如「24h」）兼容落到的钟点；判为无效会
// 让原本按时备份的机器悄悄停掉。
const legacyScheduleHour = 3

func ParseBackupSchedule(raw string) (BackupSchedule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return BackupSchedule{Empty: true}, nil
	}
	if _, err := time.ParseDuration(s); err == nil {
		return BackupSchedule{Hour: legacyScheduleHour}, nil
	}
	parts := strings.Split(s, "@")
	switch {
	case len(parts) == 2 && parts[0] == "daily":
		h, m, err := parseHourMinute(parts[1])
		if err != nil {
			return BackupSchedule{}, err
		}
		return BackupSchedule{Hour: h, Minute: m}, nil
	case len(parts) == 3 && parts[0] == "weekly":
		d, err := strconv.Atoi(parts[1])
		if err != nil || d < 0 || d > 6 {
			return BackupSchedule{}, fmt.Errorf("星期要在 0（周日）到 6（周六）之间：%q", parts[1])
		}
		h, m, err := parseHourMinute(parts[2])
		if err != nil {
			return BackupSchedule{}, err
		}
		return BackupSchedule{Weekly: true, Weekday: time.Weekday(d), Hour: h, Minute: m}, nil
	}
	return BackupSchedule{}, fmt.Errorf("计划要写成 daily@03:00 或 weekly@0@04:00：%q", s)
}

func parseHourMinute(s string) (int, int, error) {
	hm := strings.Split(s, ":")
	if len(hm) != 2 {
		return 0, 0, fmt.Errorf("时间要写成 HH:MM：%q", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("时间要在 00:00 到 23:59 之间：%q", s)
	}
	return h, m, nil
}

func (s BackupSchedule) String() string {
	if s.Empty {
		return ""
	}
	if s.Weekly {
		return fmt.Sprintf("weekly@%d@%02d:%02d", int(s.Weekday), s.Hour, s.Minute)
	}
	return fmt.Sprintf("daily@%02d:%02d", s.Hour, s.Minute)
}

// Next 返回严格晚于 after 的第一个触发时刻。
func (s BackupSchedule) Next(after time.Time) time.Time {
	if s.Empty {
		return time.Time{}
	}
	at := time.Date(after.Year(), after.Month(), after.Day(), s.Hour, s.Minute, 0, 0, after.Location())
	if !s.Weekly {
		if !at.After(after) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	delta := (int(s.Weekday) - int(after.Weekday()) + 7) % 7
	at = at.AddDate(0, 0, delta)
	if !at.After(after) {
		at = at.AddDate(0, 0, 7)
	}
	return at
}

// Describe 返回界面展示用的计划文字。
func (s BackupSchedule) Describe() string {
	if s.Empty {
		return "未设置"
	}
	if s.Weekly {
		return fmt.Sprintf("每周%s %02d:%02d", weekdayCN[s.Weekday], s.Hour, s.Minute)
	}
	return fmt.Sprintf("每天 %02d:%02d", s.Hour, s.Minute)
}

var weekdayCN = [...]string{"日", "一", "二", "三", "四", "五", "六"}
