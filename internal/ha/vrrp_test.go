package ha

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sampleKeepalived = `global_defs {
    script_user root
}
vrrp_instance ndiskless {
    state BACKUP
    interface ens33
    priority 150
    unicast_src_ip 192.168.10.3
    unicast_peer {
        192.168.10.4
    }
    virtual_ipaddress {
        192.168.10.250
    }
}
`

func TestUnicastPeersParseAndRewrite(t *testing.T) {
	if got := UnicastPeers(sampleKeepalived); !reflect.DeepEqual(got, []string{"192.168.10.4"}) {
		t.Fatalf("parsed peers=%v", got)
	}
	out, err := WithUnicastPeers(sampleKeepalived, []string{"192.168.10.4", "192.168.10.5"})
	if err != nil {
		t.Fatal(err)
	}
	if got := UnicastPeers(out); !reflect.DeepEqual(got, []string{"192.168.10.4", "192.168.10.5"}) {
		t.Fatalf("rewritten peers=%v\n%s", got, out)
	}
	// 块以外的内容（VIP、priority、interface）保持不变。
	for _, keep := range []string{"priority 150", "unicast_src_ip 192.168.10.3", "192.168.10.250", "interface ens33"} {
		if !contains(out, keep) {
			t.Fatalf("rewrite lost %q:\n%s", keep, out)
		}
	}
	// 没有我们这个块的配置（不是我们写的，或被手工删掉）直接拒绝，绝不盲改。
	if _, err := WithUnicastPeers("vrrp_instance other {\n}\n", []string{"1.1.1.1"}); err == nil {
		t.Fatal("config without unicast_peer block should be refused")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 对账器让 keepalived 的 peer 列表跟随名册：名册新增的加上、减少的去掉，从不列出自己，只在文件变化时重载 keepalived。
func TestPeerReconcilerFollowsRoster(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "keepalived.conf")
	if err := os.WriteFile(conf, []byte(sampleKeepalived), 0o600); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	roster := []string{"192.168.10.3", "192.168.10.4", "192.168.10.5"}
	r := PeerReconciler{
		ConfPath: conf, Self: "192.168.10.3",
		Roster: func(context.Context) ([]string, error) { return roster, nil },
		Reload: func(context.Context) error { reloads++; return nil },
	}

	changed, err := r.Once(context.Background())
	if err != nil || !changed || reloads != 1 {
		t.Fatalf("first pass: changed=%v err=%v reloads=%d", changed, err, reloads)
	}
	data, _ := os.ReadFile(conf)
	if got := UnicastPeers(string(data)); !reflect.DeepEqual(got, []string{"192.168.10.4", "192.168.10.5"}) {
		t.Fatalf("peers after reconcile=%v", got)
	}
	if fi, _ := os.Stat(conf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v, keepalived.conf carries the VRRP password and must stay 0600", fi.Mode().Perm())
	}

	// 稳态：无事可做，不重载。
	changed, err = r.Once(context.Background())
	if err != nil || changed || reloads != 1 {
		t.Fatalf("steady pass: changed=%v err=%v reloads=%d", changed, err, reloads)
	}
	// 集合相同、按安装器（名册）顺序排列也不算偏差。
	unsorted, _ := WithUnicastPeers(sampleKeepalived, []string{"192.168.10.5", "192.168.10.4"})
	_ = os.WriteFile(conf, []byte(unsorted), 0o600)
	changed, err = r.Once(context.Background())
	if err != nil || changed || reloads != 1 {
		t.Fatalf("reordered pass: changed=%v err=%v reloads=%d", changed, err, reloads)
	}

	// 节点离开名册：从列表中去掉。
	roster = []string{"192.168.10.3", "192.168.10.5"}
	changed, _ = r.Once(context.Background())
	data, _ = os.ReadFile(conf)
	if !changed || !reflect.DeepEqual(UnicastPeers(string(data)), []string{"192.168.10.5"}) {
		t.Fatalf("after leave: changed=%v peers=%v", changed, UnicastPeers(string(data)))
	}

	// 名册返回空（DB 抖动）不能清空 peer 列表。
	roster = nil
	changed, err = r.Once(context.Background())
	data, _ = os.ReadFile(conf)
	if changed || err == nil || !reflect.DeepEqual(UnicastPeers(string(data)), []string{"192.168.10.5"}) {
		t.Fatalf("empty roster: changed=%v err=%v peers=%v", changed, err, UnicastPeers(string(data)))
	}
}

func TestPeerReconcilerSkipsWhenNoConfig(t *testing.T) {
	r := PeerReconciler{ConfPath: filepath.Join(t.TempDir(), "nope.conf"), Self: "1.1.1.1",
		Roster: func(context.Context) ([]string, error) { return []string{"1.1.1.1", "2.2.2.2"}, nil },
		Reload: func(context.Context) error { t.Fatal("must not reload"); return nil }}
	if changed, err := r.Once(context.Background()); changed || err != nil {
		t.Fatalf("no config: changed=%v err=%v", changed, err)
	}
}

// 从界面加入的节点已写好 keepalived 配置但未启动；必须等写入者把它列为 VRRP peer 才能启动，
// 否则收不到通告会自选为 master，VIP 落到两台机器上。标记文件代表「尚未启动」，加入中途重启也保留。
func TestJoinFinisherWaitsForWriterToListUs(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "keepalived.pending")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	listed := false
	enabled := 0
	f := JoinFinisher{
		Marker: marker, Self: "192.168.10.5",
		Writer: func(context.Context) (PeerStatus, error) {
			if listed {
				return PeerStatus{Role: "active", VRRPPeers: []string{"192.168.10.4", "192.168.10.5"}}, nil
			}
			return PeerStatus{Role: "active", VRRPPeers: []string{"192.168.10.4"}}, nil
		},
		Enable: func(context.Context) error { enabled++; return nil },
	}

	// 尚未被列出：等待、保留标记、不启动。
	done, err := f.Once(context.Background())
	if done || err != nil || enabled != 0 {
		t.Fatalf("not listed: done=%v err=%v enabled=%d", done, err, enabled)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("marker must survive while waiting")
	}

	// 写入者不可达：照样等待（绝不盲目启动）。
	f.Writer = func(context.Context) (PeerStatus, error) { return PeerStatus{}, errors.New("refused") }
	if done, _ := f.Once(context.Background()); done || enabled != 0 {
		t.Fatalf("unreachable writer: done=%v enabled=%d", done, enabled)
	}

	// 已被列出：启动 keepalived、删除标记、报告完成。
	listed = true
	f.Writer = func(context.Context) (PeerStatus, error) {
		return PeerStatus{Role: "active", VRRPPeers: []string{"192.168.10.4", "192.168.10.5"}}, nil
	}
	done, err = f.Once(context.Background())
	if !done || err != nil || enabled != 1 {
		t.Fatalf("listed: done=%v err=%v enabled=%d", done, err, enabled)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker must be removed once keepalived is started")
	}

	// 根本没有标记：无待办，立即完成，不探测。
	f.Writer = func(context.Context) (PeerStatus, error) { t.Fatal("must not probe"); return PeerStatus{}, nil }
	if done, err := f.Once(context.Background()); !done || err != nil {
		t.Fatalf("no marker: done=%v err=%v", done, err)
	}
}

// 备机的 priority 也必须跟随名册，不能停在装机那一刻。排名是为了让备机不同分（原因见 StandbyPriority），
// 但装机/加入时只按当时的 --peer 算过一次，先加入的节点看到的列表更短，之后名册变长也不重算：
//
//	.4 装机时看到 .3,.5 → 排第 2 → 101
//	.5 加入时只看到 .3   → 排第 2 → 101   ← 少了一台，撞了
//
// 单播列表已在这里按名册对账，priority 是同一份事实的另一半。
func TestPeerReconcilerKeepsStandbyPriorityInStepWithTheRoster(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "keepalived.conf")
	// 本机是 .5，装机时只看到 .3，于是 priority 停在 101
	if err := os.WriteFile(conf, []byte(`vrrp_instance ND {
    state BACKUP
    priority 101
    unicast_src_ip 192.168.10.5
    unicast_peer {
        192.168.10.3
    }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloaded int
	r := PeerReconciler{
		ConfPath: conf, Self: "192.168.10.5",
		Roster: func(context.Context) ([]string, error) {
			return []string{"192.168.10.3", "192.168.10.4", "192.168.10.5"}, nil
		},
		Reload: func(context.Context) error { reloaded++; return nil },
	}
	changed, err := r.Once(context.Background())
	if err != nil || !changed {
		t.Fatalf("changed = %v err = %v", changed, err)
	}
	out, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// .3 / .4 / .5 里 .5 排第三 → 100 + 2
	if !strings.Contains(string(out), "priority 102") {
		t.Fatalf("priority 没跟着花名册走：\n%s", out)
	}
}

// 主机的 150 档不能被排名覆盖：它必须一直赢，否则写入者会被备机顶下去。
func TestPeerReconcilerLeavesTheWritersPriorityAlone(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "keepalived.conf")
	if err := os.WriteFile(conf, []byte(`vrrp_instance ND {
    state BACKUP
    priority 150
    unicast_src_ip 192.168.10.5
    unicast_peer {
        192.168.10.3
    }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := PeerReconciler{
		ConfPath: conf, Self: "192.168.10.5",
		Roster: func(context.Context) ([]string, error) {
			return []string{"192.168.10.3", "192.168.10.4", "192.168.10.5"}, nil
		},
		Reload: func(context.Context) error { return nil },
	}
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(conf)
	if !strings.Contains(string(out), "priority 150") {
		t.Fatalf("主机档位被改掉了：\n%s", out)
	}
}

// 真机配置的 priority 行尾带注释，断言必须按产品真正写出的那一行来；只要求数字后直接换行的正则在真机上匹配不到。
// 用 deploy/keepalived.conf.tmpl 那一行的真实形状来钉住。
func TestStandbyPriorityRewriteHandlesTheTemplatesTrailingComment(t *testing.T) {
	conf := "vrrp_instance ND {\n" +
		"    state BACKUP\n" +
		"    priority 101     # 主机 150；备机 100 起按地址排序递增，不得同分\n" +
		"}\n"
	out, changed := WithStandbyPriority(conf, 102)
	if !changed {
		t.Fatalf("带注释的 priority 行没被认出来：\n%s", conf)
	}
	if !strings.Contains(out, "priority 102") {
		t.Fatalf("没改成 102：\n%s", out)
	}
	// 注释是给运维看的说明，改档位不能把它抹掉
	if !strings.Contains(out, "# 主机 150") {
		t.Fatalf("行尾注释被吃掉了：\n%s", out)
	}
	// 主机档位照旧不动
	writer := strings.Replace(conf, "priority 101", "priority 150", 1)
	if _, changed := WithStandbyPriority(writer, 102); changed {
		t.Fatal("主机的 150 不该被排名覆盖")
	}
}
