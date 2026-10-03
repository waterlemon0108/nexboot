package ha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func createStub() (*CreateService, *JoinParams) {
	var got JoinParams
	cs := &CreateService{
		AlreadyClustered: func() bool { return false },
		LocalPool:        func() string { return "ndpool" },
		OnLocalSubnet:    func(string) bool { return true },
		ResolveSelfAddr:  func(string) (string, error) { return "192.168.10.3", nil },
		NewToken:         func() (string, error) { return "generated-token", nil },
		Reconfigure:      func(_ context.Context, p JoinParams) error { got = p; return nil },
	}
	return cs, &got
}

// 建集群只问虚 IP。令牌是集群内部的共享密钥，由产品生成（与签名密钥、CHAP 密钥同样做法），运维无需知道也不该自己编。
func TestCreateClusterGeneratesTheTokenItself(t *testing.T) {
	cs, got := createStub()
	if err := cs.Create(context.Background(), CreateRequest{VIP: "192.168.10.250"}); err != nil {
		t.Fatal(err)
	}
	if got.Token != "generated-token" {
		t.Fatalf("令牌应由产品生成，得到 %q", got.Token)
	}
	if got.VIP != "192.168.10.250" || got.Role != "active" || got.NodeAddr != "192.168.10.3" || got.Pool != "ndpool" {
		t.Fatalf("params = %#v", *got)
	}
}

func TestCreateClusterGuards(t *testing.T) {
	t.Run("虚 IP 必填", func(t *testing.T) {
		cs, _ := createStub()
		if err := cs.Create(context.Background(), CreateRequest{VIP: "  "}); err == nil {
			t.Fatal("空虚 IP 应被拒")
		}
	})
	t.Run("已在集群中", func(t *testing.T) {
		cs, _ := createStub()
		cs.AlreadyClustered = func() bool { return true }
		if err := cs.Create(context.Background(), CreateRequest{VIP: "10.0.0.1"}); !errors.Is(err, ErrAlreadyClustered) {
			t.Fatalf("want ErrAlreadyClustered, got %v", err)
		}
	})
	t.Run("虚 IP 不在本机任何网段内", func(t *testing.T) {
		// keepalived 要把 VIP 挂到某块网卡上，不在任何网卡网段内就无处可挂。
		// 这个错误发生在分离进程里运维看不到，必须在受理时就挡下。
		cs, _ := createStub()
		cs.OnLocalSubnet = func(string) bool { return false }
		err := cs.Create(context.Background(), CreateRequest{VIP: "10.99.0.1"})
		if err == nil || !strings.Contains(err.Error(), "网段") {
			t.Fatalf("应说清虚 IP 不在本机网段内，得到 %v", err)
		}
	})
}

// HTTP 面：受理返回 202（重配在分离进程中进行，本进程随后重启），各种拒绝要能被界面区分。
func TestCreateHandlerStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		tune func(*CreateService)
		want int
	}{
		{"ok", func(*CreateService) {}, http.StatusAccepted},
		{"已入盟", func(c *CreateService) { c.AlreadyClustered = func() bool { return true } }, http.StatusConflict},
		{"网段不符", func(c *CreateService) { c.OnLocalSubnet = func(string) bool { return false } }, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, _ := createStub()
			tc.tune(cs)
			req := httptest.NewRequest("POST", "/api/cluster/create", strings.NewReader(`{"vip":"192.168.10.250"}`))
			rec := httptest.NewRecorder()
			cs.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: code=%d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// 先建集群后建池：没池的主机健康检查照样通过（见 nodeHealth），VIP 才落得下来，运维才能经 VIP 给各台建池。
func TestCreateClusterWithoutAPoolYet(t *testing.T) {
	cs, got := createStub()
	cs.LocalPool = func() string { return "" }
	if err := cs.Create(context.Background(), CreateRequest{VIP: "192.168.10.250"}); err != nil {
		t.Fatal(err)
	}
	if got.Role != "active" || got.Pool != "" {
		t.Fatalf("params = %#v", *got)
	}
}

// VIP 落进分组地址范围或已被客户机占用，建完立刻冲突：提交时就拦，并点名是谁在用。
func TestCreateClusterRefusesAVIPAlreadyInUse(t *testing.T) {
	reconfigured := false
	s := CreateService{
		OnLocalSubnet:   func(string) bool { return true },
		ResolveSelfAddr: func(string) (string, error) { return "192.168.10.3", nil },
		NewToken:        func() (string, error) { return "tok", nil },
		AddressInUse: func(_ context.Context, ip string) []string {
			return []string{"分组「大厅散座」的地址范围"}
		},
		Reconfigure: func(context.Context, JoinParams) error { reconfigured = true; return nil },
	}
	err := s.Create(context.Background(), CreateRequest{VIP: "192.168.10.25"})
	if err == nil || !strings.Contains(err.Error(), "大厅散座") || !strings.Contains(err.Error(), "192.168.10.25") || reconfigured {
		t.Fatalf("err = %v, reconfigured = %v", err, reconfigured)
	}
}
