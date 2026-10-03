package main

import "context"

// standbyCleaner 是 quiesceStandby 对存储 agent 的需求。故意收窄：下面的顺序是安全性质，接口小到能伪造，测试才能钉住它。
type standbyCleaner interface {
	ClientCloneMACs(ctx context.Context) ([]string, error)
	CleanupClientClones(ctx context.Context, mac string, keep []string) error
	SuperCloneMACs(ctx context.Context) ([]string, error)
	CleanupSuperClones(ctx context.Context, mac string) error
}

// quiesceStandby 清掉备机不该持有的一切：先拆 iSCSI 导出，再删其后的克隆。
// 顺序就是安全性：导出拆完本机不再对外供盘，后面不可能删掉客户机正在用的卷，无需判断「有没有活会话」
// （回收器曾因看不见 CHAP 会话误删过在用的盘）。
// 超管克隆也一并清掉：超管机放在写入者上，切换后旧写入者的那份没人回收，下次该节点写入时还会被复用，
// 让机器的盘取决于谁持有 VIP；未保存的超管会话本来就不承诺跨切换保留。
// 每步都尽力而为：没收拾完的备机仍是备机，下次降级再试；为了清理让启动失败更糟。
func quiesceStandby(ctx context.Context, teardownExports func(context.Context) error,
	cleaner standbyCleaner, logf func(msg string, args ...any)) {
	if teardownExports != nil {
		if err := teardownExports(ctx); err != nil {
			logf("standby quiesce: LIO teardown failed", "error", err)
		}
	}
	if cleaner == nil {
		return
	}
	if macs, err := cleaner.ClientCloneMACs(ctx); err != nil {
		logf("standby quiesce: client clone listing failed", "error", err)
	} else {
		for _, mac := range macs {
			if err := cleaner.CleanupClientClones(ctx, mac, nil); err != nil {
				logf("standby quiesce: clone cleanup failed", "mac", mac, "error", err)
			}
		}
	}
	if macs, err := cleaner.SuperCloneMACs(ctx); err != nil {
		logf("standby quiesce: super clone listing failed", "error", err)
	} else {
		for _, mac := range macs {
			if err := cleaner.CleanupSuperClones(ctx, mac); err != nil {
				logf("standby quiesce: super clone cleanup failed", "mac", mac, "error", err)
			}
		}
	}
}
