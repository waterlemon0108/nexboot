package ha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adoptSrv(h AdoptHandler) *httptest.Server { return httptest.NewServer(h) }

// 出厂态 = 没有数据池且不在任何集群里。此时节点上按定义没有任何数据，可以被集群直接纳管，
// 运维不必到它的界面上转述 VIP 和令牌。
func TestIdentityReportsFactoryStateWithoutLeakingSecrets(t *testing.T) {
	factory := true
	srv := adoptSrv(AdoptHandler{
		NodeID: "n9", Version: "v1.2.3",
		Factory: func(context.Context) bool { return factory },
		Adopt:   func(context.Context, JoinRequest) error { return nil },
	})
	defer srv.Close()

	get := func() (int, string) {
		resp, err := http.Get(srv.URL + "/internal/cluster/identity")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// 免认证可读：集群要靠它在网段里认出「哪几台是空的」。
	st, body := get()
	if st != http.StatusOK {
		t.Fatalf("identity 应免认证可读，得到 %d", st)
	}
	var id NodeIdentity
	if err := json.Unmarshal([]byte(body), &id); err != nil {
		t.Fatal(err)
	}
	if !id.Adoptable || id.NodeID != "n9" || id.Version != "v1.2.3" {
		t.Fatalf("identity = %#v", id)
	}
	// 它只说「我是谁、我空不空」，绝不能带出任何密钥。
	for _, leak := range []string{"secret", "token", "password", "jwt", "chap"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("identity 泄露了 %q：%s", leak, body)
		}
	}

	factory = false
	if _, body := get(); !strings.Contains(body, `"adoptable":false`) {
		t.Fatalf("非出厂态要如实说 adoptable=false：%s", body)
	}
}

func TestAdoptOnlyInFactoryState(t *testing.T) {
	post := func(h AdoptHandler) int {
		srv := adoptSrv(h)
		defer srv.Close()
		resp, err := http.Post(srv.URL+"/internal/cluster/adopt", "application/json",
			strings.NewReader(`{"vip":"192.168.10.250","cluster_token":"tok"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	called := 0
	base := AdoptHandler{
		NodeID:  "n9",
		Factory: func(context.Context) bool { return true },
		Adopt:   func(context.Context, JoinRequest) error { called++; return nil },
	}

	// 出厂态：受理。不要求先有池，「先纳管、再从控制台建池」正是目的。
	if code := post(base); code != http.StatusAccepted {
		t.Fatalf("出厂态应受理，得到 %d", code)
	}
	if called != 1 {
		t.Fatalf("纳管没有真正执行，called=%d", called)
	}

	// 非出厂态（有池或已入盟）：一律拒绝。纳管会把它变成备机并被复制流回滚，是静默的数据销毁。
	notFactory := base
	notFactory.Factory = func(context.Context) bool { return false }
	if code := post(notFactory); code != http.StatusConflict {
		t.Fatalf("非出厂态必须拒绝，得到 %d", code)
	}

	// 现场可以整体关掉纳管。
	off := base
	off.Disabled = true
	if code := post(off); code != http.StatusForbidden {
		t.Fatalf("关掉纳管后应拒绝，得到 %d", code)
	}
	// 关掉之后连 identity 都不能自称可纳管
	srv := adoptSrv(off)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/internal/cluster/identity")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"adoptable":false`) {
		t.Fatalf("纳管关掉时 identity 要说 adoptable=false：%s", raw)
	}
}

// 纳管走的是和「界面加入」同一条路，只是不要求本机先有池：出厂态节点本来就没有。
func TestAdoptDoesNotRequireAPool(t *testing.T) {
	js, got := joinStub()
	js.HasPool = func(context.Context) bool { return false }
	js.LocalPool = func() string { return "" } // 还没建池
	if err := js.Adopt(context.Background(), JoinRequest{VIP: "10.0.0.1", ClusterToken: "tok"}); err != nil {
		t.Fatalf("出厂态纳管不该被「先建池」挡住：%v", err)
	}
	// 集群池名要写进它的 env，运维在控制台给它建池时就用这个名字。
	if got.Pool != "ndpool" || got.Role != "standby" {
		t.Fatalf("params = %#v", *got)
	}
}
