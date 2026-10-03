package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/tianwei/diskless/internal/ha"
)

var errNotSynced = errors.New("切换前同步未完成，已保持主机身份")

type fakeHA struct {
	err error
}

func (f fakeHA) Status(context.Context) (ha.APIStatus, error) { return ha.APIStatus{}, nil }
func (f fakeHA) SetRate(context.Context, int) error           { return nil }
func (f fakeHA) PlannedSwitch(context.Context) error          { return f.err }

// 计划切换被拒的两种原因处置相反，不能都回同一个 409：
//
//	指错了机器 → 换一台重发
//	同步没完成 → 留在这台等复制追平
func TestPlannedSwitchSeparatesWrongNodeFromNotSynced(t *testing.T) {
	wrongNode := newRouter(Services{Server: &recordingServerService{}, HA: fakeHA{err: ha.ErrNotActive}})
	rec := do(wrongNode, http.MethodPost, "/api/ha/planned-switch", "")
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("「指错了机器」应当用独立状态码（421），得到 %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] == "" {
		t.Fatalf("理由要带出来：%s", rec.Body.String())
	}

	notSynced := newRouter(Services{Server: &recordingServerService{}, HA: fakeHA{err: errNotSynced}})
	rec = do(notSynced, http.MethodPost, "/api/ha/planned-switch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("「同步没完成」保持 409，得到 %d", rec.Code)
	}

	ok := newRouter(Services{Server: &recordingServerService{}, HA: fakeHA{}})
	rec = do(ok, http.MethodPost, "/api/ha/planned-switch", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("受理应当 202，得到 %d", rec.Code)
	}
}

// 摘除节点走 {node} 参数（与同前缀的跨节点路由一致）。用取 {id} 的通用处理器会拿到空串，
// 「移除某台」变成「移除没指定的那台」，下游还报「节点不存在」。
func TestForgetNodeRouteCarriesTheNodeID(t *testing.T) {
	var got string
	r := newRouter(Services{Server: &recordingServerService{}, ForgetNode: func(_ context.Context, id string) error {
		got = id
		return nil
	}})
	rec := do(r, http.MethodDelete, "/api/cluster/nodes/2f454fa237eb", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("应 204，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got != "2f454fa237eb" {
		t.Fatalf("节点 id 没传下去，拿到 %q", got)
	}
}
