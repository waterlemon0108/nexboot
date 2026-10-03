package store

import (
	"context"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func seedAudit(t *testing.T, st *SQLStore) {
	t.Helper()
	base := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	rows := []domain.AuditLog{
		{ID: "l1", Type: "operation", Username: "admin", Action: "导入", Module: "镜像", Detail: "POST /api/images/import", Status: "ok", HTTPStatus: 201, CreatedAt: base},
		{ID: "l2", Type: "operation", Username: "admin", Action: "删除", Module: "终端", Detail: "DELETE /api/terminals/x", Status: "err", HTTPStatus: 400, CreatedAt: base.Add(1 * time.Minute)},
		{ID: "l3", Type: "operation", Username: "op1", Action: "移动", Module: "终端", Detail: "POST /api/terminals/move", Status: "ok", HTTPStatus: 200, CreatedAt: base.Add(2 * time.Minute)},
		{ID: "l4", Type: "login", Username: "op1", Action: "登录成功", Detail: "登录成功", Module: "登录", Status: "ok", CreatedAt: base.Add(3 * time.Minute)},
		{ID: "l5", Type: "login", Username: "attacker", Action: "登录失败", Detail: "密码错误", Module: "登录", Status: "err", CreatedAt: base.Add(4 * time.Minute)},
	}
	for _, r := range rows {
		if err := st.AuditLogs().Create(context.Background(), r); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}
}

func TestAuditListPagedFilters(t *testing.T) {
	st := newTestStore(t)
	seedAudit(t, st)
	ctx := context.Background()
	repo := st.AuditLogs()

	// 按新到旧，全部。
	all, total, err := repo.ListPaged(ctx, AuditFilter{}, 20, 0)
	if err != nil || total != 5 || len(all) != 5 {
		t.Fatalf("all: total=%d len=%d err=%v", total, len(all), err)
	}
	if all[0].ID != "l5" || all[4].ID != "l1" {
		t.Fatalf("expected newest-first, got %s..%s", all[0].ID, all[4].ID)
	}

	// 按 type=login 过滤。
	logins, total, _ := repo.ListPaged(ctx, AuditFilter{Type: "login"}, 20, 0)
	if total != 2 || len(logins) != 2 {
		t.Fatalf("login filter: total=%d len=%d", total, len(logins))
	}

	// 按模块 + 状态过滤。
	termErr, total, _ := repo.ListPaged(ctx, AuditFilter{Module: "终端", Status: "err"}, 20, 0)
	if total != 1 || termErr[0].ID != "l2" {
		t.Fatalf("module+status filter wrong: total=%d %+v", total, termErr)
	}

	// 按用户名过滤。
	_, total, _ = repo.ListPaged(ctx, AuditFilter{Username: "op1"}, 20, 0)
	if total != 2 {
		t.Fatalf("username filter total=%d, want 2", total)
	}

	// 在 action/detail 中做子串搜索。
	_, total, _ = repo.ListPaged(ctx, AuditFilter{Search: "密码"}, 20, 0)
	if total != 1 {
		t.Fatalf("search total=%d, want 1", total)
	}

	// 分页：每页 2 条、偏移 2 → 第 3、4 条（l3、l2）。
	page2, total, _ := repo.ListPaged(ctx, AuditFilter{}, 2, 2)
	if total != 5 || len(page2) != 2 || page2[0].ID != "l3" {
		t.Fatalf("pagination wrong: total=%d len=%d first=%s", total, len(page2), page2[0].ID)
	}

	// 时间范围：从 l3 起（>= base+2min）。
	from := time.Date(2026, 7, 3, 9, 2, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	_, total, _ = repo.ListPaged(ctx, AuditFilter{From: from}, 20, 0)
	if total != 3 {
		t.Fatalf("from filter total=%d, want 3", total)
	}
}
