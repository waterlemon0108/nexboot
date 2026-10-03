package storage

import (
	"context"
	"sync"
	"time"
)

// catalogueChanges 让复制轮次与目录变更互斥。超管保存、合并等变更有多个 ZFS 步骤，且先写池后写库；
// 若快照恰好落在中间，接任的备机拿到的是不完整目录并会拒绝接任。所以每轮复制要等进行中的变更
// （池操作加库提交）结束，新变更也要等这轮复制。等客户机关机、导入导出流传输不得放在锁内。
var catalogueChanges sync.RWMutex

// ChangeCatalogue 标记一次目录变更开始，调用 done 结束；由控制层包住存储调用和数据库提交。
// 不可重入：有复制轮次在等时嵌套获取会死锁。
func ChangeCatalogue() (done func()) {
	catalogueChanges.RLock()
	return catalogueChanges.RUnlock
}

// HoldCatalogue 等进行中的目录变更结束，并在 release 前挡住新变更。
func HoldCatalogue() (release func()) {
	catalogueChanges.Lock()
	return catalogueChanges.Unlock
}

// HoldCatalogueContext 是可放弃的 HoldCatalogue：只尝试不排队，等待中的读者不会挡住后面的变更。
func HoldCatalogueContext(ctx context.Context) (release func(), err error) {
	for !catalogueChanges.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return catalogueChanges.Unlock, nil
}
