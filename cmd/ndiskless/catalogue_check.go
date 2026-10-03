package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// dbCopyMissing 在临时文件上打开复制来的库副本（本节点接任后要服务的目录），列出其中记着而池里没有的东西。
// 检查进行不下去时一律答「没缺」：完全没有副本由 PrepareActiveDB 自己拒绝，池不可读则健康检查会失败。
func dbCopyMissing(ctx context.Context, copyPath string, agent storage.StorageAgent, logger *slog.Logger) []string {
	data, err := os.ReadFile(copyPath)
	if err != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "ndiskless-dbcheck-")
	if err != nil {
		logger.Warn("catalogue completeness check skipped", "error", err)
		return nil
	}
	defer os.RemoveAll(dir)
	path := dir + "/ndiskless.db"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		logger.Warn("catalogue completeness check skipped", "error", err)
		return nil
	}
	st, err := store.Open(ctx, "file:"+path)
	if err != nil {
		logger.Warn("catalogue completeness check skipped", "error", err)
		return nil
	}
	defer st.Close()
	missing, err := ops.ReconcileService{Store: st, Storage: agent}.MissingDatasets(ctx)
	if err != nil {
		logger.Warn("catalogue completeness check skipped", "error", err)
		return nil
	}
	return missing
}
