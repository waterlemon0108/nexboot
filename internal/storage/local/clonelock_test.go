package local

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// hookZFS 让测试把调用停在 Agent 临界区内，直接观察串行化，而不是靠时序推断。
type hookZFS struct {
	*fakePool
	onList func()
}

func (h *hookZFS) ListDatasets(ctx context.Context) ([]string, error) {
	if h.onList != nil {
		h.onList()
	}
	return h.fakePool.ListDatasets(ctx)
}

func TestClientCloneMACs(t *testing.T) {
	agent := New("server-a", poolWith(
		"tank/run/CLIENT-AABBCCDDEE01",
		"tank/run/CLIENT-AABBCCDDEE01-DATA-1", // 同一台客户机，不应重复
		"tank/run/CLIENT-AABBCCDDEE02",
		"tank/run/SCLIENT-AABBCCDDEE03", // 超管克隆是持久的，不回收
		"tank/nd/img-windows-11",
		"tank/nd/cfg-default",
	))

	macs, err := agent.ClientCloneMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(macs, []string{"AABBCCDDEE01", "AABBCCDDEE02"}) {
		t.Fatalf("macs = %v, want the two throwaway clients only", macs)
	}
}

func TestCreateClientLUNRollbackDoesNotDeadlock(t *testing.T) {
	// CreateClientLUN 内的回滚持有该客户机锁去清理克隆，必须走不加锁的路径，否则死锁。
	agent := New("server-a", newFakePool())
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	done := make(chan error, 1)
	go func() {
		// 空 source 让 createClientDisks 失败，触发回滚。
		_, err := agent.CreateClientLUN(context.Background(), storage.ClientReq{MAC: "aa:bb:cc:dd:ee:ff"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the missing-source error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CreateClientLUN deadlocked on its own rollback")
	}
}

func TestCloneLifecycleSerializesPerClient(t *testing.T) {
	// 同一客户机的回收与供给不能交错，否则回收会删掉开机流程刚建的克隆。
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	zfsFake := &hookZFS{fakePool: newFakePool()}
	zfsFake.onList = func() {
		entered <- struct{}{}
		<-release
	}
	agent := New("server-a", zfsFake)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	go func() { _ = agent.CleanupClientClones(context.Background(), "aa:bb:cc:dd:ee:ff", nil) }()
	<-entered // 第一次调用停在临界区内

	second := make(chan struct{})
	go func() {
		_ = agent.CleanupClientClones(context.Background(), "AA:BB:CC:DD:EE:FF", nil) // 同一 MAC 的不同写法
		close(second)
	}()

	select {
	case <-entered:
		t.Fatal("second call for the same client entered while the first still held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-entered
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second call never completed after the lock was released")
	}
}

func TestCloneLifecycleRunsClientsInParallel(t *testing.T) {
	// 按客户机加锁不能退化成全局串行：不同客户机应并发执行，否则开机风暴会排队。
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	zfsFake := &hookZFS{fakePool: newFakePool()}
	zfsFake.onList = func() {
		entered <- struct{}{}
		<-release
	}
	agent := New("server-a", zfsFake)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }

	for _, mac := range []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"} {
		go func() { _ = agent.CleanupClientClones(context.Background(), mac, nil) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("distinct clients did not run concurrently")
		}
	}
	close(release)
}

// SuperCloneMACs 决定哪些盘会被销毁，必须单独测。它与 ClientCloneMACs 互补
// （后者故意不含超管克隆），两者合起来覆盖 run/ 下全部克隆。
func TestSuperCloneMACs(t *testing.T) {
	agent := New("server-a", poolWith(
		"tank/run/SCLIENT-AABBCCDDEE01",
		"tank/run/SCLIENT-AABBCCDDEE01-DATA-1", // 同一台机器的数据盘，不该重复出现
		"tank/run/SCLIENT-AABBCCDDEE02",
		"tank/run/CLIENT-AABBCCDDEE03", // 普通客户机归另一条路
		"tank/run/INSPECT-AABBCCDDEE04",
		"tank/nd/img-windows-11",
		"tank/nd/cfg-default",
	))

	macs, err := agent.SuperCloneMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(macs, []string{"AABBCCDDEE01", "AABBCCDDEE02"}) {
		t.Fatalf("macs = %v，只该是两台超管机", macs)
	}
}

// 别的池里的同名数据集不属于本机。销毁不可逆，宁漏勿错，所以比 ClientCloneMACs 多做一步 poolRelative。
func TestSuperCloneMACsIgnoresAnotherPool(t *testing.T) {
	agent := New("server-a", poolWith(
		"tank/run/SCLIENT-AABBCCDDEE01",
		"otherpool/run/SCLIENT-AABBCCDDEE99",
	))

	macs, err := agent.SuperCloneMACs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(macs, []string{"AABBCCDDEE01"}) {
		t.Fatalf("macs = %v，别的池里的不该被认领", macs)
	}
}
