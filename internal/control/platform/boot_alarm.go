package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

const (
	// 失败之后会话保持这么久，说明系统已经起来了。
	bootedAfter = 3 * time.Minute
	// 没修也没再开机的，一天后不再提。
	bootFailureAlarmFor = 24 * time.Hour
)

// BootFailureAlarmRule 展示每台机器最近一次开机失败，直到它开机成功或失败已超过一天。
func BootFailureAlarmRule(st store.Store, now func() time.Time) AlarmRule {
	return AlarmRule{Source: "boot", Eval: func(ctx context.Context) ([]AlarmCondition, error) {
		failures, err := st.BootFailures().List(ctx)
		if err != nil || len(failures) == 0 {
			return nil, err
		}
		terminals, err := st.Terminals().List(ctx)
		if err != nil {
			return nil, err
		}
		byMAC := map[string]domain.Terminal{}
		for _, t := range terminals {
			byMAC[storage.NormalizeMAC(t.MAC)] = t
		}
		images, err := st.Images().List(ctx)
		if err != nil {
			return nil, err
		}
		imageName := map[string]string{}
		for _, img := range images {
			imageName[img.ID] = img.Name
		}
		at := now().UTC()
		var conds []AlarmCondition
		for _, f := range failures {
			t, ok := byMAC[f.MAC]
			if !ok || at.Sub(f.At) > bootFailureAlarmFor || bootedSince(t, f.At, at) {
				continue
			}
			name := t.Name
			if name == "" {
				name = t.MAC
			}
			img := imageName[f.ImageID]
			if img == "" {
				img = f.ImageID
			}
			conds = append(conds, AlarmCondition{
				Key:      "boot:" + f.MAC,
				Severity: "error",
				Type:     "开机失败",
				Resource: name,
				Value:    f.Stage,
				Message:  clock(f.At) + " " + bootFailureMessage(name, img, f),
			})
		}
		return conds, nil
	}}
}

func bootedSince(t domain.Terminal, failedAt, now time.Time) bool {
	if t.State != domain.TerminalStateOnline || t.OnlineSince == nil {
		return false
	}
	from := *t.OnlineSince
	if failedAt.After(from) {
		from = failedAt
	}
	return now.Sub(from) >= bootedAfter
}

func bootFailureMessage(name, image string, f domain.BootFailure) string {
	code := ""
	if f.Code != "" {
		code = "，错误码 " + f.Code
	}
	switch f.Stage {
	case domain.BootStageBootMode:
		fw, other := "UEFI", "BIOS"
		if f.Platform == "pcbios" {
			fw, other = "BIOS", "UEFI"
		}
		return fmt.Sprintf("%s 按 %s 启动，镜像 %s 只能 %s 引导：请把客户机固件改成 %s，或给它的分组换一个支持 %s 引导的镜像",
			name, fw, image, other, other, fw)
	case domain.BootStageSanhook:
		return fmt.Sprintf("%s 开机时连不上系统盘（iSCSI 登录失败%s）：请检查客户机到服务器 3260 端口的网络，以及服务器上的 iSCSI 服务", name, code)
	default:
		return fmt.Sprintf("%s 挂上了系统盘但没能从盘上引导（镜像 %s%s）：请到「镜像」页给 %s 重跑体检，看引导方式与客户机固件是否一致", name, image, code, image)
	}
}
