package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tianwei/diskless/internal/ha"

	"github.com/go-chi/chi/v5"
	"github.com/tianwei/diskless/internal/control"
	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/ops"
	"github.com/tianwei/diskless/internal/control/platform"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "{\"status\":\"ok\"}\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestSPAFallback(t *testing.T) {
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), testAssets())

	tests := []struct {
		path string
		want int
		body string
	}{
		{path: "/", want: http.StatusOK, body: "index"},
		{path: "/groups", want: http.StatusOK, body: "index"},
		{path: "/assets/app.js", want: http.StatusOK, body: "asset"},
		{path: "/api/missing", want: http.StatusNotFound, body: "404"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Fatalf("%s status = %d, want %d", tt.path, rec.Code, tt.want)
		}
		if got := rec.Body.String(); !strings.Contains(got, tt.body) {
			t.Fatalf("%s body = %q, want contains %q", tt.path, got, tt.body)
		}
	}
}

func TestBootEndpoint(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n"}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	// 多出的查询参数是 dnsmasq 启动 URL 一直在发的硬件画像参数，现在没人读，但旧启动 URL 必须照样能启动。
	req := httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb&platform=efi&busid=01:00:8086:15b8&manufacturer=ASUS&product=PRIME", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if service.mac != "aa:bb" || rec.Body.String() != "#!ipxe\n" {
		t.Fatalf("mac=%q body=%q", service.mac, rec.Body.String())
	}
}

func TestBootChecksTheClientsFirmwareAgainstTheImage(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n"}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb&platform=efi", nil))
	if rec.Code != http.StatusOK || strings.Join(service.checked, " ") != "aa:bb efi" {
		t.Fatalf("status=%d checked=%v", rec.Code, service.checked)
	}
}

func TestBootFailedRecordsTheReport(t *testing.T) {
	service := &fakeBootService{}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/failed?mac=aa:bb&stage=sanboot&err=1058021379&platform=efi", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(service.failure, " "); got != "aa:bb sanboot 1058021379 efi" {
		t.Fatalf("recorded %q", got)
	}
}

func TestBootFailedUsesTheSameAddressCheckAndUnknownIs404(t *testing.T) {
	service := &fakeBootService{registeredIP: "10.0.0.7"}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})
	req := httptest.NewRequest(http.MethodGet, "/boot/failed?mac=aa:bb&stage=sanboot", nil)
	req.RemoteAddr = "10.0.0.99:5000"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || service.failure != nil {
		t.Fatalf("someone else's report: status=%d recorded=%v", rec.Code, service.failure)
	}

	router = NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: emptyBootService{}})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/failed?mac=aa:bb&stage=sanboot", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown mac status = %d", rec.Code)
	}
}

func TestBootEndpointErrors(t *testing.T) {
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: &fakeBootService{err: control.ErrUnknownTerminal}})

	req := httptest.NewRequest(http.MethodGet, "/boot", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing mac status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/boot?mac=aa", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown mac status = %d", rec.Code)
	}
}

// 无盘客户机拿到根盘前无法自证身份，能核对的只有来源地址，而这个地址是产品自己分配的。
// 不核对的话谁都能拿到别人系统盘的 target IQN，第二个写入者会把正被挂载的 NTFS 写坏。
func TestBootRefusesToAnswerForSomeoneElsesAddress(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n", registeredIP: "10.0.0.7"}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	req := httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.7:49152"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机来问自己的配置 status = %d, want 200", rec.Code)
	}

	// 换一台来问同一个 MAC：答成「没这台机器」而不是 403，否则状态码就能枚举出全场在册的 MAC。
	req = httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.9:49152"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("别人替它问 status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "unknown mac") {
		t.Fatalf("body = %q, want 与未知 MAC 同一句", got)
	}
}

// 同一道门要挡住整个 /boot 族：客户机脚本读的两个 JSON 端点泄露分组网关、DNS 和盘符映射。
func TestBootJSONEndpointsRefuseSomeoneElsesAddress(t *testing.T) {
	service := &fakeBootService{
		letters:      []control.DataDiskLetter{{LUN: 1, Letter: "D"}},
		net:          control.NetConfig{Gateway: "10.0.0.1"},
		registeredIP: "10.0.0.7",
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	for _, path := range []string{"/boot/data-disks?mac=aa:bb", "/boot/net-config?mac=aa:bb"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.9:49152"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 被别人问到 status = %d, want 404", path, rec.Code)
		}
	}
}

// 没登记地址的终端照常放行：首次开机的机器还没分配地址，盘也还不存在。
func TestBootAllowsTerminalsWithoutARegisteredAddress(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n", registeredIP: ""}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	req := httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.9:49152"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("未登记地址的终端 status = %d, want 200", rec.Code)
	}
}

// 已登录的调用方可以问任意 MAC（运维 curl 排查、e2e 模拟开机）；这道门防的是客户机前的普通用户。
func TestBootAnswersForAnyAddressWhenTheCallerIsLoggedIn(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n", registeredIP: "10.0.0.7"}
	auth := &fakeAuthService{verifyResult: platform.UserInfo{Username: "admin"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		Services{Boot: service, Auth: auth})

	req := httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.9:49152" // 不是那台机器的地址
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员带令牌问 status = %d, want 200", rec.Code)
	}

	// 令牌无效时不算数，照旧按地址判。
	auth.verifyErr = platform.ErrInvalidCredentials
	req = httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.9:49152"
	req.Header.Set("Authorization", "Bearer bad-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无效令牌 status = %d, want 404", rec.Code)
	}
}

// 应急开关必须可用：多网卡或跨网段 relay 让来源地址与分配地址不符时，整场机器都开不了机，要能一句话恢复。
func TestBootPeerCheckCanBeSwitchedOff(t *testing.T) {
	service := &fakeBootService{script: "#!ipxe\n", registeredIP: "10.0.0.7"}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		Services{Boot: service, BootPeerCheckOff: true})

	req := httptest.NewRequest(http.MethodGet, "/boot?mac=aa:bb", nil)
	req.RemoteAddr = "10.0.0.9:49152" // 地址对不上，但开关关了校验
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("关掉校验后 status = %d, want 200", rec.Code)
	}
}

func TestBootDataDisksEndpoint(t *testing.T) {
	service := &fakeBootService{letters: []control.DataDiskLetter{{LUN: 1, Letter: "D"}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	req := httptest.NewRequest(http.MethodGet, "/boot/data-disks?mac=aa:bb", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if service.mac != "aa:bb" || !strings.Contains(rec.Body.String(), `"lun":1`) || !strings.Contains(rec.Body.String(), `"letter":"D"`) {
		t.Fatalf("mac=%q body=%q", service.mac, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/boot/data-disks", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing mac status = %d", rec.Code)
	}
}

func TestBootDataDisksAsTextForShellScripts(t *testing.T) {
	service := &fakeBootService{letters: []control.DataDiskLetter{{LUN: 1, Letter: "/data"}, {LUN: 2, Letter: "/srv/game disk"}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/data-disks?mac=aa:bb&format=text", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "1\t/data\n2\t/srv/game disk\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestBootNetConfigEndpoint(t *testing.T) {
	service := &fakeBootService{net: control.NetConfig{Gateway: "192.168.50.1", DNS: []string{"223.5.5.5"}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Boot: service})

	req := httptest.NewRequest(http.MethodGet, "/boot/net-config?mac=aa:bb", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if service.mac != "aa:bb" || !strings.Contains(rec.Body.String(), `"gateway":"192.168.50.1"`) || !strings.Contains(rec.Body.String(), `"223.5.5.5"`) {
		t.Fatalf("mac=%q body=%q", service.mac, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/boot/net-config", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing mac status = %d", rec.Code)
	}
}

func TestImportImageEndpoint(t *testing.T) {
	service := &fakeImageService{result: assets.ImportImageResult{TaskID: "task-1", ImageID: "img-1"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Images: service})
	body := bytes.NewBufferString(`{"name":"win","source_path":"/var/lib/ndiskless/imports/win.zfs","os_type":"windows"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/images/import", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.req.Name != "win" || service.req.SourcePath != "/var/lib/ndiskless/imports/win.zfs" || service.req.OSType != domain.OSTypeWindows {
		t.Fatalf("req = %#v", service.req)
	}
	var got assets.ImportImageResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "task-1" {
		t.Fatalf("result = %#v", got)
	}
}

func TestRuntimeEndpoint(t *testing.T) {
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Runtime: RuntimeConfig{ImportDir: "/tank/imports"}})

	req := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got RuntimeConfig
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != "/tank/imports" {
		t.Fatalf("runtime = %#v", got)
	}
}

func TestRuntimeEndpointUsesSettingsService(t *testing.T) {
	settings := &fakeSettingsService{view: platform.SystemSettingsView{ImportDir: "/data/imports", DefaultImportDir: "/var/lib/ndiskless/imports"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Runtime: RuntimeConfig{ImportDir: "/var/lib/ndiskless/imports"}, Settings: settings})

	req := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got RuntimeConfig
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ImportDir != "/data/imports" {
		t.Fatalf("runtime = %#v", got)
	}
}

func TestSettingsEndpoints(t *testing.T) {
	service := &fakeSettingsService{view: platform.SystemSettingsView{ImportDir: "/tank/imports", DefaultImportDir: "/var/lib/ndiskless/imports"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Settings: service})

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/tank/imports") {
		t.Fatalf("settings status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"import_dir":"/data/imports"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.saveReq.ImportDir != "/data/imports" {
		t.Fatalf("save status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.err = errs.ErrInvalid
	req = httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"import_dir":"relative"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthProtectsAPIAndAllowsLogin(t *testing.T) {
	auth := &fakeAuthService{loginResult: platform.LoginResult{Token: "token-1", User: platform.UserInfo{ID: "user-1", Username: "admin"}}}
	images := &fakeImageService{list: assets.ImageListResult{Items: []assets.ImageItem{{Image: domain.Image{ID: "img-1", Name: "win"}}}, Total: 1}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Auth: auth, Images: images})

	req := httptest.NewRequest(http.MethodGet, "/api/images", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || auth.loginReq.Username != "admin" || !strings.Contains(rec.Body.String(), "token-1") {
		t.Fatalf("login status=%d auth=%#v body=%s", rec.Code, auth, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/images", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || auth.verifiedToken != "token-1" || !strings.Contains(rec.Body.String(), "img-1") {
		t.Fatalf("authorized status=%d auth=%#v body=%s", rec.Code, auth, rec.Body.String())
	}
}

func TestAuthRejectsInvalidCredentialsAndInvalidToken(t *testing.T) {
	auth := &fakeAuthService{
		loginErr:       platform.ErrInvalidCredentials,
		verifyErr:      platform.ErrInvalidCredentials,
		verifyErrToken: "bad",
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Auth: auth, Images: &fakeImageService{}})

	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"username":"admin","password":"wrong"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/images", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/images", nil)
	req.Header.Set("Authorization", "Bearer "+auth.verifyErrToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status=%d body=%s", rec.Code, rec.Body.String())
	}

	if auth.verifyCalls != 1 {
		t.Fatalf("verify calls = %d", auth.verifyCalls)
	}
}

func TestAuthProtectsStorageDisksEndpoint(t *testing.T) {
	auth := &fakeAuthService{}
	pools := &fakePoolService{diskList: ops.DiskListResult{Items: []storage.DiskInfo{{Path: "/dev/sdb", Name: "sdb", Type: "disk"}}, Total: 1}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Auth: auth, Pools: pools})

	req := httptest.NewRequest(http.MethodGet, "/api/storage/disks", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/storage/disks", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || auth.verifiedToken != "token-1" || !pools.listDisksCalled {
		t.Fatalf("authorized status=%d auth=%#v pools=%#v body=%s", rec.Code, auth, pools, rec.Body.String())
	}
}

func TestAuthAllowsPublicLoginAndHealthzAndBoot(t *testing.T) {
	boot := &fakeBootService{script: "ipxe\n"}
	auth := &fakeAuthService{loginResult: platform.LoginResult{Token: "token-1", User: platform.UserInfo{ID: "user-1", Username: "admin"}}}
	terminal := &fakeTerminalService{item: domain.Terminal{ID: "term-1", MAC: "AA"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Auth: auth, Boot: boot, Terminals: terminal})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/boot?mac=AA:BB", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("boot status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/terminals/heartbeat", bytes.NewBufferString(`{"mac":"AA"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || terminal.heartbeatMAC != "AA" {
		t.Fatalf("heartbeat status=%d body=%s terminal=%#v", rec.Code, rec.Body.String(), terminal)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthAllowsTerminalHeartbeat(t *testing.T) {
	auth := &fakeAuthService{}
	terminal := &fakeTerminalService{item: domain.Terminal{ID: "term-1", MAC: "AA"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Auth: auth, Terminals: terminal})

	req := httptest.NewRequest(http.MethodPost, "/api/terminals/heartbeat", bytes.NewBufferString(`{"mac":"AA"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || terminal.heartbeatMAC != "AA" || auth.verifiedToken != "" {
		t.Fatalf("heartbeat status=%d auth=%#v terminal=%#v body=%s", rec.Code, auth, terminal, rec.Body.String())
	}
}

func TestImageListAndDeleteEndpoints(t *testing.T) {
	service := &fakeImageService{
		list: assets.ImageListResult{Items: []assets.ImageItem{{Image: domain.Image{ID: "img-1", Name: "win"}}}, Total: 1},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Images: service})

	req := httptest.NewRequest(http.MethodGet, "/api/images", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "img-1") {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/images/img-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.deleted != "img-1" {
		t.Fatalf("delete status=%d deleted=%q", rec.Code, service.deleted)
	}

	// 空白数据盘从页面创建（任务），镜像用途可以原地修改。
	req = httptest.NewRequest(http.MethodPost, "/api/images/blank", bytes.NewBufferString(`{"name":"games","size_bytes":107374182400,"filesystem":"ntfs","label":"GAMES"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.blankReq.Name != "games" || service.blankReq.SizeBytes != 107374182400 || service.blankReq.Filesystem != "ntfs" {
		t.Fatalf("blank status=%d req=%#v body=%s", rec.Code, service.blankReq, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPatch, "/api/images/img-1", bytes.NewBufferString(`{"purpose":"data"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.purposeID != "img-1" || service.purpose != "data" {
		t.Fatalf("patch status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}
}

func TestTaskEndpoint(t *testing.T) {
	service := &fakeTaskService{task: domain.Task{ID: "task-1", Status: domain.TaskStatusSuccess}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Tasks: service})

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/task-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.id != "task-1" {
		t.Fatalf("id = %q", service.id)
	}
}

func TestActiveTasksEndpoint(t *testing.T) {
	service := &fakeTaskService{active: []domain.Task{{ID: "task-1", Status: domain.TaskStatusRunning, Progress: 40}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Tasks: service})

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got []domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "task-1" || got[0].Progress != 40 {
		t.Fatalf("got = %#v", got)
	}
}

func TestTasksHistoryEndpoint(t *testing.T) {
	service := &fakeTaskService{listResult: assets.TaskListResult{
		Items: []domain.Task{{ID: "task-1", Status: domain.TaskStatusSuccess}},
		Total: 5,
		Page:  2,
		Size:  20,
	}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Tasks: service})

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/history?page=2&size=20&status=success", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.listQuery.Status != "success" || service.listQuery.Page != 2 || service.listQuery.Size != 20 {
		t.Fatalf("query = %#v", service.listQuery)
	}
	var got assets.TaskListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 5 || len(got.Items) != 1 || got.Items[0].ID != "task-1" {
		t.Fatalf("got = %#v", got)
	}
}

func TestConfigListEndpoint(t *testing.T) {
	service := &fakeConfigService{
		list: assets.ConfigListResult{
			Items: []domain.Config{{ID: "cfg-1", ImageID: "img-1", Name: "default"}},
			Total: 1,
		},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Images: &fakeImageService{}, Configs: service})

	req := httptest.NewRequest(http.MethodGet, "/api/images/img-1/configs", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cfg-1") || service.listImageID != "img-1" {
		t.Fatalf("body=%s image=%q", rec.Body.String(), service.listImageID)
	}

	service.err = sql.ErrNoRows
	req = httptest.NewRequest(http.MethodGet, "/api/images/img-1/configs", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("notfound status = %d", rec.Code)
	}
}

func TestConfigListRouteNotRegisteredWithoutConfigService(t *testing.T) {
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Images: &fakeImageService{}})

	req := httptest.NewRequest(http.MethodGet, "/api/images/img-1/configs", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreateConfigFromImageEndpoint(t *testing.T) {
	service := &fakeConfigService{createdFromImage: assets.ConfigOperationResult{TaskID: "task-1", Config: domain.Config{ID: "cfg-1", Name: "default", ImageID: "img-1"}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Images: &fakeImageService{}, Configs: service})

	req := httptest.NewRequest(http.MethodPost, "/api/images/img-1/configs", bytes.NewBufferString(`{"name":"default"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.createFromImageImageID != "img-1" || service.createdFromImageReq.Name != "default" {
		t.Fatalf("create call = %#v", service)
	}
}

func TestForkConfigEndpoint(t *testing.T) {
	service := &fakeConfigService{createdFromConfig: assets.ConfigOperationResult{TaskID: "task-1", Config: domain.Config{ID: "cfg-2", Name: "clone", ImageID: "img-1"}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Configs: service})

	req := httptest.NewRequest(http.MethodPost, "/api/configs/cfg-1/fork", bytes.NewBufferString(`{"name":"clone","reduction_id":"red-1"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.createFromConfigID != "cfg-1" || service.createFromConfigReq.ReductionID != "red-1" {
		t.Fatalf("create from config call = %#v", service)
	}
}

func TestDeleteConfigEndpoint(t *testing.T) {
	service := &fakeConfigService{deleteResult: assets.ConfigTaskResult{TaskID: "task-1"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Configs: service})

	req := httptest.NewRequest(http.MethodDelete, "/api/configs/cfg-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if service.deleted != "cfg-1" {
		t.Fatalf("deleted = %q", service.deleted)
	}

	service.err = assets.ErrConfigInUse
	req = httptest.NewRequest(http.MethodDelete, "/api/configs/cfg-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", rec.Code)
	}
}

func TestMergeConfigEndpoint(t *testing.T) {
	service := &fakeConfigService{mergeResult: assets.ConfigTaskResult{TaskID: "task-1"}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Configs: service})

	req := httptest.NewRequest(http.MethodPost, "/api/configs/cfg-1/merge", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.merged != "cfg-1" {
		t.Fatalf("merged = %q", service.merged)
	}
}

func TestReductionEndpoints(t *testing.T) {
	service := &fakeReductionService{
		list:         assets.ReductionListResult{Items: []domain.Reduction{{ID: "red-1", Name: "@0"}}, Total: 1},
		createResult: assets.ReductionOperationResult{TaskID: "task-create", Reduction: domain.Reduction{ID: "red-2", Name: "@1"}},
		deleteResult: assets.ReductionTaskResult{TaskID: "task-delete"},
		mergeResult:  assets.ReductionTaskResult{TaskID: "task-merge"},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Reductions: service})

	req := httptest.NewRequest(http.MethodGet, "/api/configs/cfg-1/reductions", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.listConfigID != "cfg-1" {
		t.Fatalf("list status=%d config=%q", rec.Code, service.listConfigID)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/configs/cfg-1/reductions", bytes.NewBufferString(`{"name":"r1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.createConfigID != "cfg-1" || service.createReq.Name != "r1" {
		t.Fatalf("create status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/reductions/red-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.deleted != "red-1" {
		t.Fatalf("delete status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/configs/cfg-1/reductions/merge", bytes.NewBufferString(`{"keep_reduction_id":"red-2","delete_reduction_ids":["red-1"]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.mergeConfigID != "cfg-1" || service.mergeReq.KeepReductionID != "red-2" {
		t.Fatalf("merge status=%d service=%#v", rec.Code, service)
	}

	// 把还原点设为当前只是移动指针，立即答复（200）。
	req = httptest.NewRequest(http.MethodPost, "/api/reductions/red-2/apply", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.setCurrentID != "red-2" || !strings.Contains(rec.Body.String(), `"reduction_id":"red-2"`) {
		t.Fatalf("apply status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}
}

func TestReductionEndpointErrors(t *testing.T) {
	service := &fakeReductionService{err: assets.ErrReductionInUse}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Reductions: service})

	req := httptest.NewRequest(http.MethodDelete, "/api/reductions/red-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestGroupEndpoints(t *testing.T) {
	group := domain.Group{ID: "grp-1", Name: "default", IsDefault: true}
	service := &fakeGroupService{
		list: assets.GroupListResult{Items: []domain.Group{group}, Total: 1},
		item: group,
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Groups: service})

	req := httptest.NewRequest(http.MethodGet, "/api/groups", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "grp-1") {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/groups", bytes.NewBufferString(`{"name":"default","start_ip":"192.168.1.10"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || service.createReq.Name != "default" {
		t.Fatalf("create status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/groups/default", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.defaultRead {
		t.Fatalf("default status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/groups/grp-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.getID != "grp-1" {
		t.Fatalf("get status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/groups/grp-1", bytes.NewBufferString(`{"name":"renamed"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.updateID != "grp-1" || service.updateReq.Name != "renamed" {
		t.Fatalf("update status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/groups/grp-1/default", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.defaultID != "grp-1" {
		t.Fatalf("set default status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/groups/grp-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.deleted != "grp-1" {
		t.Fatalf("delete status=%d service=%#v", rec.Code, service)
	}
}

func TestGroupEndpointErrors(t *testing.T) {
	service := &fakeGroupService{err: assets.ErrGroupInvalidNetwork}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Groups: service})

	req := httptest.NewRequest(http.MethodPost, "/api/groups", bytes.NewBufferString(`{"name":"bad"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", rec.Code)
	}

	service.err = assets.ErrGroupInUse
	req = httptest.NewRequest(http.MethodDelete, "/api/groups/grp-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", rec.Code)
	}

	service.err = sql.ErrNoRows
	req = httptest.NewRequest(http.MethodGet, "/api/groups/grp-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("notfound status = %d", rec.Code)
	}
}

func TestGroupDiskEndpoints(t *testing.T) {
	disk := domain.GroupDisk{ID: "disk-1", GroupID: "grp-1", MountTarget: "D", ImageID: "img-1", ConfigID: "cfg-1"}
	service := &fakeGroupDiskService{list: assets.GroupDiskListResult{Items: []domain.GroupDisk{disk}, Total: 1}, item: disk}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{GroupDisks: service})

	req := httptest.NewRequest(http.MethodGet, "/api/groups/grp-1/disks", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.listGroupID != "grp-1" {
		t.Fatalf("list status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/groups/grp-1/disks", bytes.NewBufferString(`{"mount_target":"D","image_id":"img-1","config_id":"cfg-1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || service.createGroupID != "grp-1" || service.createReq.MountTarget != "D" {
		t.Fatalf("create status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/group-disks/disk-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.deleted != "disk-1" {
		t.Fatalf("delete status=%d service=%#v", rec.Code, service)
	}

	service.err = assets.ErrGroupDiskExists
	req = httptest.NewRequest(http.MethodPost, "/api/groups/grp-1/disks", bytes.NewBufferString(`{"mount_target":"D"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d", rec.Code)
	}
}

func TestTerminalEndpoints(t *testing.T) {
	terminal := domain.Terminal{ID: "terminal-1", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: "grp-1", State: domain.TerminalStateUnknown}
	service := &fakeTerminalService{
		list: assets.TerminalListResult{Items: []domain.Terminal{terminal}, Total: 1},
		item: terminal,
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodGet, "/api/terminals?group_id=grp-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.listGroupID != "grp-1" || !strings.Contains(rec.Body.String(), "AABBCCDDEEFF") {
		t.Fatalf("list status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/terminals", bytes.NewBufferString(`{"name":"front-01","mac":"aa:bb:cc:dd:ee:ff","ip":"192.168.1.10","group_id":"grp-1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || service.createReq.MAC != "aa:bb:cc:dd:ee:ff" || service.createReq.Name != "front-01" {
		t.Fatalf("create status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/terminals/terminal-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.getID != "terminal-1" {
		t.Fatalf("get status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/terminals/terminal-1", bytes.NewBufferString(`{"name":"front-02","mac":"aa","ip":"192.168.1.11","group_id":"grp-1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.updateID != "terminal-1" || service.updateReq.IP != "192.168.1.11" || service.updateReq.Name != "front-02" {
		t.Fatalf("update status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/terminals/terminal-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.deleted != "terminal-1" {
		t.Fatalf("delete status=%d service=%#v", rec.Code, service)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/terminals/heartbeat", bytes.NewBufferString(`{"mac":"AA:BB:CC:DD:EE:FF"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.heartbeatMAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("heartbeat status=%d service=%#v", rec.Code, service)
	}
}

func TestTerminalEndpointErrors(t *testing.T) {
	service := &fakeTerminalService{err: assets.ErrTerminalInvalidMAC}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodPost, "/api/terminals", bytes.NewBufferString(`{"mac":"bad"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", rec.Code)
	}

	service.err = assets.ErrTerminalMACExists
	req = httptest.NewRequest(http.MethodPost, "/api/terminals", bytes.NewBufferString(`{"mac":"aa"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", rec.Code)
	}

	service.err = sql.ErrNoRows
	req = httptest.NewRequest(http.MethodGet, "/api/terminals/terminal-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("notfound status = %d", rec.Code)
	}
}

func TestTerminalImportExportEndpoints(t *testing.T) {
	service := &fakeTerminalService{
		exportData:   []byte("xlsx"),
		importResult: assets.TerminalImportResult{Created: 1},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodGet, "/api/terminals/template", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("template status=%d content-type=%s", rec.Code, rec.Header().Get("Content-Type"))
	}

	req = httptest.NewRequest(http.MethodGet, "/api/terminals/export?group_id=grp-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "xlsx" || service.exportGroupID != "grp-1" {
		t.Fatalf("export status=%d service=%#v body=%q", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/terminals/import", bytes.NewBufferString("xlsx"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || string(service.importData) != "xlsx" {
		t.Fatalf("import status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.importResult = assets.TerminalImportResult{Errors: []assets.TerminalImportError{{Row: 2, Error: "bad"}}}
	req = httptest.NewRequest(http.MethodPost, "/api/terminals/import", bytes.NewBufferString("xlsx"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"row":2`) {
		t.Fatalf("import error status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMoveTerminalsEndpoint(t *testing.T) {
	service := &fakeTerminalService{moveResult: assets.MoveTerminalsResult{Moved: 1}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodPost, "/api/terminals/move", bytes.NewBufferString(`{"terminal_ids":["term-1"],"group_id":"grp-2"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.moveReq.GroupID != "grp-2" || len(service.moveReq.TerminalIDs) != 1 {
		t.Fatalf("move status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.err = assets.ErrTerminalMoveRequired
	req = httptest.NewRequest(http.MethodPost, "/api/terminals/move", bytes.NewBufferString(`{}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("move bad request status=%d", rec.Code)
	}
}

func TestTerminalSuperEndpoints(t *testing.T) {
	terminal := domain.Terminal{ID: "terminal-1", MAC: "AABBCCDDEEFF", IsSuper: true}
	service := &fakeTerminalService{item: terminal}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodPost, "/api/terminals/terminal-1/super", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.enableID != "terminal-1" {
		t.Fatalf("enable status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/terminals/terminal-1/super", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.disableID != "terminal-1" {
		t.Fatalf("disable status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.stopResult = assets.SuperStopResult{TaskID: "task-1"}
	req = httptest.NewRequest(http.MethodPost, "/api/terminals/terminal-1/super/stop", bytes.NewBufferString(`{"reduction_name":"saved"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.stopID != "terminal-1" || service.stopReq.ReductionName != "saved" {
		t.Fatalf("stop status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.err = assets.TerminalSuperConflict{ConfigID: "cfg-1", TerminalID: "terminal-2", MAC: "001122334466"}
	req = httptest.NewRequest(http.MethodPost, "/api/terminals/terminal-3/super", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "001122334466") {
		t.Fatalf("conflict status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPoolEndpoints(t *testing.T) {
	service := &fakePoolService{
		list: ops.PoolListResult{
			Items: []ops.PoolItem{{ID: "pool-tank2", ServerID: "server-a", Name: "tank2", Disks: []string{"/dev/sdb"}, Health: "ONLINE"}},
			Total: 1,
		},
		diskList:      ops.DiskListResult{Items: []storage.DiskInfo{{Path: "/dev/sdb", Name: "sdb", Type: "disk", Size: 1073741824}}, Total: 1},
		item:          ops.PoolItem{ID: "pool-tank2", Name: "tank2", Health: "ONLINE"},
		createResult:  ops.PoolOperationResult{TaskID: "task-create", Pool: ops.PoolItem{ID: "pool-tank2", Name: "tank2"}},
		destroyResult: ops.PoolTaskResult{TaskID: "task-destroy"},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Pools: service})

	req := httptest.NewRequest(http.MethodGet, "/api/storage/disks", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.listDisksCalled || !strings.Contains(rec.Body.String(), `"/dev/sdb"`) {
		t.Fatalf("disk list status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/pools?server_id=server-a", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.listServerID != "server-a" || !strings.Contains(rec.Body.String(), "ONLINE") {
		t.Fatalf("list status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools", bytes.NewBufferString(`{"name":"tank2","disks":["/dev/sdb"]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.createReq.Name != "tank2" || len(service.createReq.Disks) != 1 {
		t.Fatalf("create status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/pools/pool-tank2", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.getID != "pool-tank2" {
		t.Fatalf("get status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/disks", bytes.NewBufferString(`{"disks":["/dev/sdc"]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.addID != "pool-tank2" || service.addReq.Disks[0] != "/dev/sdc" {
		t.Fatalf("add disk status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/disks/remove", bytes.NewBufferString(`{"disk":"/dev/sdb"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.removeID != "pool-tank2" || service.removeReq.Disk != "/dev/sdb" {
		t.Fatalf("remove disk status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/mirror-upgrade", bytes.NewBufferString(`{"pairs":[{"target":"/dev/sdb","disk":"/dev/sdz"}]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.upgradeID != "pool-tank2" || len(service.upgradeReq.Pairs) != 1 || service.upgradeReq.Pairs[0].Disk != "/dev/sdz" {
		t.Fatalf("mirror upgrade status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	for _, tc := range []struct{ path, body, want string }{
		{"/api/pools/pool-tank2/special", `{"disks":["/dev/nvme2n1","/dev/nvme3n1"]}`, "special"},
		{"/api/pools/pool-tank2/special/remove", `{"disk":"mirror-3"}`, "special/remove"},
		{"/api/pools/pool-tank2/spares", `{"disks":["/dev/sdz"]}`, "spares"},
		{"/api/pools/pool-tank2/spares/remove", `{"disk":"/dev/sdz"}`, "spares/remove"},
	} {
		req = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body))
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted || service.lastVdevOp != tc.want || service.lastVdevID != "pool-tank2" {
			t.Fatalf("%s status=%d op=%q body=%s", tc.path, rec.Code, service.lastVdevOp, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/disks/replace", bytes.NewBufferString(`{"old_disk":"/dev/sdb","new_disk":"/dev/sdd"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.replaceID != "pool-tank2" || service.replaceReq.NewDisk != "/dev/sdd" {
		t.Fatalf("replace disk status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/read-cache", bytes.NewBufferString(`{"disks":["/dev/nvme0n1"]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.addReadCacheID != "pool-tank2" || service.addReadCacheReq.Disks[0] != "/dev/nvme0n1" {
		t.Fatalf("add read cache status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/read-cache/remove", bytes.NewBufferString(`{"disk":"/dev/nvme0n1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.removeReadCacheID != "pool-tank2" || service.removeReadCacheReq.Disk != "/dev/nvme0n1" {
		t.Fatalf("remove read cache status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/write-cache", bytes.NewBufferString(`{"disks":["/dev/nvme1n1"]}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.addWriteCacheID != "pool-tank2" || service.addWriteCacheReq.Disks[0] != "/dev/nvme1n1" {
		t.Fatalf("add write cache status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/write-cache/remove", bytes.NewBufferString(`{"disk":"/dev/nvme1n1"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.removeWriteCacheID != "pool-tank2" || service.removeWriteCacheReq.Disk != "/dev/nvme1n1" {
		t.Fatalf("remove write cache status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pools/pool-tank2/write-cache/flush", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.flushWriteCacheID != "pool-tank2" {
		t.Fatalf("flush write cache status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/pools/pool-tank2", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.destroyID != "pool-tank2" {
		t.Fatalf("delete status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.err = ops.ErrPoolInUse
	req = httptest.NewRequest(http.MethodDelete, "/api/pools/pool-tank2", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-use status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPoolEndpointErrors(t *testing.T) {
	service := &fakePoolService{err: ops.ErrPoolNameRequired}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Pools: service})

	req := httptest.NewRequest(http.MethodPost, "/api/pools", bytes.NewBufferString(`{"name":" ","disks":["/dev/sdb"]}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad request status=%d body=%s", rec.Code, rec.Body.String())
	}

	service.err = ops.ErrPoolInUse
	req = httptest.NewRequest(http.MethodDelete, "/api/pools/pool-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", rec.Code, rec.Body.String())
	}

	service.err = sql.ErrNoRows
	req = httptest.NewRequest(http.MethodGet, "/api/pools/pool-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not found status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUserEndpoints(t *testing.T) {
	service := &fakeUserService{
		list: platform.UserListResult{Items: []platform.UserInfo{{ID: "user-1", Username: "admin"}}, Total: 1},
		item: platform.UserInfo{ID: "user-2", Username: "ops"},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Users: service})

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "admin") {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(`{"username":"ops","password":"secret"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || service.createReq.Username != "ops" || service.createReq.Password != "secret" {
		t.Fatalf("create status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/users/user-2", bytes.NewBufferString(`{"username":"operator"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.updateID != "user-2" || service.updateReq.Username != "operator" {
		t.Fatalf("update status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/users/user-2/password", bytes.NewBufferString(`{"old_password":"old","new_password":"new"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.passwordID != "user-2" || service.passwordReq.NewPassword != "new" {
		t.Fatalf("password status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/users/user-2", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || service.deletedID != "user-2" {
		t.Fatalf("delete status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	service.err = platform.ErrUserLastAdmin
	req = httptest.NewRequest(http.MethodDelete, "/api/users/user-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("last admin status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBackupEndpoints(t *testing.T) {
	service := &fakeBackupService{
		config: domain.BackupConfig{ID: domain.BackupConfigDefaultID, Enabled: true, Schedule: "daily@03:00"},
		status: ops.BackupStatus{
			Config: ops.BackupConfigStatus{Enabled: true, Schedule: "daily@03:00", ScheduleText: "每天 03:00", LastTaskID: "task-1"},
			Node:   ops.NodeBackupStatus{BackupPool: "backup"},
		},
		run: ops.BackupRunResult{TaskID: "task-1"},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Backups: service})

	req := httptest.NewRequest(http.MethodGet, "/api/backup/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "daily@03:00") {
		t.Fatalf("config status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 目标池不再由请求携带：每台机器备份到自己的第二个池。
	req = httptest.NewRequest(http.MethodPut, "/api/backup/config", bytes.NewBufferString(`{"enabled":true,"schedule":"daily@03:00"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.saveReq.Enabled || service.saveReq.Schedule != "daily@03:00" {
		t.Fatalf("save status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/backup/run", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !service.runCalled || !strings.Contains(rec.Body.String(), "task-1") {
		t.Fatalf("run status=%d service=%#v body=%s", rec.Code, service, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"backup_pool":"backup"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	service.err = ops.ErrBackupRunning
	req = httptest.NewRequest(http.MethodPost, "/api/backup/run", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("running status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func testAssets() fs.FS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("index")},
		"assets/app.js": {Data: []byte("asset")},
	}
}

type fakeBootService struct {
	BootScriptService // 只写出测试用到的方法
	script            string
	err               error
	mac               string
	letters           []control.DataDiskLetter
	net               control.NetConfig
	registeredIP      string
	registeredErr     error
	failure           []string
	checked           []string
}

func (s *fakeBootService) RegisteredIP(_ context.Context, _ string) (string, error) {
	return s.registeredIP, s.registeredErr
}

func (s *fakeBootService) BuildBootScript(_ context.Context, mac string) (string, error) {
	s.mac = mac
	return s.script, s.err
}

func (s *fakeBootService) RecordBootFailure(_ context.Context, mac, stage, code, platform string) error {
	s.failure = []string{mac, stage, code, platform}
	return s.err
}

func (s *fakeBootService) CheckBootMode(_ context.Context, mac, platform string) error {
	s.checked = []string{mac, platform}
	return nil
}

func (s *fakeBootService) DataDiskLetters(_ context.Context, mac string) ([]control.DataDiskLetter, error) {
	s.mac = mac
	return s.letters, s.err
}

func (s *fakeBootService) NetConfig(_ context.Context, mac string) (control.NetConfig, error) {
	s.mac = mac
	return s.net, s.err
}

type fakeImageService struct {
	ImageImportService // 只写出测试用到的方法
	req                assets.ImportImageRequest
	result             assets.ImportImageResult
	list               assets.ImageListResult
	detail             assets.ImageDetail
	err                error
	deleted            string
	blankReq           assets.BlankImageRequest
	purposeID          string
	purpose            string
}

func (s *fakeImageService) ImportImage(_ context.Context, req assets.ImportImageRequest) (assets.ImportImageResult, error) {
	s.req = req
	return s.result, s.err
}

func (s *fakeImageService) ListImportSources(context.Context) (assets.ImportSourceListResult, error) {
	return assets.ImportSourceListResult{}, s.err
}

func (s *fakeImageService) CreateBlankImage(_ context.Context, req assets.BlankImageRequest) (assets.ImportImageResult, error) {
	s.blankReq = req
	return assets.ImportImageResult{TaskID: "task-create_blank_image-1", ImageID: req.Name}, s.err
}

func (s *fakeImageService) SetPurpose(_ context.Context, id string, req assets.SetPurposeRequest) (domain.Image, error) {
	s.purposeID, s.purpose = id, req.Purpose
	return domain.Image{ID: id, Purpose: domain.ImagePurpose(req.Purpose)}, s.err
}

func (s *fakeImageService) ListImages(context.Context) (assets.ImageListResult, error) {
	return s.list, s.err
}

func (s *fakeImageService) GetImage(_ context.Context, _ string) (assets.ImageDetail, error) {
	return s.detail, s.err
}

func (s *fakeImageService) DeleteImage(_ context.Context, id string) error {
	s.deleted = id
	return s.err
}

func (s *fakeImageService) RunHealthCheck(_ context.Context, id string) (assets.HealthCheckResult, error) {
	if s.err != nil {
		return assets.HealthCheckResult{}, s.err
	}
	return assets.HealthCheckResult{TaskID: "task-health-" + id}, nil
}

func (s *fakeImageService) GetHealth(_ context.Context, id string) (domain.ImageHealthReport, error) {
	if s.err != nil {
		return domain.ImageHealthReport{}, s.err
	}
	return domain.ImageHealthReport{ID: "health-" + id, ImageID: id, Level: domain.HealthOK}, nil
}

type fakeTaskService struct {
	TaskService // 只写出测试用到的方法
	id          string
	task        domain.Task
	err         error
	active      []domain.Task
	listErr     error
	listResult  assets.TaskListResult
	listQuery   assets.TaskListQuery
}

func (s *fakeTaskService) GetTask(_ context.Context, id string) (domain.Task, error) {
	s.id = id
	return s.task, s.err
}

func (s *fakeTaskService) ListActiveTasks(_ context.Context) ([]domain.Task, error) {
	return s.active, s.listErr
}

func (s *fakeTaskService) ListTasks(_ context.Context, q assets.TaskListQuery) (assets.TaskListResult, error) {
	s.listQuery = q
	return s.listResult, s.listErr
}

type fakeAuthService struct {
	AuthService    // 只写出测试用到的方法
	loginResult    platform.LoginResult
	loginErr       error
	loginReq       platform.LoginRequest
	verifyResult   platform.UserInfo
	verifyErr      error
	verifyCalls    int
	verifiedToken  string
	verifyErrToken string
}

func (s *fakeAuthService) Login(_ context.Context, req platform.LoginRequest) (platform.LoginResult, error) {
	s.loginReq = req
	if s.loginErr != nil {
		return platform.LoginResult{}, s.loginErr
	}
	return s.loginResult, nil
}

func (s *fakeAuthService) VerifyToken(token string) (platform.UserInfo, error) {
	s.verifyCalls++
	s.verifiedToken = token
	if s.verifyErr != nil {
		return platform.UserInfo{}, s.verifyErr
	}
	if s.verifyErrToken != "" && token == s.verifyErrToken {
		return platform.UserInfo{}, platform.ErrInvalidCredentials
	}
	return s.verifyResult, nil
}

type fakeConfigService struct {
	ConfigService          // 只写出测试用到的方法
	list                   assets.ConfigListResult
	listImageID            string
	createdFromImage       assets.ConfigOperationResult
	createdFromImageReq    assets.CreateConfigRequest
	createFromImageImageID string

	createdFromConfig   assets.ConfigOperationResult
	createFromConfigReq assets.CreateConfigFromConfigRequest
	createFromConfigID  string
	deleted             string
	deleteResult        assets.ConfigTaskResult
	merged              string
	mergeResult         assets.ConfigTaskResult
	err                 error
}

func (s *fakeConfigService) List(_ context.Context, imageID string) (assets.ConfigListResult, error) {
	s.listImageID = imageID
	return s.list, s.err
}

func (s *fakeConfigService) CreateFromImage(_ context.Context, imageID string, req assets.CreateConfigRequest) (assets.ConfigOperationResult, error) {
	s.createFromImageImageID = imageID
	s.createdFromImageReq = req
	return s.createdFromImage, s.err
}

func (s *fakeConfigService) CreateFromConfig(_ context.Context, configID string, req assets.CreateConfigFromConfigRequest) (assets.ConfigOperationResult, error) {
	s.createFromConfigID = configID
	s.createFromConfigReq = req
	return s.createdFromConfig, s.err
}

func (s *fakeConfigService) Delete(_ context.Context, configID string) (assets.ConfigTaskResult, error) {
	s.deleted = configID
	return s.deleteResult, s.err
}

func (s *fakeConfigService) Merge(_ context.Context, configID string) (assets.ConfigTaskResult, error) {
	s.merged = configID
	return s.mergeResult, s.err
}

type fakeReductionService struct {
	ReductionService // 只写出测试用到的方法
	list             assets.ReductionListResult
	listConfigID     string
	createResult     assets.ReductionOperationResult
	createConfigID   string
	createReq        assets.CreateReductionRequest
	deleteResult     assets.ReductionTaskResult
	deleted          string
	mergeResult      assets.ReductionTaskResult
	mergeConfigID    string
	mergeReq         assets.MergeReductionsRequest
	setCurrentID     string
	err              error
}

func (s *fakeReductionService) List(_ context.Context, configID string) (assets.ReductionListResult, error) {
	s.listConfigID = configID
	return s.list, s.err
}

func (s *fakeReductionService) Create(_ context.Context, configID string, req assets.CreateReductionRequest) (assets.ReductionOperationResult, error) {
	s.createConfigID = configID
	s.createReq = req
	return s.createResult, s.err
}

func (s *fakeReductionService) Delete(_ context.Context, id string) (assets.ReductionTaskResult, error) {
	s.deleted = id
	return s.deleteResult, s.err
}

func (s *fakeReductionService) SetCurrent(_ context.Context, id string) (assets.SetCurrentResult, error) {
	s.setCurrentID = id
	return assets.SetCurrentResult{ConfigID: "cfg-1", ReductionID: id}, s.err
}

func (s *fakeReductionService) Merge(_ context.Context, configID string, req assets.MergeReductionsRequest) (assets.ReductionTaskResult, error) {
	s.mergeConfigID = configID
	s.mergeReq = req
	return s.mergeResult, s.err
}

type fakeGroupService struct {
	GroupService // 只写出测试用到的方法
	list         assets.GroupListResult
	item         domain.Group
	err          error
	getID        string
	defaultRead  bool
	createReq    assets.GroupRequest
	updateID     string
	updateReq    assets.GroupRequest
	deleted      string
	defaultID    string
}

func (s *fakeGroupService) List(context.Context) (assets.GroupListResult, error) {
	return s.list, s.err
}

func (s *fakeGroupService) Get(_ context.Context, id string) (domain.Group, error) {
	s.getID = id
	return s.item, s.err
}

func (s *fakeGroupService) GetDefault(context.Context) (domain.Group, error) {
	s.defaultRead = true
	return s.item, s.err
}

func (s *fakeGroupService) Create(_ context.Context, req assets.GroupRequest) (domain.Group, error) {
	s.createReq = req
	return s.item, s.err
}

func (s *fakeGroupService) Update(_ context.Context, id string, req assets.GroupRequest) (domain.Group, error) {
	s.updateID = id
	s.updateReq = req
	return s.item, s.err
}

func (s *fakeGroupService) Delete(_ context.Context, id string) error {
	s.deleted = id
	return s.err
}

func (s *fakeGroupService) SetDefault(_ context.Context, id string) (domain.Group, error) {
	s.defaultID = id
	return s.item, s.err
}

type fakeGroupDiskService struct {
	GroupDiskService // 只写出测试用到的方法
	list             assets.GroupDiskListResult
	item             domain.GroupDisk
	err              error
	listGroupID      string
	createGroupID    string
	createReq        assets.GroupDiskRequest
	deleted          string
}

func (s *fakeGroupDiskService) List(_ context.Context, groupID string) (assets.GroupDiskListResult, error) {
	s.listGroupID = groupID
	return s.list, s.err
}

func (s *fakeGroupDiskService) Create(_ context.Context, groupID string, req assets.GroupDiskRequest) (domain.GroupDisk, error) {
	s.createGroupID = groupID
	s.createReq = req
	return s.item, s.err
}

func (s *fakeGroupDiskService) Delete(_ context.Context, id string) error {
	s.deleted = id
	return s.err
}

type fakeTerminalService struct {
	TerminalService // 只写出测试用到的方法
	list            assets.TerminalListResult
	item            domain.Terminal
	err             error
	listGroupID     string
	getID           string
	createReq       assets.TerminalRequest
	updateID        string
	updateReq       assets.TerminalRequest
	deleted         string
	exportData      []byte
	exportGroupID   string
	importData      []byte
	importResult    assets.TerminalImportResult
	moveReq         assets.MoveTerminalsRequest
	moveResult      assets.MoveTerminalsResult
	enableID        string
	disableID       string
	publishID       string
	publishReq      assets.PublishDataDiskRequest
	publishResult   assets.PublishDataDiskResult
	superDisks      assets.SuperDiskListResult
	superDisksID    string
	stopID          string
	stopReq         assets.SuperStopRequest
	stopResult      assets.SuperStopResult
	heartbeatMAC    string
}

func (s *fakeTerminalService) List(_ context.Context, groupID string) (assets.TerminalListResult, error) {
	s.listGroupID = groupID
	return s.list, s.err
}

func (s *fakeTerminalService) Get(_ context.Context, id string) (domain.Terminal, error) {
	s.getID = id
	return s.item, s.err
}

func (s *fakeTerminalService) Create(_ context.Context, req assets.TerminalRequest) (domain.Terminal, error) {
	s.createReq = req
	return s.item, s.err
}

func (s *fakeTerminalService) Update(_ context.Context, id string, req assets.TerminalRequest) (domain.Terminal, error) {
	s.updateID = id
	s.updateReq = req
	return s.item, s.err
}

func (s *fakeTerminalService) Delete(_ context.Context, id string) error {
	s.deleted = id
	return s.err
}

func (s *fakeTerminalService) Export(_ context.Context, groupID string) ([]byte, error) {
	s.exportGroupID = groupID
	return s.exportData, s.err
}

func (s *fakeTerminalService) Import(_ context.Context, data []byte) (assets.TerminalImportResult, error) {
	s.importData = append([]byte{}, data...)
	return s.importResult, s.err
}

func (s *fakeTerminalService) Move(_ context.Context, req assets.MoveTerminalsRequest) (assets.MoveTerminalsResult, error) {
	s.moveReq = req
	return s.moveResult, s.err
}

func (s *fakeTerminalService) Heartbeat(_ context.Context, mac string) (domain.Terminal, error) {
	s.heartbeatMAC = mac
	return s.item, s.err
}

func (s *fakeTerminalService) EnableSuper(_ context.Context, id string) (domain.Terminal, error) {
	s.enableID = id
	return s.item, s.err
}

func (s *fakeTerminalService) DisableSuper(_ context.Context, id string) (domain.Terminal, error) {
	s.disableID = id
	return s.item, s.err
}

func (s *fakeTerminalService) StopSuper(_ context.Context, id string, req assets.SuperStopRequest) (assets.SuperStopResult, error) {
	s.stopID = id
	s.stopReq = req
	return s.stopResult, s.err
}

type fakePoolService struct {
	PoolService           // 只写出测试用到的方法
	list                  ops.PoolListResult
	diskList              ops.DiskListResult
	item                  ops.PoolItem
	err                   error
	listServerID          string
	listDisksCalled       bool
	getID                 string
	upgradeID             string
	upgradeReq            ops.MirrorUpgradeRequest
	lastVdevOp            string
	lastVdevID            string
	createReq             ops.PoolRequest
	createResult          ops.PoolOperationResult
	addID                 string
	addReq                ops.PoolDiskRequest
	removeID              string
	removeReq             ops.PoolDiskRequest
	replaceID             string
	replaceReq            ops.PoolReplaceDiskRequest
	addReadCacheID        string
	addReadCacheReq       ops.PoolReadCacheRequest
	removeReadCacheID     string
	removeReadCacheReq    ops.PoolReadCacheRequest
	addWriteCacheID       string
	addWriteCacheReq      ops.PoolWriteCacheRequest
	removeWriteCacheID    string
	removeWriteCacheReq   ops.PoolWriteCacheRequest
	flushWriteCacheID     string
	flushWriteCacheResult ops.PoolTaskResult
	destroyID             string
	destroyResult         ops.PoolTaskResult
}

func (s *fakePoolService) List(_ context.Context, serverID string) (ops.PoolListResult, error) {
	s.listServerID = serverID
	return s.list, s.err
}

func (s *fakePoolService) ListDisks(context.Context) (ops.DiskListResult, error) {
	s.listDisksCalled = true
	return s.diskList, s.err
}

func (s *fakePoolService) Get(_ context.Context, id string) (ops.PoolItem, error) {
	s.getID = id
	return s.item, s.err
}

func (s *fakePoolService) Create(_ context.Context, req ops.PoolRequest) (ops.PoolOperationResult, error) {
	s.createReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) AddDisk(_ context.Context, id string, req ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.addID = id
	s.addReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) AddSpecial(_ context.Context, id string, _ ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.lastVdevOp, s.lastVdevID = "special", id
	return s.createResult, s.err
}
func (s *fakePoolService) RemoveSpecial(_ context.Context, id string, _ ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.lastVdevOp, s.lastVdevID = "special/remove", id
	return s.createResult, s.err
}
func (s *fakePoolService) AddSpare(_ context.Context, id string, _ ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.lastVdevOp, s.lastVdevID = "spares", id
	return s.createResult, s.err
}
func (s *fakePoolService) RemoveSpare(_ context.Context, id string, _ ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.lastVdevOp, s.lastVdevID = "spares/remove", id
	return s.createResult, s.err
}

func (s *fakePoolService) MirrorUpgrade(_ context.Context, id string, req ops.MirrorUpgradeRequest) (ops.PoolOperationResult, error) {
	s.upgradeID = id
	s.upgradeReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) RemoveDisk(_ context.Context, id string, req ops.PoolDiskRequest) (ops.PoolOperationResult, error) {
	s.removeID = id
	s.removeReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) ReplaceDisk(_ context.Context, id string, req ops.PoolReplaceDiskRequest) (ops.PoolOperationResult, error) {
	s.replaceID = id
	s.replaceReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) AddReadCache(_ context.Context, id string, req ops.PoolReadCacheRequest) (ops.PoolOperationResult, error) {
	s.addReadCacheID = id
	s.addReadCacheReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) RemoveReadCache(_ context.Context, id string, req ops.PoolReadCacheRequest) (ops.PoolOperationResult, error) {
	s.removeReadCacheID = id
	s.removeReadCacheReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) AddWriteCache(_ context.Context, id string, req ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error) {
	s.addWriteCacheID = id
	s.addWriteCacheReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) RemoveWriteCache(_ context.Context, id string, req ops.PoolWriteCacheRequest) (ops.PoolOperationResult, error) {
	s.removeWriteCacheID = id
	s.removeWriteCacheReq = req
	return s.createResult, s.err
}

func (s *fakePoolService) FlushWriteCache(_ context.Context, id string) (ops.PoolTaskResult, error) {
	s.flushWriteCacheID = id
	return s.flushWriteCacheResult, s.err
}

func (s *fakePoolService) Destroy(_ context.Context, id string) (ops.PoolTaskResult, error) {
	s.destroyID = id
	return s.destroyResult, s.err
}

type fakeUserService struct {
	UserService // 只写出测试用到的方法
	list        platform.UserListResult
	item        platform.UserInfo
	err         error
	createReq   platform.CreateUserRequest
	updateID    string
	updateReq   platform.UpdateUserRequest
	deletedID   string
	passwordID  string
	passwordReq platform.ChangePasswordRequest
}

func (s *fakeUserService) List(context.Context) (platform.UserListResult, error) {
	return s.list, s.err
}

func (s *fakeUserService) Create(_ context.Context, req platform.CreateUserRequest) (platform.UserInfo, error) {
	s.createReq = req
	return s.item, s.err
}

func (s *fakeUserService) Update(_ context.Context, id string, req platform.UpdateUserRequest) (platform.UserInfo, error) {
	s.updateID = id
	s.updateReq = req
	return s.item, s.err
}

func (s *fakeUserService) Delete(_ context.Context, id string) error {
	s.deletedID = id
	return s.err
}

func (s *fakeUserService) ChangePassword(_ context.Context, id string, req platform.ChangePasswordRequest) error {
	s.passwordID = id
	s.passwordReq = req
	return s.err
}

type fakeBackupService struct {
	BackupService // 只写出测试用到的方法
	config        domain.BackupConfig
	status        ops.BackupStatus
	run           ops.BackupRunResult
	err           error
	saveReq       ops.BackupConfigRequest
	runCalled     bool
}

func (s *fakeBackupService) GetConfig(context.Context) (domain.BackupConfig, error) {
	return s.config, s.err
}

func (s *fakeBackupService) SaveConfig(_ context.Context, req ops.BackupConfigRequest) (domain.BackupConfig, error) {
	s.saveReq = req
	return s.config, s.err
}

func (s *fakeBackupService) Run(context.Context) (ops.BackupRunResult, error) {
	s.runCalled = true
	return s.run, s.err
}

func (s *fakeBackupService) Status(context.Context) (ops.BackupStatus, error) {
	return s.status, s.err
}

type fakeSettingsService struct {
	SettingsService // 只写出测试用到的方法
	view            platform.SystemSettingsView
	saveReq         platform.SystemSettingsRequest
	err             error
}

func (s *fakeSettingsService) Get(context.Context) (platform.SystemSettingsView, error) {
	return s.view, s.err
}

func (s *fakeSettingsService) Save(_ context.Context, req platform.SystemSettingsRequest) (platform.SystemSettingsView, error) {
	s.saveReq = req
	if s.err != nil {
		return platform.SystemSettingsView{}, s.err
	}
	s.view.ImportDir = req.ImportDir
	return s.view, nil
}

// TestEveryAPIRouteRequiresAuth 遍历所有已注册路由，证明认证中间件都覆盖到：不带 token 的请求必须在任何
// handler 运行前返回 401（服务都是零值，走到就会 panic）。用来抓将来注册在认证组外的路由。
func TestEveryAPIRouteRequiresAuth(t *testing.T) {
	svc := Services{
		Auth:        platform.AuthService{Secret: []byte("test-secret")},
		Images:      assets.ImageService{},
		Configs:     assets.ConfigService{},
		Reductions:  assets.ReductionService{},
		Groups:      assets.GroupService{},
		GroupDisks:  assets.GroupDiskService{},
		Terminals:   assets.TerminalService{},
		Pools:       ops.PoolService{},
		Users:       platform.UserService{},
		Backups:     ops.BackupService{},
		Settings:    platform.SystemSettingsService{},
		Tasks:       assets.ImageService{},
		Drivers:     adapt.DriverService{},
		Adaptations: adapt.AdaptationService{},
		Server:      platform.ServiceService{},
		Audit:       platform.AuditService{},
		Alarms:      platform.AlarmService{},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, svc)

	chiRouter, ok := router.(chi.Router)
	if !ok {
		t.Fatalf("router is %T, not chi.Router", router)
	}
	routes := 0
	err := chi.Walk(chiRouter, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/") {
			return nil // healthz、/boot*、SPA 回退按设计公开
		}
		if route == "/api/login" || route == "/api/terminals/heartbeat" {
			return nil // 有意的白名单
		}
		routes++
		path := strings.ReplaceAll(route, "{id}", "x")
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d without token, want 401", method, route, rec.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if routes < 80 {
		t.Fatalf("walked only %d /api routes — registration shrank?", routes)
	}
}

// TestUsersCRUDRoundTrip 用真实 store 和服务走完整 HTTP 栈：登录、创建（201）、列表、更新、
// 坏 JSON（400）、改密码（204）、缺失 id（404）、删除（204）。
func TestUsersCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "file:"+filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	auth := platform.AuthService{Store: st, Secret: []byte("test-secret")}
	if err := auth.EnsureInitialAdmin(ctx, "admin", "admin-pass"); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{
		Auth:  auth,
		Users: platform.UserService{Store: st},
	})

	do := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 登录
	rec := do(http.MethodPost, "/api/login", "", `{"username":"admin","password":"admin-pass"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var login platform.LoginResult
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}

	// 创建 → 201
	rec = do(http.MethodPost, "/api/users", login.Token, `{"username":"ops","password":"ops-pass-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created platform.UserInfo
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// 坏 JSON → 400，带标准提示
	rec = do(http.MethodPost, "/api/users", login.Token, `{oops`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "请求格式错误") {
		t.Fatalf("bad json: %d %s", rec.Code, rec.Body.String())
	}

	// 列表 → 200，包含新用户
	rec = do(http.MethodGet, "/api/users", login.Token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ops") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// 更新 → 200
	rec = do(http.MethodPut, "/api/users/"+created.ID, login.Token, `{"username":"ops2"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ops2") {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	// 更新不存在的用户 → 404
	rec = do(http.MethodPut, "/api/users/user-ghost", login.Token, `{"username":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update ghost: %d %s", rec.Code, rec.Body.String())
	}

	// 改密码 → 204（handleErrIDIn 形态）
	rec = do(http.MethodPost, "/api/users/"+created.ID+"/password", login.Token, `{"old_password":"ops-pass-1","new_password":"ops-pass-2"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password: %d %s", rec.Code, rec.Body.String())
	}

	// 删除 → 204，再删 → 404
	rec = do(http.MethodDelete, "/api/users/"+created.ID, login.Token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodDelete, "/api/users/"+created.ID, login.Token, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete ghost: %d %s", rec.Code, rec.Body.String())
	}
}

// keepalived 的 track_script 读 /healthz 决定能否持有 VIP；接了检查器时必须反映真实健康，
// 静态 ok 会让 VIP 留在池已丢失的节点上。
func TestHealthzReflectsInjectedChecker(t *testing.T) {
	bad := errors.New("存储池不可读")
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Health: func(context.Context) error { return bad }})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "存储池不可读") {
		t.Fatalf("body = %s", rec.Body.String())
	}

	router = NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Health: func(context.Context) error { return nil }})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}

	// 没接检查器（测试、精简部署）：保持静态 ok。
	router = NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

// 备机闸门关闭：所有写操作和启动路径返回指向主机的 503；读、登录和健康检查保持开放。
func TestGateClosedBlocksWritesAndBootButNotReads(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("本节点为备机", "http://192.168.50.10:8080")
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: gate})

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/groups", http.StatusServiceUnavailable},
		{"DELETE", "/api/terminals/t1", http.StatusServiceUnavailable},
		{"GET", "/boot?mac=aa:bb:cc:dd:ee:ff", http.StatusServiceUnavailable},
		{"GET", "/boot/failed?mac=aa:bb:cc:dd:ee:ff&stage=sanboot", http.StatusServiceUnavailable},
		{"POST", "/api/login", http.StatusNotFound}, // 保持开放（这里没接认证服务）
		{"GET", "/healthz", http.StatusOK},
		{"GET", "/api/groups", http.StatusNotFound}, // 读路径通过闸门（404：这里没接服务）
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s %s = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if tc.want == http.StatusServiceUnavailable {
			if loc := rec.Header().Get("Location"); loc != "http://192.168.50.10:8080" {
				t.Fatalf("%s %s Location = %q", tc.method, tc.path, loc)
			}
			if !strings.Contains(rec.Body.String(), "备机") {
				t.Fatalf("body = %s", rec.Body.String())
			}
		}
	}

	// 闸门打开：不干预（照常 404 或认证错误，绝不 503）。
	gate.Open()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("POST", "/api/groups", nil))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("open gate still blocking: %d", rec.Code)
	}
}

// 这个接口没有请求体：用要求请求体的处理器接它，每次导出都会 400，而单测带着 body 就发现不了。
func TestExportToDirNeedsNoBody(t *testing.T) {
	service := &fakeExportDirService{}
	router := newRouter(Services{Images: service})
	rec := do(router, http.MethodPost, "/api/images/win11/export", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if service.exportedID != "win11" {
		t.Fatalf("exported = %q", service.exportedID)
	}
	if !strings.Contains(rec.Body.String(), "192.168.10.3") {
		t.Fatalf("回答里没说落在哪台：%s", rec.Body.String())
	}
}

func TestExportReductionToDirNeedsNoBody(t *testing.T) {
	service := &fakeExportDirService{}
	router := newRouter(Services{Images: service})
	rec := do(router, http.MethodPost, "/api/reductions/red-1/export", "")
	if rec.Code != http.StatusAccepted || service.exportedID != "red-1" || !strings.Contains(rec.Body.String(), "192.168.10.3") {
		t.Fatalf("status = %d, exported = %q, body %s", rec.Code, service.exportedID, rec.Body.String())
	}
}

type fakeExportDirService struct {
	ImageImportService
	exportedID string
}

func (s *fakeExportDirService) ExportReductionToDir(_ context.Context, id string) (assets.ExportImageResult, error) {
	s.exportedID = id
	return assets.ExportImageResult{TaskID: "task-1", Path: "/var/lib/ndiskless/imports/Win 11-0.zfs", Node: "192.168.10.3"}, nil
}

func (s *fakeExportDirService) ExportImageToDir(_ context.Context, req assets.ImageExportRequest) (assets.ExportImageResult, error) {
	s.exportedID = req.ImageID
	return assets.ExportImageResult{TaskID: "task-1", Path: "/var/lib/ndiskless/imports/Win 11.zfs", Node: "192.168.10.3"}, nil
}

// 备机也要能回答网关和盘符：多数客户机的盘由备机供，/boot/net-config 和 /boot/data-disks 是纯读，
// 被写闸门拦成 503 的话，客户机有盘没网络配置，开不起来。
func TestStandbyStillAnswersTheReadOnlyBootEndpoints(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("本节点为备机", "http://192.168.50.10:8080")
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: gate})

	// /boot 本身要建克隆、导出 LUN，是写：备机照旧拒绝。
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/boot?mac=aa:bb:cc:dd:ee:ff", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/boot 在备机上应当仍被挡下，得到 %d", rec.Code)
	}

	// 两个只读端点必须放行（这里没接 Boot 服务，过闸门后是 404 而不是 503）。
	for _, path := range []string{"/boot/net-config?mac=aa:bb:cc:dd:ee:ff", "/boot/data-disks?mac=aa:bb:cc:dd:ee:ff"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusServiceUnavailable {
			t.Fatalf("%s 是纯读，备机上不该被写闸门挡下：%d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// 备机答不上来就转发给写入者：备机在用库不跟随写入者，认不得被放置到它上面的客户机。
// 转发必须带上客户机自己的地址，且该头只在出示集群令牌时采信。
func TestStandbyForwardsUnknownBootReadsToTheWriter(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("本节点为备机", "http://192.168.50.10:8080")

	var gotPath, gotClient string
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{
		Gate: gate, Boot: &emptyBootService{},
		BootReadProxy: func(_ context.Context, path, query, clientIP, _ string) (int, []byte, string, error) {
			gotPath, gotClient = path+"?"+query, clientIP
			return http.StatusOK, []byte(`{"gateway":"192.168.10.2"}`), "application/json", nil
		},
	})

	req := httptest.NewRequest("GET", "/boot/net-config?mac=aa:bb:cc:dd:ee:ff", nil)
	req.RemoteAddr = "192.168.10.10:5000"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "192.168.10.2") {
		t.Fatalf("备机应把答不上来的只读请求转给写入者：%d %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/boot/net-config?mac=aa:bb:cc:dd:ee:ff" {
		t.Fatalf("转发的路径不对：%q", gotPath)
	}
	if gotClient != "192.168.10.10" {
		t.Fatalf("转发要带上客户机自己的地址，得到 %q", gotClient)
	}
}

// 备机本地库不跟随写入者：认得这台机器也不知道刚加的数据盘，照本地答就是空列表。
func TestStandbyAsksTheWriterEvenForMachinesItKnows(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("本节点为备机", "http://192.168.50.10:8080")
	local := &fakeBootService{letters: nil}
	proxied := 0
	proxy := func(_ context.Context, _, _, _, _ string) (int, []byte, string, error) {
		proxied++
		return http.StatusOK, []byte("1\t/data\n"), "text/plain; charset=utf-8", nil
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: gate, Boot: local, BootReadProxy: proxy})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/boot/data-disks?mac=aa:bb&format=text", nil))
	if rec.Body.String() != "1\t/data\n" || proxied != 1 {
		t.Fatalf("standby answered from its own stale catalogue: %q proxied=%d", rec.Body.String(), proxied)
	}

	// 写入者问不到时，本地的答案总比没有好。
	router = NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: gate,
		Boot: &fakeBootService{letters: []control.DataDiskLetter{{LUN: 1, Letter: "/old"}}},
		BootReadProxy: func(context.Context, string, string, string, string) (int, []byte, string, error) {
			return 0, nil, "", errors.New("writer unreachable")
		}})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/boot/data-disks?mac=aa:bb&format=text", nil))
	if rec.Body.String() != "1\t/old\n" {
		t.Fatalf("fallback = %q", rec.Body.String())
	}

	// 写入者一侧（闸门开着）照本地答，不转发。
	open := ha.NewOpenGate()
	proxied = 0
	router = NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: open, Boot: local, BootReadProxy: proxy})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boot/data-disks?mac=aa:bb", nil))
	if proxied != 0 {
		t.Fatal("the writer forwarded its own answer")
	}
}

// 写入者只在对方出示集群令牌时才采信客户机地址转发头，否则任何人都能用它冒充别的机器。
func TestForwardedClientAddressIsTrustedOnlyFromAClusterPeer(t *testing.T) {
	svc := &fixedIPBootService{ip: "192.168.10.10"}
	mkAdmin := func(cluster, forwarded string) bool {
		r := httptest.NewRequest("GET", "/boot/net-config?mac=aa:bb:cc:dd:ee:ff", nil)
		r.RemoteAddr = "192.168.10.4:6000"
		r.Header.Set("Authorization", "Bearer admin-jwt")
		r.Header.Set("X-ND-Cluster", cluster)
		r.Header.Set("X-ND-Client", forwarded)
		ok, _ := answersFor(r, svc, &fakeAuthService{verifyResult: platform.UserInfo{Username: "admin"}},
			"aa:bb:cc:dd:ee:ff", false, "cluster-secret")
		return ok
	}
	mk := func(bearer, forwarded string) bool {
		r := httptest.NewRequest("GET", "/boot/net-config?mac=aa:bb:cc:dd:ee:ff", nil)
		r.RemoteAddr = "192.168.10.4:6000" // 转发它的那台备机
		if bearer != "" {
			r.Header.Set("X-ND-Cluster", bearer)
		}
		if forwarded != "" {
			r.Header.Set("X-ND-Client", forwarded)
		}
		ok, _ := answersFor(r, svc, nil, "aa:bb:cc:dd:ee:ff", false, "cluster-secret")
		return ok
	}
	if !mk("cluster-secret", "192.168.10.10") {
		t.Fatal("集群节点转发、且客户机地址对得上时应当放行")
	}
	// 原始调用方的身份也要带过来：备机吞掉管理员会话的话，同一条命令在主机上能答、在备机上 404。
	if !mkAdmin("cluster-secret", "192.168.10.99") {
		t.Fatal("转发里带着有效的管理员会话时应当放行，与转发地址无关")
	}
	if mk("cluster-secret", "192.168.10.99") {
		t.Fatal("转发的地址对不上仍要拒绝")
	}
	if mk("", "192.168.10.10") {
		t.Fatal("没有集群令牌就不能用转发头冒充别的机器")
	}
	if mk("wrong-token", "192.168.10.10") {
		t.Fatal("令牌不对就不能用转发头")
	}
}

// 只实现 /boot 族接口的桩：认不得任何 MAC（备机的处境）。
type emptyBootService struct{}

func (emptyBootService) BuildBootScript(context.Context, string) (string, error) {
	return "", control.ErrUnknownTerminal
}
func (emptyBootService) DataDiskLetters(context.Context, string) ([]control.DataDiskLetter, error) {
	return nil, control.ErrUnknownTerminal
}
func (emptyBootService) NetConfig(context.Context, string) (control.NetConfig, error) {
	return control.NetConfig{}, control.ErrUnknownTerminal
}
func (emptyBootService) RegisteredIP(context.Context, string) (string, error) { return "", nil }
func (emptyBootService) RecordBootFailure(context.Context, string, string, string, string) error {
	return control.ErrUnknownTerminal
}
func (emptyBootService) CheckBootMode(context.Context, string, string) error { return nil }

// 认得这台机器，并报出它登记的地址。
type fixedIPBootService struct {
	emptyBootService
	ip string
}

func (s *fixedIPBootService) RegisteredIP(context.Context, string) (string, error) {
	return s.ip, nil
}

func (s *fakeTerminalService) PublishDataDisk(_ context.Context, id string, req assets.PublishDataDiskRequest) (assets.PublishDataDiskResult, error) {
	s.publishID, s.publishReq = id, req
	return s.publishResult, s.err
}

func (s *fakeTerminalService) SuperDisks(_ context.Context, id string) (assets.SuperDiskListResult, error) {
	s.superDisksID = id
	return s.superDisks, s.err
}

// 数据盘在线发布的两个接口：列出超管机的数据盘及未发布量，以及发布一块盘。发布是任务，按 202 处理；
// 被拒绝（如盘还在写）要带着原因回到运维面前，而不是 500。
func TestSuperPublishEndpoints(t *testing.T) {
	service := &fakeTerminalService{
		superDisks: assets.SuperDiskListResult{Items: []assets.SuperDiskItem{
			{DiskID: "gd-1", LUN: 1, MountTarget: "D:", ConfigName: "游戏盘", CurrentName: "9月14日更新", Written: 13 << 30, Ready: true},
		}, Total: 1},
		publishResult: assets.PublishDataDiskResult{TaskID: "task-publish-1"},
	}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Terminals: service})

	req := httptest.NewRequest(http.MethodGet, "/api/terminals/terminal-1/super/disks", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.superDisksID != "terminal-1" || !strings.Contains(rec.Body.String(), "游戏盘") {
		t.Fatalf("disks status=%d id=%q body=%s", rec.Code, service.superDisksID, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/terminals/terminal-1/super/publish", bytes.NewBufferString(`{"disk_id":"gd-1","name":"装了永劫无间"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || service.publishID != "terminal-1" || service.publishReq.DiskID != "gd-1" || service.publishReq.Name != "装了永劫无间" {
		t.Fatalf("publish status=%d service=%#v", rec.Code, service)
	}

	// 拒绝原样回到界面：409 + 原因。
	service.err = errs.Conflict("D: 正在写入，等它写完再发布")
	req = httptest.NewRequest(http.MethodPost, "/api/terminals/terminal-1/super/publish", bytes.NewBufferString(`{"disk_id":"gd-1","name":"x"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "正在写入") {
		t.Fatalf("refusal status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 保留副本在保住它的那台上，写入者靠这个只读接口去问；走集群令牌，不给浏览器用。
func TestNodePreservedEndpointNeedsTheClusterToken(t *testing.T) {
	svc := Services{ClusterToken: "tok", NodePreserved: func(context.Context) ([]storage.PreservedCopy, error) {
		return []storage.PreservedCopy{{Dataset: "data/nd-diverged-1", Kind: storage.PreservedDiverged, Points: 3, Used: 13 << 30}}, nil
	}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, svc)

	req := httptest.NewRequest(http.MethodGet, "/internal/node/preserved", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("没带令牌不该放行：%d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/internal/node/preserved", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nd-diverged-1") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 池操作任务记在执行它的节点上，写入者靠这个接口去取；走集群令牌。
func TestNodeTasksEndpointNeedsTheClusterToken(t *testing.T) {
	svc := Services{ClusterToken: "tok", NodeTasks: func(context.Context) ([]domain.Task, error) {
		return []domain.Task{{ID: "task-create_pool-1", Type: domain.TaskTypeCreatePool, Status: domain.TaskStatusFailed}}, nil
	}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, svc)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/node/tasks", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("没带令牌不该放行：%d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/node/tasks", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "task-create_pool-1") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 备机库里登记的是改网段前的旧地址：不能拿它先把客户机拒掉，要先交写入者按客户机地址核对；
// 写入者问不到时才退回本地核对，旧地址对不上照旧拒绝，不能因为退回就替别人回答。
func TestStandbyForwardsBeforeCheckingItsStaleAddress(t *testing.T) {
	gate := ha.NewOpenGate()
	gate.Close("本节点为备机", "http://192.168.50.10:8080")
	local := &fakeBootService{registeredIP: "10.0.0.7", net: control.NetConfig{Gateway: "10.0.0.1"}}
	var gotClient string
	proxy := func(_ context.Context, _, _, clientIP, _ string) (int, []byte, string, error) {
		gotClient = clientIP
		return http.StatusOK, []byte(`{"gateway":"10.0.1.1"}`), "application/json", nil
	}
	ask := func(p func(context.Context, string, string, string, string) (int, []byte, string, error), path string) *httptest.ResponseRecorder {
		router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Gate: gate, Boot: local, BootReadProxy: p})
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "10.0.1.9:5000" // 客户机的新地址
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/boot/net-config?mac=aa:bb", "/boot/data-disks?mac=aa:bb"} {
		gotClient = ""
		rec := ask(proxy, path)
		if rec.Code != http.StatusOK || gotClient != "10.0.1.9" {
			t.Fatalf("%s：备机应先转给写入者并带客户机地址，得到 %d %s client=%q", path, rec.Code, rec.Body.String(), gotClient)
		}
	}
	down := func(context.Context, string, string, string, string) (int, []byte, string, error) {
		return 0, nil, "", errors.New("writer unreachable")
	}
	if rec := ask(down, "/boot/net-config?mac=aa:bb"); rec.Code != http.StatusNotFound {
		t.Fatalf("写入者问不到时应按本地登记地址核对并拒绝，得到 %d %s", rec.Code, rec.Body.String())
	}
}
