package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/platform"
)

type recordedForward struct {
	node, method, suffix, body, query string
}

// 控制台点名哪台，存储写请求就带着那个名字送到哪台，且方法、路径和请求体原样转发；
// 否则三节点里另外两台的池只能看不能动。
func TestPoolWritesReachTheNamedNode(t *testing.T) {
	var got []recordedForward
	r := newRouter(Services{
		Pools: &stubPools{},
		ClusterProxy: func(_ context.Context, node, method, suffix, query string, body io.Reader) (ProxyResponse, error) {
			b, _ := io.ReadAll(body)
			got = append(got, recordedForward{node: node, method: method, suffix: suffix, body: string(b), query: query})
			return ProxyResponse{Status: http.StatusAccepted, Body: []byte(`{"ok":true}`), ContentType: "application/json"}, nil
		},
	})

	rec := do(r, http.MethodPost, "/api/cluster/nodes/peer/pools/p-1/disks", `{"disk":"/dev/sdb"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("应透传对端的状态码，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("没转发: %+v", got)
	}
	if got[0].node != "peer" || got[0].method != http.MethodPost || got[0].suffix != "p-1/disks" {
		t.Fatalf("转发的目标或路径不对: %+v", got[0])
	}
	if !strings.Contains(got[0].body, "/dev/sdb") {
		t.Fatalf("请求体没原样带过去: %+v", got[0])
	}

	// 销毁是 DELETE，方法也必须原样带过去，退化成 POST 会打到别的处理器上。
	got = nil
	rec = do(r, http.MethodDelete, "/api/cluster/nodes/peer/pools/p-1", "")
	if rec.Code != http.StatusAccepted || len(got) != 1 || got[0].method != http.MethodDelete || got[0].suffix != "p-1" {
		t.Fatalf("DELETE 转发不对: code=%d %+v", rec.Code, got)
	}
}

// 对端一侧：池写操作要能不带登录会话调用（调用方持集群令牌），但必须挡住没令牌的人，否则存储控制面对整个网络敞开。
func TestNodePoolWritesNeedClusterToken(t *testing.T) {
	pools := &stubPools{}
	r := newRouter(Services{Pools: pools, ClusterToken: "s3cret", NodePoolWrites: true})

	rec := do(r, http.MethodDelete, "/internal/node/pools/p-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/internal/node/pools/p-1", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("带令牌应受理，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if pools.destroyed != "p-1" {
		t.Fatalf("没打到本机的池服务: %q", pools.destroyed)
	}
}

// 花名册地址会过期：机器换了 IP，旧记录指向的地址可能已是另一台，「销毁 B 的池」会在 C 上成功执行。
// 被点名的节点必须核对名字，这是动手前唯一能发现指错人的地方。
func TestNodePoolWriteRefusesWhenItIsNotTheNamedNode(t *testing.T) {
	pools := &stubPools{}
	r := newRouter(Services{Pools: pools, ClusterToken: "s3cret", NodePoolWrites: true, NodeID: "nodeA"})

	req := httptest.NewRequest(http.MethodDelete, "/internal/node/pools/p-1?node=nodeB", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("指错节点应拒绝（409），得到 %d: %s", rec.Code, rec.Body.String())
	}
	if pools.destroyed != "" {
		t.Fatalf("已经动手了，这正是要防的: %q", pools.destroyed)
	}

	// 点名正确时照常执行。
	req = httptest.NewRequest(http.MethodDelete, "/internal/node/pools/p-1?node=nodeA", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || pools.destroyed != "p-1" {
		t.Fatalf("点名正确却没执行: code=%d destroyed=%q", rec.Code, pools.destroyed)
	}
}

// 节点间调用带的是集群令牌而不是登录会话。挂进需登录的组里单测照样绿（很少同时接登录服务），
// 真机上每次跨节点写都会被拦成 401，报错还指向登录凭证。
func TestNodePoolWritesBypassSessionAuth(t *testing.T) {
	pools := &stubPools{}
	r := newRouter(Services{
		Pools: pools, ClusterToken: "s3cret", NodePoolWrites: true, NodeID: "nodeA",
		Auth: rejectingAuth{}, // 会话一律不认，只有集群令牌该放行
	})
	req := httptest.NewRequest(http.MethodDelete, "/internal/node/pools/p-1?node=nodeA", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("集群令牌应当放行，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if pools.destroyed != "p-1" {
		t.Fatalf("没打到池服务: %q", pools.destroyed)
	}
}

type rejectingAuth struct{}

func (rejectingAuth) Login(context.Context, platform.LoginRequest) (platform.LoginResult, error) {
	return platform.LoginResult{}, errors.New("no")
}
func (rejectingAuth) VerifyToken(string) (platform.UserInfo, error) {
	return platform.UserInfo{}, errors.New("登录凭证无效或已过期")
}

type stubPools struct {
	destroyed string
	added     string
}

func (s *stubPools) List(context.Context, string) (ops.PoolListResult, error) {
	return ops.PoolListResult{}, nil
}
func (s *stubPools) ListDisks(context.Context) (ops.DiskListResult, error) {
	return ops.DiskListResult{}, nil
}
func (s *stubPools) Get(context.Context, string) (ops.PoolItem, error) { return ops.PoolItem{}, nil }
func (s *stubPools) Create(context.Context, ops.PoolRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) AddDisk(_ context.Context, id string, _ ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.added = id
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) RemoveDisk(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) MirrorUpgrade(context.Context, string, ops.MirrorUpgradeRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) AddSpecial(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) RemoveSpecial(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) AddSpare(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) RemoveSpare(context.Context, string, ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) ReplaceDisk(context.Context, string, ops.PoolReplaceDiskRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) AddReadCache(context.Context, string, ops.PoolReadCacheRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) RemoveReadCache(context.Context, string, ops.PoolReadCacheRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) AddWriteCache(context.Context, string, ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) RemoveWriteCache(context.Context, string, ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error) {
	return ops.PoolOperationResult{}, nil
}
func (s *stubPools) FlushWriteCache(context.Context, string) (ops.PoolTaskResult, error) {
	return ops.PoolTaskResult{}, nil
}
func (s *stubPools) Destroy(_ context.Context, id string) (ops.PoolTaskResult, error) {
	s.destroyed = id
	return ops.PoolTaskResult{}, nil
}
