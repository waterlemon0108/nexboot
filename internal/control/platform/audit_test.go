package platform

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/store"
)

func newAuditTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 审计日志要能按操作者的问法查回：谁、在哪个模块、哪些失败、哪段时间，每个过滤条件对应一个问法。
func TestAuditListAnswersTheQuestionsItIsAskedWith(t *testing.T) {
	ctx := context.Background()
	st := newAuditTestStore(t)
	clock := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	svc := AuditService{Store: st, Now: func() time.Time { clock = clock.Add(time.Minute); return clock }}

	for _, e := range []AuditEntry{
		{Type: "operation", Username: "admin", Module: "镜像", Action: "导入", Detail: "POST /api/images/import", Status: "ok", HTTPStatus: 202},
		{Type: "operation", Username: "admin", Module: "镜像", Action: "删除", Detail: "DELETE /api/images/win11", Status: "err", HTTPStatus: 409},
		{Type: "operation", Username: "teacher", Module: "终端", Action: "新建", Detail: "POST /api/terminals", Status: "ok", HTTPStatus: 201},
		{Type: "login", Username: "teacher", Action: "登录", Detail: "登录成功", Status: "ok"},
	} {
		if err := svc.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name  string
		query AuditQuery
		want  int
	}{
		{"全部", AuditQuery{}, 4},
		{"按类型", AuditQuery{Type: "login"}, 1},
		{"按用户", AuditQuery{Username: "admin"}, 2},
		{"按模块", AuditQuery{Module: "镜像"}, 2},
		{"只看失败", AuditQuery{Status: "err"}, 1},
		{"搜索命中 detail", AuditQuery{Search: "win11"}, 1},
		{"两个条件相与", AuditQuery{Username: "admin", Status: "ok"}, 1},
		{"没有命中不是错误", AuditQuery{Username: "nobody"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.List(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if got.Total != tc.want || len(got.Items) != tc.want {
				t.Fatalf("total = %d / items = %d, want %d", got.Total, len(got.Items), tc.want)
			}
		})
	}
}

// 最新的排在前面，操作者要看的是刚发生的事。
func TestAuditListReturnsNewestFirstAndPages(t *testing.T) {
	ctx := context.Background()
	st := newAuditTestStore(t)
	clock := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	svc := AuditService{Store: st, Now: func() time.Time { clock = clock.Add(time.Minute); return clock }}
	for _, name := range []string{"第一件", "第二件", "第三件"} {
		if err := svc.Record(ctx, AuditEntry{Type: "operation", Username: "admin", Detail: name}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := svc.List(ctx, AuditQuery{Page: 1, Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Items) != 2 || first.Items[0].Detail != "第三件" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := svc.List(ctx, AuditQuery{Page: 2, Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Detail != "第一件" {
		t.Fatalf("second page = %#v", second)
	}
}

// 未设置或过大的分页大小回落到合理值，避免整表一次返回。
func TestAuditListBoundsThePageSize(t *testing.T) {
	ctx := context.Background()
	svc := AuditService{Store: newAuditTestStore(t)}
	for _, tc := range []struct{ asked, want int }{{0, 20}, {-5, 20}, {5000, 20}, {50, 50}} {
		got, err := svc.List(ctx, AuditQuery{Size: tc.asked})
		if err != nil {
			t.Fatal(err)
		}
		if got.Size != tc.want {
			t.Fatalf("size %d -> %d, want %d", tc.asked, got.Size, tc.want)
		}
		if got.Page != 1 {
			t.Fatalf("page = %d, want 1", got.Page)
		}
	}
}

// 未写结果的记录视为成功：中间件只在出错时设 Status，留空在过滤时既不算 ok 也不算 err。
func TestRecordDefaultsToSuccessAndStampsTheRow(t *testing.T) {
	ctx := context.Background()
	st := newAuditTestStore(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	svc := AuditService{Store: st, Now: func() time.Time { return at }}

	if err := svc.Record(ctx, AuditEntry{Type: "operation", Username: "admin", Detail: "无状态"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.List(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %#v", got.Items)
	}
	row := got.Items[0]
	if row.Status != "ok" {
		t.Fatalf("status = %q, want ok", row.Status)
	}
	if !row.CreatedAt.Equal(at) {
		t.Fatalf("created = %s, want %s", row.CreatedAt, at)
	}
	if row.ID == "" {
		t.Fatal("row has no ID")
	}
}

// 同一时刻记录的两条不能共用 ID，否则会丢一条。
func TestRecordGivesEveryRowItsOwnID(t *testing.T) {
	ctx := context.Background()
	st := newAuditTestStore(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	svc := AuditService{Store: st, Now: func() time.Time { return at }}
	for i := 0; i < 20; i++ {
		if err := svc.Record(ctx, AuditEntry{Type: "operation", Username: "admin"}); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	got, err := svc.List(ctx, AuditQuery{Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 20 {
		t.Fatalf("total = %d, want 20", got.Total)
	}
}
