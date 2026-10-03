package ha

import (
	"fmt"
	"os"
	"syscall"
)

// AcquireProcessLock 对 path 加排他 flock，已被其他进程（或本进程第二次）持有时拒绝：
// 同一个池上跑两个实例会重复清理、并发回收克隆，所有跨进程锁都失效。锁随返回的 release（或进程）存在。
func AcquireProcessLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("已有 ndiskless 实例在运行（锁 %s 被占用），本实例退出", path)
	}
	_ = os.Truncate(path, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
