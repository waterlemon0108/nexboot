package platform

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/ha"
)

// takeoverAlarmFor 是接任遗留问题在告警列表上保留的时长，能恢复的几天内都已恢复。
const takeoverAlarmFor = 7 * 24 * time.Hour

// TakeoverAlarmRule 告诉操作者上次接任没能带过来什么。节点没追平也会接任（VIP 上没有写入者谁都服务不了），
// 遗留问题必须在会被看到的地方说出来，不能只写在已重启机器的日志里。
func TakeoverAlarmRule(read func() (ha.TakeoverReport, bool), now func() time.Time) AlarmRule {
	return AlarmRule{Source: "takeover", Eval: func(context.Context) ([]AlarmCondition, error) {
		report, ok := read()
		if !ok || now().Sub(report.At) > takeoverAlarmFor {
			return nil, nil
		}
		var parts []string
		behind := ""
		if mine, theirs, ok := markerTimes(report.Mine, report.Theirs); ok && theirs.After(mine) {
			behind = fmt.Sprintf("%d 分钟", int(theirs.Sub(mine).Round(time.Minute).Minutes()))
			who := "原主机"
			if report.Peer != "" {
				who = "原主机 " + report.Peer
			}
			parts = append(parts, fmt.Sprintf("接任时本机的目录比%s少约 %s的改动（本机到 %s，它到 %s），这段改动没有带过来。"+
				"%s恢复后会跟随本机，它手里多出的内容会被挪到一边保留并另行告警，届时请确认要不要找回",
				who, behind, clock(mine), clock(theirs), who))
		}
		if len(report.Missing) > 0 {
			parts = append(parts, fmt.Sprintf("接任时本机缺少 %s 的数据集。如果它们是切换前刚删除的，"+
				"到「镜像管理」里把它们删掉即可清掉残留记录；否则请从备份或保留的目录副本里恢复",
				strings.Join(report.Missing, "、")))
		}
		if len(parts) == 0 {
			parts = append(parts, "接任前没能与原主机核对完目录，可能有改动没有带过来；原主机恢复后请留意「保留的目录副本」告警")
		}
		msg := fmt.Sprintf("%s 发生过一次故障切换：%s", clock(report.At), strings.Join(parts, "；"))
		if report.Cause != "" {
			msg += "（未能追平的原因：" + report.Cause + "）"
		}
		value := "目录不完整"
		if behind != "" {
			value = "少约 " + behind
		}
		return []AlarmCondition{{
			Key:      "takeover:" + strconv.FormatInt(report.At.Unix(), 10),
			Severity: "warn",
			Type:     "故障切换未追平",
			Resource: report.Peer,
			Value:    value,
			Message:  msg,
		}}, nil
	}}
}

// markerTimes 读取两个复制标记的打点时间，标记格式为 rep-<unix 纳秒>，用打点写入者的时钟。
func markerTimes(a, b string) (time.Time, time.Time, bool) {
	ta, okA := markerTime(a)
	tb, okB := markerTime(b)
	return ta, tb, okA && okB
}

func markerTime(marker string) (time.Time, bool) {
	_, digits, ok := strings.Cut(marker, "-")
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

func clock(t time.Time) string { return t.Local().Format("01-02 15:04") }
