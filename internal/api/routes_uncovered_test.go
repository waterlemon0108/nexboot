package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/network"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/platform"
	"github.com/tianwei/diskless/internal/domain"
)

// 这些路由原先只被「认证是否拦截」的遍历覆盖。每条都有 handler 可能悄悄做错的传输层工作：
// 服务收不到的查询参数、退化成 500 的错误类别、下载需要的头。下面的替身只实现各测试用到的方法，
// 其余是嵌入的接口，handler 碰到就会 panic（说明测试断言错了东西）。

type recordingAlarmService struct {
	AlarmService
	query    platform.AlarmQuery
	acked    string
	deleted  string
	result   platform.AlarmListResult
	ackErr   error
	delError error
}

func (s *recordingAlarmService) List(_ context.Context, q platform.AlarmQuery) (platform.AlarmListResult, error) {
	s.query = q
	return s.result, nil
}
func (s *recordingAlarmService) Acknowledge(_ context.Context, id string) error {
	s.acked = id
	return s.ackErr
}
func (s *recordingAlarmService) Delete(_ context.Context, id string) error {
	s.deleted = id
	return s.delError
}

func newRouter(svc Services) http.Handler {
	return NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, svc)
}

func do(router http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func TestListAlarmsPassesTheFilterThrough(t *testing.T) {
	alarms := &recordingAlarmService{result: platform.AlarmListResult{Total: 3}}
	rec := do(newRouter(Services{Alarms: alarms}), http.MethodGet,
		"/api/alarms?status=active&severity=critical&page=2&size=50", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	want := platform.AlarmQuery{Status: "active", Severity: "critical", Page: 2, Size: 50}
	if alarms.query != want {
		t.Fatalf("query = %#v, want %#v", alarms.query, want)
	}
}

func TestAcknowledgeAndDeleteAlarmCarryTheID(t *testing.T) {
	alarms := &recordingAlarmService{}
	router := newRouter(Services{Alarms: alarms})

	if rec := do(router, http.MethodPost, "/api/alarms/alarm-7/ack", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("ack status = %d: %s", rec.Code, rec.Body)
	}
	if alarms.acked != "alarm-7" {
		t.Fatalf("acked = %q", alarms.acked)
	}
	if rec := do(router, http.MethodDelete, "/api/alarms/alarm-7", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body)
	}
	if alarms.deleted != "alarm-7" {
		t.Fatalf("deleted = %q", alarms.deleted)
	}
}

// 已不存在的告警必须返回 404 而不是 500：界面显示「该告警已不存在」而不是「服务器错误」。
func TestAcknowledgeMissingAlarmIs404(t *testing.T) {
	alarms := &recordingAlarmService{ackErr: errs.NotFound("告警不存在")}
	rec := do(newRouter(Services{Alarms: alarms}), http.MethodPost, "/api/alarms/ghost/ack", "{}")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

type recordingAuditService struct {
	AuditService
	query platform.AuditQuery
}

func (s *recordingAuditService) List(_ context.Context, q platform.AuditQuery) (platform.AuditListResult, error) {
	s.query = q
	return platform.AuditListResult{}, nil
}
func (s *recordingAuditService) Record(context.Context, platform.AuditEntry) error { return nil }

func TestListLogsPassesEveryFilterThrough(t *testing.T) {
	audit := &recordingAuditService{}
	rec := do(newRouter(Services{Audit: audit}), http.MethodGet,
		"/api/logs?type=operation&user=admin&module=image&status=success&q=win&from=2026-07-01&to=2026-07-30&page=3&size=20", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	want := platform.AuditQuery{
		Type: "operation", Username: "admin", Module: "image", Status: "success",
		Search: "win", From: "2026-07-01", To: "2026-07-30", Page: 3, Size: 20,
	}
	if audit.query != want {
		t.Fatalf("query = %#v, want %#v", audit.query, want)
	}
}

type recordingServerService struct {
	ServerService
	key, action string
	lines       int
	actionErr   error
}

func (s *recordingServerService) ListServices(context.Context) (platform.ServiceListResult, error) {
	return platform.ServiceListResult{}, nil
}
func (s *recordingServerService) ServiceAction(_ context.Context, key, action string) error {
	s.key, s.action = key, action
	return s.actionErr
}
func (s *recordingServerService) ServiceLogs(_ context.Context, key string, lines int) (string, error) {
	s.key, s.lines = key, lines
	return "line one\nline two", nil
}

func TestServiceActionAndLogsCarryTheirParameters(t *testing.T) {
	server := &recordingServerService{}
	router := newRouter(Services{Server: server})

	rec := do(router, http.MethodPost, "/api/services/dnsmasq/action", `{"action":"restart"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("action status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if server.key != "dnsmasq" || server.action != "restart" {
		t.Fatalf("service = %q action = %q", server.key, server.action)
	}

	rec = do(router, http.MethodGet, "/api/services/dnsmasq/logs?lines=200", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d: %s", rec.Code, rec.Body)
	}
	if server.lines != 200 {
		t.Fatalf("lines = %d, want 200", server.lines)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["logs"], "line one") {
		t.Fatalf("body = %#v", body)
	}
}

// 未知动作不能作为 systemctl 动词到达主机。
func TestServiceActionRejectsWhatTheServiceRefuses(t *testing.T) {
	server := &recordingServerService{actionErr: errs.Invalid("不支持的操作")}
	rec := do(newRouter(Services{Server: server}), http.MethodPost, "/api/services/dnsmasq/action", `{"action":"rm -rf"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

type recordingDriverService struct {
	DriverService
	id, status  string
	archiveName string
	archive     []byte
	err         error
}

func (s *recordingDriverService) SetPackStatus(_ context.Context, id string, st domain.DriverPackStatus) (domain.DriverPack, error) {
	s.id, s.status = id, string(st)
	if s.err != nil {
		return domain.DriverPack{}, s.err
	}
	return domain.DriverPack{ID: id, Status: st}, nil
}
func (s *recordingDriverService) SetRecommended(_ context.Context, id string) (domain.DriverPack, error) {
	s.id = id
	return domain.DriverPack{ID: id, Recommended: true}, s.err
}
func (s *recordingDriverService) DeletePack(_ context.Context, id string) error {
	s.id = id
	return s.err
}
func (s *recordingDriverService) BundleArchive(_ context.Context, id string) (string, []byte, error) {
	s.id = id
	return s.archiveName, s.archive, s.err
}
func (s *recordingDriverService) ListBundles(context.Context) (adapt.DriverBundleListResult, error) {
	return adapt.DriverBundleListResult{}, s.err
}

func TestDriverPackStatusAndRecommendCarryTheID(t *testing.T) {
	drivers := &recordingDriverService{}
	router := newRouter(Services{Drivers: drivers})

	rec := do(router, http.MethodPost, "/api/driver-packs/pack-1/status", `{"status":"disabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if drivers.id != "pack-1" || drivers.status != "disabled" {
		t.Fatalf("id = %q status = %q", drivers.id, drivers.status)
	}

	rec = do(router, http.MethodPost, "/api/driver-packs/pack-2/recommend", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("recommend status = %d: %s", rec.Code, rec.Body)
	}
	if drivers.id != "pack-2" {
		t.Fatalf("recommend id = %q", drivers.id)
	}
}

// 归档是唯一不是 JSON 的响应：没有这些头，浏览器会把 zip 当文本显示而不是保存。
func TestDriverBundleArchiveIsServedAsADownload(t *testing.T) {
	drivers := &recordingDriverService{archiveName: "bundle-1.zip", archive: []byte("PK\x03\x04zip")}
	rec := do(newRouter(Services{Drivers: drivers}), http.MethodGet, "/api/driver-bundles/bundle-1/archive", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "bundle-1.zip") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if rec.Body.String() != "PK\x03\x04zip" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestDriverBundleArchiveMissingIs404(t *testing.T) {
	drivers := &recordingDriverService{err: errs.NotFound("驱动包不存在")}
	rec := do(newRouter(Services{Drivers: drivers}), http.MethodGet, "/api/driver-bundles/ghost/archive", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

type stubConsistencyService struct {
	report ops.ConsistencyReport
	err    error
}

func (s stubConsistencyService) Check(context.Context) (ops.ConsistencyReport, error) {
	return s.report, s.err
}

func TestConsistencyReportIsServed(t *testing.T) {
	report := ops.ConsistencyReport{OK: false, Issues: []ops.ConsistencyIssue{
		{Kind: ops.IssueMissingSnapshot, Ref: "cfg_r1", Detail: "快照 @r1 不存在"},
	}}
	rec := do(newRouter(Services{Consistency: stubConsistencyService{report: report}}), http.MethodGet, "/api/system/consistency", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got ops.ConsistencyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || len(got.Issues) != 1 || got.Issues[0].Ref != "cfg_r1" {
		t.Fatalf("report = %#v", got)
	}
}

type recordingNetworkService struct {
	NetworkService
	clientMax int
	updated   network.Request
	probed    network.ProbeRequest
	view      network.View
	err       error
}

func (s *recordingNetworkService) Get(_ context.Context, clientMax int) (network.View, error) {
	s.clientMax = clientMax
	return s.view, s.err
}

func (s *recordingNetworkService) Update(_ context.Context, req network.Request) (network.View, error) {
	s.updated = req
	return s.view, s.err
}

func (s *recordingNetworkService) Probe(_ context.Context, req network.ProbeRequest) (network.ProbeResult, error) {
	s.probed = req
	return network.ProbeResult{IP: req.IP, Reachable: true, RTTMillis: 0.5}, s.err
}

// 网络页的三个调用：client_max 传到 Get（默认 30），开关传到 Update，探测地址传到 Probe；
// 被拒绝（有 relay 分组时关闭开关）返回带提示的 400。
func TestNetworkRoutes(t *testing.T) {
	service := &recordingNetworkService{view: network.View{ClientIface: "ndbr0", Known: true}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Network: service})

	req := httptest.NewRequest(http.MethodGet, "/api/network?client_max=12", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.clientMax != 12 || !strings.Contains(rec.Body.String(), `"client_iface":"ndbr0"`) {
		t.Fatalf("get status=%d clientMax=%d body=%s", rec.Code, service.clientMax, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/network", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if service.clientMax != 30 {
		t.Fatalf("default client_max = %d, want 30", service.clientMax)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/network", strings.NewReader(`{"client_iface":"ndbr0","allow_cross_subnet":true}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.updated.AllowCrossSubnet || service.updated.ClientIface != "ndbr0" {
		t.Fatalf("put status=%d updated=%#v", rec.Code, service.updated)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/network/probe", strings.NewReader(`{"ip":"192.168.10.1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.probed.IP != "192.168.10.1" || !strings.Contains(rec.Body.String(), `"reachable":true`) {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}

	service.err = errs.Invalid("还有 1 个跨网段分组：vmdk。先把它们改到服务器网段，再关闭跨网段分组")
	req = httptest.NewRequest(http.MethodPut, "/api/network", strings.NewReader(`{"allow_cross_subnet":false}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "vmdk") {
		t.Fatalf("refused put status=%d body=%s", rec.Code, rec.Body.String())
	}
}
