package main

import (
	"context"
	"fmt"
	"time"

	"github.com/tianwei/diskless/internal/control/ops"
)

// catchUpTo 反复拉取，直到本节点完整持有 target 或 ctx 结束。一次未必够：递归轮被拒时会退回逐个数据集追，
// 两次拉取之间也可能落下新的一轮。
func catchUpTo(ctx context.Context, target string, pull func(context.Context) error,
	held func(context.Context) string, pause time.Duration) error {
	for {
		err := pull(ctx)
		now := held(ctx)
		if ops.CaughtUpTo(now, target) {
			return nil
		}
		select {
		case <-ctx.Done():
			if now == "" {
				now = "无"
			}
			if err != nil {
				return fmt.Errorf("期限内没有追平到 %s（本机停在 %s）：%w", target, now, err)
			}
			return fmt.Errorf("期限内没有追平到 %s（本机停在 %s）", target, now)
		case <-time.After(pause):
		}
	}
}
