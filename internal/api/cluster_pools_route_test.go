package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/platform"
)

// 只有节点自己答得出「你有哪些池」。没有这个端点写入者无从收集，每台对端的容量都显示未知。
func TestNodePoolsEndpointNeedsClusterToken(t *testing.T) {
	r := newRouter(Services{
		ClusterToken: "s3cret",
		NodePools: func(context.Context) (ops.PoolListResult, error) {
			return ops.PoolListResult{Items: []ops.PoolItem{{Name: "tank", Capacity: 1 << 30}}, Total: 1}, nil
		},
	})

	// 没令牌：谁都回答的对端端点会把存储布局交给任何能连上端口的人。
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/node/pools", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/internal/node/pools", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带令牌应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out ops.PoolListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "tank" {
		t.Fatalf("池没带出来: %s", rec.Body.String())
	}
}

// 概览和存储页读同一个集群级端点；接上它，「1 个池、77 GiB」（本机）才变成集群真实总量。
func TestClusterPoolsEndpointServesAggregate(t *testing.T) {
	r := newRouter(Services{
		ClusterPools: func(context.Context) (platform.ClusterPoolsResult, error) {
			return platform.ClusterPoolsResult{
				Nodes:         []platform.ClusterPoolView{{IP: "10.0.0.3", Reachable: true}},
				TotalCapacity: 160 << 30, PoolCount: 2,
			}, nil
		},
	})
	rec := do(r, http.MethodGet, "/api/cluster/pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out platform.ClusterPoolsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PoolCount != 2 || out.TotalCapacity != 160<<30 {
		t.Fatalf("集群合计没带出来: %s", rec.Body.String())
	}
}
