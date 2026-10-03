package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

// 认领本机数据池要按 (server_id, name) 判断：pools 表随复制共享，各节点池名
// 常相同，只按名字判断会让后登记的节点永远认领不到自己的池。
func TestAdoptRegistersThisNodesPoolEvenWhenAPeerHasOneOfTheSameName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)

	// 对端先登记了它的 tank（复制来的记录）。
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "peer", Name: "peer", IP: "10.0.0.4", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-peer--tank", ServerID: "peer", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	svc := PoolService{Store: st, Storage: &fakePoolStorage{
		poolName:     "tank",
		statusByName: map[string]domain.PoolStatus{"tank": {Health: "ONLINE", Capacity: 500, Used: 120}},
	}}
	if err := svc.AdoptDataPool(ctx); err != nil {
		t.Fatal(err)
	}

	pools, err := st.Pools().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine *domain.Pool
	for i := range pools {
		if pools[i].ServerID == "server-a" {
			mine = &pools[i]
		}
	}
	if mine == nil {
		t.Fatalf("本机的池没被认领，表里只有：%+v", pools)
	}
	if mine.Name != "tank" {
		t.Fatalf("认领的不是本机的数据池：%+v", mine)
	}

	// 认领每次启动都跑，再跑一次不能重复登记。
	if err := svc.AdoptDataPool(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Pools().List(ctx)
	if len(again) != len(pools) {
		t.Fatalf("重复认领了：%d -> %d", len(pools), len(again))
	}
}

// 加第二台时表里唯一的另一行是对端，不能当成本机旧身份去接管它的行和池；
// 该路径还曾嵌套事务，SQLite 单连接下会让启动挂死。
func TestSecondNodeDoesNotSupersedeTheFirstOne(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)

	// 第一台已在册并有自己的池。
	first := domain.Server{ID: "first", Name: "first", IP: "10.0.0.3", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}
	if err := st.Servers().Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-first--tank", ServerID: "first", Name: "tank"}); err != nil {
		t.Fatal(err)
	}

	// 第二台以自己的身份登记本机池。
	svc := PoolService{Store: st, Storage: &fakePoolStorage{
		poolName:     "tank",
		statusByName: map[string]domain.PoolStatus{"tank": {Health: "ONLINE", Capacity: 500, Used: 120}},
	}}
	done := make(chan error, 1)
	go func() { done <- svc.AdoptDataPool(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("认领挂住了——外层事务里又开了一个事务，单连接的 SQLite 永远等不到")
	}

	// 第一台必须原样：仍在册、IP 未改、池仍归它。
	got, err := st.Servers().Get(ctx, "first")
	if err != nil {
		t.Fatalf("第一台被删了：%v", err)
	}
	if got.IP != "10.0.0.3" {
		t.Fatalf("第一台的地址被当成旧身份改掉了：%s", got.IP)
	}
	pool, err := st.Pools().Get(ctx, "pool-first--tank")
	if err != nil {
		t.Fatalf("第一台的池没了：%v", err)
	}
	if pool.ServerID != "first" {
		t.Fatalf("第一台的池被划到了别人名下：%s", pool.ServerID)
	}
}

// 建池重名校验按节点算：两台都把数据池叫 tank 是合法部署，别台的复制记录
// 不能让本机建池被拒。
func TestCreatePoolAllowsANameAnotherNodeAlreadyUses(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "peer", Name: "peer", IP: "10.0.0.4", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
	// 对端的 tank，复制来的记录。
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-peer--tank", ServerID: "peer", Name: "tank"}); err != nil {
		t.Fatal(err)
	}
	svc := PoolService{Store: st, Storage: &fakePoolStorage{}}

	if err := svc.ensurePoolNameUnique(ctx, "tank"); err != nil {
		t.Fatalf("别台叫 tank 不该挡住本机建 tank：%v", err)
	}

	// 本机已有 tank 时仍须拒绝。
	if err := st.Servers().Create(ctx, domain.Server{
		ID: "server-a", Name: "server-a", IP: "10.0.0.3", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pools().Create(ctx, domain.Pool{ID: "pool-server-a--tank", ServerID: "server-a", Name: "tank"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ensurePoolNameUnique(ctx, "tank"); !errors.Is(err, ErrPoolExists) {
		t.Fatalf("本机重名要拒，得到 %v", err)
	}
}
