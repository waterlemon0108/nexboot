package ha

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
)

// 控制台要能在客户机网段里认出可拉进来的空机器。已在册的节点、非出厂态机器、探不通的地址都不能出现在候选里。
func TestDiscoverListsOnlyFactoryNodesNotAlreadyInTheCluster(t *testing.T) {
	empty := httptest.NewServer(AdoptHandler{NodeID: "empty-1",
		Factory: func(context.Context) bool { return true }})
	defer empty.Close()
	used := httptest.NewServer(AdoptHandler{NodeID: "has-pool",
		Factory: func(context.Context) bool { return false }})
	defer used.Close()
	member := httptest.NewServer(AdoptHandler{NodeID: "already-in",
		Factory: func(context.Context) bool { return true }})
	defer member.Close()

	hostOf := func(s *httptest.Server) string {
		u, _ := url.Parse(s.URL)
		return u.Hostname() + ":" + u.Port()
	}
	d := Discovery{
		Candidates: func(context.Context) []string {
			return []string{hostOf(empty), hostOf(used), hostOf(member), "127.0.0.1:1"}
		},
		InCluster: func(nodeID string) bool { return nodeID == "already-in" },
	}
	found, err := d.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].NodeID != "empty-1" {
		t.Fatalf("候选 = %#v，只该有那台空节点", found)
	}
	if found[0].Address == "" {
		t.Fatal("候选要带上地址，否则控制台没法纳管它")
	}
}

// 纳管：递过去集群自己知道的 VIP 与令牌，运维一个字段都不用填。
func TestAdoptNodeHandsOverWhatTheClusterAlreadyKnows(t *testing.T) {
	var got JoinRequest
	adopted := httptest.NewServer(AdoptHandler{NodeID: "n2",
		Factory: func(context.Context) bool { return true },
		Adopt:   func(_ context.Context, r JoinRequest) error { got = r; return nil }})
	defer adopted.Close()
	u, _ := url.Parse(adopted.URL)

	d := Discovery{VIP: "192.168.10.250", Token: "cluster-secret"}
	if err := d.AdoptNode(context.Background(), u.Hostname()+":"+u.Port()); err != nil {
		t.Fatal(err)
	}
	if got.VIP != "192.168.10.250" || got.ClusterToken != "cluster-secret" {
		t.Fatalf("递过去的是 %#v", got)
	}
}

func TestAdoptNodeSurfacesARefusal(t *testing.T) {
	// 非出厂态机器拒绝纳管：控制台要把原因原样告诉运维，而不是只说「失败了」。
	busy := httptest.NewServer(AdoptHandler{NodeID: "n3",
		Factory: func(context.Context) bool { return false }})
	defer busy.Close()
	u, _ := url.Parse(busy.URL)
	d := Discovery{VIP: "10.0.0.1", Token: "t"}
	err := d.AdoptNode(context.Background(), u.Hostname()+":"+u.Port())
	if err == nil || !errors.Is(err, ErrNotAdoptable) {
		t.Fatalf("want ErrNotAdoptable, got %v", err)
	}
}

// 候选限定在客户机网段且有上限：一个 /8 就是一千六百万个地址，那是网络扫荡而不是发现空节点。
func TestSubnetCandidatesStaysInsideOneMachineRoom(t *testing.T) {
	got := SubnetCandidates("192.168.10.0/29", "8080", map[string]bool{"192.168.10.3": true})
	want := []string{"192.168.10.1:8080", "192.168.10.2:8080", "192.168.10.4:8080",
		"192.168.10.5:8080", "192.168.10.6:8080", "192.168.10.7:8080"}
	if len(got) != len(want) {
		t.Fatalf("候选 = %v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选 = %v，want %v", got, want)
		}
	}
	if n := len(SubnetCandidates("10.0.0.0/8", "8080", nil)); n != 0 {
		t.Fatalf("过大的网段应当直接不扫，得到 %d 个候选", n)
	}
	if n := len(SubnetCandidates("不是网段", "8080", nil)); n != 0 {
		t.Fatalf("解析不了的网段应当返回空，得到 %d", n)
	}
}
