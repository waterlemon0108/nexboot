package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/platform"
)

// 审计日志里每行可读的那一半（「镜像 / 合并」而不是 "POST /api/configs/x/merge"）都来自这三个函数；
// 路径不再匹配时会悄悄写成「系统 / 新建」，看起来却像真的。

func TestDeriveModuleActionNamesTheOperationInOperatorTerms(t *testing.T) {
	for _, tc := range []struct{ method, path, module, action string }{
		{http.MethodPost, "/api/images/import", "镜像", "导入"},
		{http.MethodPost, "/api/configs/cfg-1/merge", "镜像", "合并"},
		{http.MethodPost, "/api/configs/cfg-1/fork", "镜像", "派生"},
		{http.MethodPost, "/api/images/img-1/health-check", "镜像", "体检"},
		{http.MethodDelete, "/api/reductions/red-1", "镜像", "删除"},
		{http.MethodPost, "/api/terminals", "终端", "新建"},
		{http.MethodPost, "/api/terminals/t-1/super/stop", "终端", "超管停机"},
		{http.MethodPut, "/api/groups/g-1", "分组", "更新"},
		{http.MethodPost, "/api/pools/tank/read-cache", "存储", "读缓存"},
		{http.MethodPost, "/api/services/dnsmasq/action", "服务器", "服务操作"},
		{http.MethodPost, "/api/alarms/a-1/ack", "告警", "确认告警"},
		{http.MethodPost, "/api/driver-packs/p-1/recommend", "驱动", "推荐"},
		// 没映射的区域也要如实记点东西，而不是什么都不记。
		{http.MethodPost, "/api/whatever", "系统", "新建"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			module, action := deriveModuleAction(tc.method, tc.path)
			if module != tc.module || action != tc.action {
				t.Fatalf("= %q/%q, want %q/%q", module, action, tc.module, tc.action)
			}
		})
	}
}

// 恰好像动词的 id 不能被当成动词：动作取自最后一段，而 id 也在那里。
func TestDeriveModuleActionDoesNotReadAnIDAsAVerb(t *testing.T) {
	if _, action := deriveModuleAction(http.MethodDelete, "/api/images/win11"); action != "删除" {
		t.Fatalf("action = %q, want 删除", action)
	}
}

func TestClientIPPrefersTheOriginalCaller(t *testing.T) {
	for _, tc := range []struct{ name, xff, remote, want string }{
		{"直连", "", "10.0.0.5:51234", "10.0.0.5"},
		{"经一层代理", "203.0.113.9", "10.0.0.5:51234", "203.0.113.9"},
		// 最左边是客户端，其余是经过的代理。
		{"经多层代理取最左", "203.0.113.9, 10.0.0.1, 10.0.0.2", "10.0.0.5:51234", "203.0.113.9"},
		{"RemoteAddr 没有端口时原样返回", "", "unix-socket", "unix-socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/images", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// 读不审计，否则会淹没写操作。有两个写操作故意豁免：每台客户机发的心跳，以及自带结果记录的登录。
func TestAuditableRequestSkipsReadsAndTheTwoExemptWrites(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/images/import", true},
		{http.MethodDelete, "/api/images/img-1", true},
		{http.MethodPut, "/api/groups/g-1", true},
		{http.MethodPatch, "/api/settings", true},
		{http.MethodGet, "/api/images", false},
		{http.MethodPost, "/api/terminals/heartbeat", false},
		{http.MethodPost, "/api/login", false},
		{http.MethodPost, "/boot", false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := auditableRequest(r); got != tc.want {
			t.Fatalf("%s %s = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

type recordingAudit struct {
	AuditService
	entries []platform.AuditEntry
	err     error
}

func (a *recordingAudit) Record(_ context.Context, e platform.AuditEntry) error {
	a.entries = append(a.entries, e)
	return a.err
}

// 经中间件端到端：一次写入落一行，带上谁、做了什么、结果和 HTTP 状态。
func TestAuditMiddlewareRecordsTheWriteAndItsOutcome(t *testing.T) {
	audit := &recordingAudit{}
	auth := &fakeAuthService{verifyResult: platform.UserInfo{Username: "admin"}}
	images := &fakeImageService{err: errs.NotFound("镜像不存在")}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		Services{Auth: auth, Audit: audit, Images: images})

	req := httptest.NewRequest(http.MethodDelete, "/api/images/ghost", nil)
	req.Header.Set("Authorization", "Bearer t")
	req.RemoteAddr = "10.0.0.5:51234"
	router.ServeHTTP(httptest.NewRecorder(), req)

	if len(audit.entries) != 1 {
		t.Fatalf("entries = %#v", audit.entries)
	}
	e := audit.entries[0]
	if e.Username != "admin" || e.Module != "镜像" || e.Action != "删除" {
		t.Fatalf("entry = %#v", e)
	}
	// 被拒绝的写入仍值得记录，标为失败。
	if e.Status != "err" || e.HTTPStatus != http.StatusNotFound {
		t.Fatalf("outcome = %q/%d, want err/404", e.Status, e.HTTPStatus)
	}
	if e.IP != "10.0.0.5" || !strings.Contains(e.Detail, "/api/images/ghost") {
		t.Fatalf("entry = %#v", e)
	}
}

func TestAuditMiddlewareLeavesReadsAlone(t *testing.T) {
	audit := &recordingAudit{}
	auth := &fakeAuthService{verifyResult: platform.UserInfo{Username: "admin"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		Services{Auth: auth, Audit: audit, Images: &fakeImageService{}})

	req := httptest.NewRequest(http.MethodGet, "/api/images", nil)
	req.Header.Set("Authorization", "Bearer t")
	router.ServeHTTP(httptest.NewRecorder(), req)

	if len(audit.entries) != 0 {
		t.Fatalf("a read was audited: %#v", audit.entries)
	}
}

// 名为「教学一班网卡」的驱动包下载时保留该名字：HTTP 头是 latin-1，原始 UTF-8 会变乱码；
// RFC 6266 的 filename* 携带真名，普通 filename 作为回退。
func TestDriverArchiveKeepsANonASCIIFilenameReadable(t *testing.T) {
	drivers := &recordingDriverService{archiveName: "教学一班网卡.zip", archive: []byte("PK\x03\x04")}
	rec := do(newRouter(Services{Drivers: drivers}), http.MethodGet, "/api/driver-bundles/b-1/archive", "")

	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q, want an RFC 6266 filename*", disposition)
	}
	if !strings.Contains(disposition, url.PathEscape("教学一班网卡.zip")) {
		t.Fatalf("Content-Disposition = %q, want the percent-encoded name", disposition)
	}
	for _, r := range disposition {
		if r > 127 {
			t.Fatalf("raw non-ascii byte in the header: %q", disposition)
		}
	}
}

// 分片追加每 8MB 一次，40GB 镜像会写出约 5000 条审计；只审计开始和放弃。
func TestAuditableRequestSkipsUploadChunks(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPatch, "/api/images/uploads/up-1", false},
		{http.MethodPost, "/api/images/uploads", true},
		{http.MethodDelete, "/api/images/uploads/up-1", true},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := auditableRequest(r); got != tc.want {
			t.Fatalf("%s %s = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
