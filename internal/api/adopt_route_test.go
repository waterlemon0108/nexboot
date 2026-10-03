package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/ha"
)

// 纳管的两个端点必须免认证：新装的机器还没有集群令牌，也没人登录过。安全边界是「出厂态」前提，
// 有池或已入盟一律拒绝。同时确认它们没被同前缀的令牌门（/internal/cluster/*）挡掉。
func TestAdoptEndpointsAreUnauthenticatedButFactoryGated(t *testing.T) {
	factory := true
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{
		ClusterToken: "cluster-secret",
		ClusterHandler: ha.ClusterHandler{
			Registry: stubRegistry{}, Token: "cluster-secret",
		},
		AdoptHandler: ha.AdoptHandler{
			NodeID: "fresh-1", Version: "v0",
			Factory: func(context.Context) bool { return factory },
			Adopt:   func(context.Context, ha.JoinRequest) error { return nil },
		},
	})

	do := func(method, path, body string) (int, string) {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r) // 不带任何 Authorization
		return rec.Code, rec.Body.String()
	}

	// identity：免认证可读，且不带任何密钥。
	code, body := do(http.MethodGet, "/internal/cluster/identity", "")
	if code != http.StatusOK {
		t.Fatalf("identity 免认证应可读，得到 %d %s", code, body)
	}
	if !strings.Contains(body, `"adoptable":true`) || !strings.Contains(body, "fresh-1") {
		t.Fatalf("identity = %s", body)
	}
	for _, leak := range []string{"secret", "token", "password"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("identity 泄露了 %q：%s", leak, body)
		}
	}

	// adopt：出厂态受理。
	if code, body := do(http.MethodPost, "/internal/cluster/adopt",
		`{"vip":"192.168.10.250","cluster_token":"t"}`); code != http.StatusAccepted {
		t.Fatalf("出厂态纳管应受理，得到 %d %s", code, body)
	}

	// 非出厂态：拒绝。这台机器上可能有数据，纳管会把它变成备机、随后被复制流覆盖。
	factory = false
	if code, _ := do(http.MethodPost, "/internal/cluster/adopt",
		`{"vip":"192.168.10.250","cluster_token":"t"}`); code != http.StatusConflict {
		t.Fatalf("非出厂态必须拒绝，得到 %d", code)
	}

	// 同前缀的其它集群端点仍然要令牌，放开的只有这两个。
	if code, _ := do(http.MethodGet, "/internal/cluster/peers", ""); code != http.StatusUnauthorized {
		t.Fatalf("/internal/cluster/peers 仍应要令牌，得到 %d", code)
	}
}

type stubRegistry struct{}

func (stubRegistry) Register(context.Context, ha.NodeInfo) error  { return nil }
func (stubRegistry) Peers(context.Context) ([]ha.NodeInfo, error) { return nil, nil }
