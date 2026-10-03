package storage

import "testing"

// 各节点数据池都叫 tank 且库整体复制，池 ID 必须带节点，否则多台的池记录会压成一行，
// 只有最后写入的节点显示有存储。
func TestPoolIDIsScopedToItsNode(t *testing.T) {
	a := PoolID("2f454fa237eb", "tank")
	b := PoolID("c9ba4e557884", "tank")
	if a == b {
		t.Fatalf("两台机器上的 tank 撞成了同一行：%s", a)
	}
	// 同一台上的同名池 ID 必须稳定，否则每次重启都算成新池。
	if PoolID("2f454fa237eb", "tank") != a {
		t.Fatal("同一台同一个池，ID 必须稳定")
	}
	// 名字里的分隔符不能让节点段和池名段混淆。
	if PoolID("node-a", "b") == PoolID("node", "a-b") {
		t.Fatal("节点段与名字段被拼接歧义了")
	}
}
