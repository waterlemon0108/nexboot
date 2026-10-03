package zfs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Dataset 和 Snapshot 把名字路由进 v2 容器：目录实体在 nd/，单机克隆在 run/。所有调用方都经这两个函数拼路径。
func TestDatasetRoutesIntoContainers(t *testing.T) {
	client := newTestClient(&fakeRunner{})
	cases := map[string]string{
		"win11":                      "tank/nd/win11",
		"win11_default":              "tank/nd/win11_default",
		"CLIENT-AABBCCDDEEFF":        "tank/run/CLIENT-AABBCCDDEEFF",
		"CLIENT-AABBCCDDEEFF-DATA-1": "tank/run/CLIENT-AABBCCDDEEFF-DATA-1",
		"SCLIENT-AABBCCDDEEFF":       "tank/run/SCLIENT-AABBCCDDEEFF",
		"INSPECT-WIN11-9":            "tank/run/INSPECT-WIN11-9",
	}
	for name, want := range cases {
		if got := client.Dataset(name); got != want {
			t.Errorf("Dataset(%q) = %q, want %q", name, got, want)
		}
	}
	if got := client.Snapshot("win11_default", "0"); got != "tank/nd/win11_default@0" {
		t.Errorf("Snapshot = %q", got)
	}
}

// 新池或已迁移的池放行；顶层有散落数据集的旧布局须拒绝，并在消息里给出迁移命令。
func TestEnsureLayoutStates(t *testing.T) {
	// 已是 v2 且两个容器都在：只读
	runner := &sequenceRunner{outs: [][]byte{[]byte("2\n"), []byte("tank\ntank/nd\ntank/run\n")}}
	client := newTestClient(runner)
	if err := client.EnsureLayout(context.Background()); err != nil {
		t.Fatalf("v2 pool: %v", err)
	}

	// 标记为 v2 但容器被手工删了：重建它，否则 nd/ 没了的备机永远收不了复制。
	runner = &sequenceRunner{outs: [][]byte{[]byte("2\n"), []byte("tank\ntank/run\n"), nil}}
	client = newTestClient(runner)
	if err := client.EnsureLayout(context.Background()); err != nil {
		t.Fatalf("v2 missing container: %v", err)
	}
	recreated := false
	for _, call := range runner.calls {
		if reflect.DeepEqual(call.args, []string{"create", "-o", "mountpoint=none", "tank/nd"}) {
			recreated = true
		}
	}
	if !recreated {
		t.Fatalf("missing container must be recreated: %#v", runner.calls)
	}

	// 空池：初始化容器并打标记
	runner = &sequenceRunner{outs: [][]byte{
		[]byte("-\n"),    // get ndiskless:layout
		[]byte("tank\n"), // list -d 1：只有池本身
		nil,              // create nd
		nil,              // create run
		nil,              // set property
	}}
	client = newTestClient(runner)
	if err := client.EnsureLayout(context.Background()); err != nil {
		t.Fatalf("empty pool: %v", err)
	}
	var created, stamped bool
	for _, call := range runner.calls {
		if reflect.DeepEqual(call.args, []string{"create", "-o", "mountpoint=none", "tank/nd"}) {
			created = true
		}
		if reflect.DeepEqual(call.args, []string{"set", "ndiskless:layout=2", "tank"}) {
			stamped = true
		}
	}
	if !created || !stamped {
		t.Fatalf("init calls = %#v", runner.calls)
	}

	// 旧布局：顶层有散落数据集
	runner = &sequenceRunner{outs: [][]byte{
		[]byte("-\n"),
		[]byte("tank\ntank/win11\ntank/win11_default\n"),
	}}
	client = newTestClient(runner)
	err := client.EnsureLayout(context.Background())
	if err == nil {
		t.Fatal("old layout must be refused")
	}
	if !strings.Contains(err.Error(), "migrate-layout") {
		t.Fatalf("error must tell the operator the command: %v", err)
	}
}

// 迁移把顶层散落数据集改名进容器并打标记；在已迁移的池上再跑是空操作。
func TestMigrateLayoutMovesAndStamps(t *testing.T) {
	runner := &sequenceRunner{outs: [][]byte{
		[]byte("-\n"), // 属性
		[]byte("tank\ntank/imports\ntank/win11\ntank/win11_default\ntank/CLIENT-AABBCCDDEEFF\ntank/SCLIENT-AABBCCDDEEFF\n"),
		nil, nil, // create nd、run
		nil, nil, nil, nil, // 改名
		nil, // set property
	}}
	client := newTestClient(runner)
	moved, err := client.MigrateLayout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if moved != 4 {
		t.Fatalf("moved = %d, want 4", moved)
	}
	var renames [][]string
	for _, call := range runner.calls {
		if len(call.args) > 0 && call.args[0] == "rename" {
			renames = append(renames, call.args)
		}
	}
	want := [][]string{
		{"rename", "tank/win11", "tank/nd/win11"},
		{"rename", "tank/win11_default", "tank/nd/win11_default"},
		{"rename", "tank/CLIENT-AABBCCDDEEFF", "tank/run/CLIENT-AABBCCDDEEFF"},
		{"rename", "tank/SCLIENT-AABBCCDDEEFF", "tank/run/SCLIENT-AABBCCDDEEFF"},
	}
	if !reflect.DeepEqual(renames, want) {
		t.Fatalf("renames = %#v", renames)
	}

	// 已迁移：属性为 2，无需移动（imports 永远留在顶层，见 keepTopLevel）
	runner = &sequenceRunner{outs: [][]byte{[]byte("2\n"), []byte("tank\ntank/nd\ntank/run\ntank/imports\n")}}
	client = newTestClient(runner)
	moved, err = client.MigrateLayout(context.Background())
	if err != nil || moved != 0 {
		t.Fatalf("idempotent run: moved=%d err=%v", moved, err)
	}
}

// 库副本放在目录容器里，一个递归快照就能把行和数据集一起冻结；数据集首次使用时创建，须挂到 VACUUM INTO 可写的位置。
func TestEnsureDBCopyDatasetCreatesAndMounts(t *testing.T) {
	// 首次使用：数据集不存在，以显式 mountpoint 创建
	runner := &sequenceRunner{
		outs: [][]byte{[]byte("cannot open 'tank/nd/db': dataset does not exist\n"), nil},
		err:  fmt.Errorf("exit status 1"), errAt: 1,
	}
	client := newTestClient(runner)
	dir, err := client.EnsureDBCopyDataset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("dir = %q", dir)
	}
	want := fakeCall{name: "zfs", args: []string{"create", "-p", "-o", "mountpoint=/ndiskless/tank/nd/db", "tank/nd/db"}}
	if !reflect.DeepEqual(runner.calls[1], want) {
		t.Fatalf("create call = %#v", runner.calls[1])
	}

	// 之后：数据集在本节点自己的路径、已挂载且可读，只读两个属性。
	// 读不通要重挂，见 TestEnsureDBCopyDatasetRepairsABrokenMount。
	runner = &sequenceRunner{outs: [][]byte{[]byte("/ndiskless/tank/nd/db\n"), []byte("yes\n")}}
	client = newTestClient(runner)
	client.statDir = func(string) error { return nil } // 挂载读得通
	dir, err = client.EnsureDBCopyDataset(context.Background())
	if err != nil || dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("dir = %q err = %v", dir, err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("existing mounted dataset must not be touched: %#v", runner.calls)
	}

	// 备机副本经 recv -u 收下：mountpoint 已设但未挂载，回答前先挂上。
	runner = &sequenceRunner{outs: [][]byte{[]byte("/ndiskless/tank/nd/db\n"), []byte("no\n"), nil}}
	client = newTestClient(runner)
	dir, err = client.EnsureDBCopyDataset(context.Background())
	if err != nil || dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("dir = %q err = %v", dir, err)
	}
	mounted := false
	for _, call := range runner.calls {
		if len(call.args) == 2 && call.args[0] == "mount" && call.args[1] == "tank/nd/db" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("unmounted copy must be mounted: %#v", runner.calls)
	}
}

// 挂着但读不了：recv -F 在挂载中回滚后内核挂载失效、读报 EIO，而 mounted 属性仍为 yes。须重挂，否则备机永远不健康。
func TestEnsureDBCopyDatasetRepairsABrokenMount(t *testing.T) {
	runner := &sequenceRunner{outs: [][]byte{[]byte("/ndiskless/tank/nd/db\n"), []byte("yes\n"), nil, nil}}
	client := newTestClient(runner)
	// 挂载点正确、mounted=yes，但读取报错。
	client.statDir = func(string) error { return errors.New("input/output error") }
	dir, err := client.EnsureDBCopyDataset(context.Background())
	if err != nil || dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("dir = %q err = %v", dir, err)
	}
	var unmounted, mounted bool
	for _, c := range runner.calls {
		if len(c.args) >= 2 && c.args[0] == "unmount" {
			unmounted = true
		}
		if len(c.args) >= 2 && c.args[0] == "mount" {
			mounted = true
		}
	}
	if !unmounted || !mounted {
		t.Fatalf("坏掉的挂载要强制重挂，实际调用：%#v", runner.calls)
	}
}

// 复制流带来写入者的 mountpoint（含别台池名的绝对路径），须改回本机路径；
// 原样接受只是侥幸能用，本机日后建同名池就会冲突。
func TestEnsureDBCopyDatasetRepointsAMountpointInheritedFromTheWriter(t *testing.T) {
	// 已存在、已挂载，但挂载点带的是别台的池名
	runner := &sequenceRunner{outs: [][]byte{[]byte("/data/nd/db\n"), nil, nil}}
	client := newTestClient(runner) // 本机池是 tank
	dir, err := client.EnsureDBCopyDataset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("必须回到本机路径，得到 %q", dir)
	}
	want := fakeCall{name: "zfs", args: []string{"set", "mountpoint=/ndiskless/tank/nd/db", "tank/nd/db"}}
	if !containsCall(runner.calls, want) {
		t.Fatalf("没有把挂载点掰回来。calls = %#v", runner.calls)
	}
}

// 改回挂载点后还得真的挂上：`zfs set mountpoint=` 对 recv -u 收下的未挂载副本不会顺带挂载，
// 只改属性就返回，备机的库仍不在盘上。
func TestEnsureDBCopyDatasetMountsAfterRepointingAnUnmountedCopy(t *testing.T) {
	runner := &sequenceRunner{outs: [][]byte{
		[]byte("/data/nd/db\n"), // 带着写入者池名的挂载点
		nil,                     // set mountpoint
		[]byte("no\n"),          // 还没挂上
		nil,                     // mount
	}}
	client := newTestClient(runner)
	client.statDir = func(string) error { return nil }
	dir, err := client.EnsureDBCopyDataset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/ndiskless/tank/nd/db" {
		t.Fatalf("dir = %q", dir)
	}
	if !containsCall(runner.calls, fakeCall{name: "zfs", args: []string{"mount", "tank/nd/db"}}) {
		t.Fatalf("掰回挂载点之后没有挂上，calls = %#v", runner.calls)
	}
}
