package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 没收到库副本的备机接管时会拒绝，所以不该是故障转移候选，更不该占着 VIP：
// 否则 nopreempt 下主机抢不回来，集群有 VIP 没主机，所有写操作 503。
func TestStandbyWithoutDBCopyIsUnfitToHoldTheVIP(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "ndiskless.db")

	// 主机用自己的在用库，从不需要这份副本。
	if err := standbyMissingDBCopy(ctx, "active", copyPath, nil); err != nil {
		t.Fatalf("主机不该因为没有副本被判不健康：%v", err)
	}

	// 备机没有副本：顶不上，就别参选。
	err := standbyMissingDBCopy(ctx, "standby", copyPath, nil)
	if err == nil {
		t.Fatal("备机没有数据库副本时必须报不健康，否则它会占着虚 IP 却接管不了")
	}
	if !strings.Contains(err.Error(), "数据库副本") {
		t.Fatalf("理由要说清楚是缺什么：%v", err)
	}

	// 有副本：正常参选。
	if err := os.WriteFile(copyPath, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := standbyMissingDBCopy(ctx, "standby", copyPath, nil); err != nil {
		t.Fatalf("副本在的备机应当健康：%v", err)
	}
}

// 备机副本是 recv -u 收来的，可能已收到但没挂载，目录读出来是空的。裸 stat 会把它误判成没收到，
// 数据齐全的备机反而让出 VIP，所以查不到时先挂一次再看。
func TestUnmountedCopyIsNotMistakenForAMissingOne(t *testing.T) {
	ctx := context.Background()
	mounted := t.TempDir()
	if err := os.WriteFile(filepath.Join(mounted, "ndiskless.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	remount := func(context.Context) (string, error) { calls++; return mounted, nil }

	// 先看的路径是空的（没挂载时就是这样）。
	if err := standbyMissingDBCopy(ctx, "standby", filepath.Join(t.TempDir(), "ndiskless.db"), remount); err != nil {
		t.Fatalf("挂上之后副本是在的，不该判死：%v", err)
	}
	if calls != 1 {
		t.Fatalf("查不到时应当尝试挂载一次，实际 %d 次", calls)
	}

	// 挂不上、也确实没有：照常判死。
	bad := func(context.Context) (string, error) { return "", errors.New("池不可用") }
	if err := standbyMissingDBCopy(ctx, "standby", filepath.Join(t.TempDir(), "ndiskless.db"), bad); err == nil {
		t.Fatal("挂载失败且无副本时必须判不健康")
	}
}

// 「还没建池」和「池坏了」是两回事：还没建池的非备机必须健康，否则最小化装机后 VIP 落不下来，运维没法从 VIP 进来建池。
// 边界是「不是备机」而非「集群里只有自己」：主机还没池时运维加入第二台是正常中间态，不能因此掉 VIP。
// 备机没池仍不健康：拿不到库副本、接管不了。
func TestOnlyANonStandbyMayHoldTheVIPWithoutAPool(t *testing.T) {
	cases := []struct {
		name     string
		dataPool string
		role     string
		want     bool
	}{
		{"刚装好的主机：还没建池", "", "active", true},
		{"加进来第二台之后，主机仍然还没建池", "", "active", true},
		{"独立节点（还没组集群）", "", "all", true},
		{"被纳管的备机：还没建池，接管不了", "", "standby", false},
		{"建过池、现在读不出来：坏机器", "ndpool", "active", false},
		{"建过池的备机", "ndpool", "standby", false},
		{"设置里是空白字符也算没建过", "   ", "active", true},
	}
	for _, c := range cases {
		if got := unprovisionedNonStandby(c.dataPool, c.role); got != c.want {
			t.Fatalf("%s：unprovisionedNonStandby(%q, %q) = %v，want %v",
				c.name, c.dataPool, c.role, got, c.want)
		}
	}
}

// 数据池名是每台机器自己的事，不跟随目录复制。目录不存带池名的绝对路径，池记录按 (节点, 池名) 区分；
// 若从会复制的 system_settings.data_pool 绑定，A 上的池名会传给池叫 tank 的 B，接管后 healthz 报池不可读。
func TestDataPoolBindingIsPerNodeNotClusterWide(t *testing.T) {
	has := func(names ...string) func(string) bool {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		return func(p string) bool { return set[p] }
	}
	cases := []struct {
		name             string
		local, persisted string
		env              string
		exists           func(string) bool
		want             string
	}{
		{"本机记过就用本机的", "tank", "data", "tank", has("tank"), "tank"},
		{"本机没记过、目录里那个本机确实有，才用它（单机升级上来的老装机）",
			"", "mypool", "tank", has("mypool"), "mypool"},
		{"本机没记过、目录里那个本机没有——那是别台的名字，退回本机 env",
			"", "data", "tank", has("tank"), "tank"},
		{"两边都没有：维持 env 默认（无池装机）", "", "", "tank", has(), "tank"},
		{"本机记的优先于一切，哪怕目录里是另一个", "ssd", "data", "tank", has("ssd", "data"), "ssd"},
	}
	for _, c := range cases {
		if got := resolveDataPool(c.local, c.persisted, c.env, c.exists); got != c.want {
			t.Fatalf("%s：resolveDataPool(%q,%q,%q) = %q，want %q",
				c.name, c.local, c.persisted, c.env, got, c.want)
		}
	}
}

// 算出来的池名必须落盘，否则每次重启都退回会复制的 system_settings.data_pool 或集群级的 NDISKLESS_POOL，
// 可能绑到本机不存在的池：
//
//	zfs recv -F -u -s tank/nd → cannot open 'tank': dataset does not exist
//
// 此时复制每轮失败、healthz 503，界面上却看不出异常。
func TestBindDataPoolRecordsWhatItResolvedSoARestartDoesNotGuessAgain(t *testing.T) {
	has := func(names ...string) func(string) bool {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		return func(p string) bool { return set[p] }
	}

	// 本机没记过，但目录里那个名字本机确实有：绑它并记下来。
	var wrote string
	pool, err := bindDataPool("", "mypool", "tank", has("mypool"),
		func(p string) error { wrote = p; return nil })
	if err != nil || pool != "mypool" {
		t.Fatalf("绑定结果 = %q, %v，want mypool", pool, err)
	}
	if wrote != "mypool" {
		t.Fatalf("解析出来的池名必须落盘，实际写入 %q", wrote)
	}

	// 无池装机：解析退回 env 的名字，而本机没有这个池。不能钉死，否则运维建了别名的池后这台仍找那个不存在的。
	wrote = ""
	pool, err = bindDataPool("", "", "tank", has(), func(p string) error { wrote = p; return nil })
	if err != nil || pool != "tank" {
		t.Fatalf("无池装机应维持 env 默认，得到 %q, %v", pool, err)
	}
	if wrote != "" {
		t.Fatalf("本机没有这个池时不该落盘，却写了 %q", wrote)
	}

	// 已经记过就不再写：本机记录是最高权威。
	wrote = ""
	pool, err = bindDataPool("ssd", "data", "tank", has("ssd", "data"),
		func(p string) error { wrote = p; return nil })
	if err != nil || pool != "ssd" {
		t.Fatalf("本机记录优先，得到 %q, %v", pool, err)
	}
	if wrote != "" {
		t.Fatalf("已有本机记录时不该重写，却写了 %q", wrote)
	}

	// 落盘失败要报出来，但不能让服务起不来：池是好的，只是没记下来。
	pool, err = bindDataPool("", "mypool", "tank", has("mypool"),
		func(string) error { return errors.New("只读文件系统") })
	if pool != "mypool" {
		t.Fatalf("落盘失败不该改变绑定结果，得到 %q", pool)
	}
	if err == nil {
		t.Fatal("落盘失败要返回错误，让调用方 warn 出来")
	}
}
