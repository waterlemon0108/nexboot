package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeExportImageService struct {
	ImageImportService // handler 用到未写出的方法时会 panic
	exportErr          error
	streamedID         string
	stream             string
	name               string
	compressAsked      bool
	streamCompressed   bool
}

func (s *fakeExportImageService) ExportDownloadName(_ context.Context, id string, compress bool) (string, error) {
	if s.exportErr != nil {
		return "", s.exportErr
	}
	if compress {
		s.compressAsked = true
	}
	return s.name, nil
}

func (s *fakeExportImageService) StreamImage(_ context.Context, id string, w io.Writer, compress bool) error {
	s.streamedID = id
	s.streamCompressed = compress
	_, err := io.WriteString(w, s.stream)
	return err
}

func TestExportDownloadTicketFlow(t *testing.T) {
	service := &fakeExportImageService{name: "教学镜像.zfs", stream: "zfs-raw-stream"}
	router := newRouter(Services{Images: service})

	rec := do(router, http.MethodPost, "/api/images/win11/export-ticket", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket status = %d, body %s", rec.Code, rec.Body.String())
	}
	var ticket struct {
		Token     string `json:"token"`
		URL       string `json:"url"`
		ExpiresIn int    `json:"expires_in"`
		FileName  string `json:"file_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Token == "" || ticket.ExpiresIn <= 0 || ticket.FileName != "教学镜像.zfs" {
		t.Fatalf("ticket = %#v", ticket)
	}
	if !strings.Contains(ticket.URL, "token="+ticket.Token) {
		t.Fatalf("url = %q", ticket.URL)
	}

	rec = do(router, http.MethodGet, ticket.URL, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body %s", rec.Code, rec.Body.String())
	}
	if service.streamedID != "win11" {
		t.Fatalf("streamed id = %q", service.streamedID)
	}
	if rec.Body.String() != "zfs-raw-stream" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type = %q", ct)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "filename*=UTF-8''") || !strings.Contains(disposition, ".zfs") {
		t.Fatalf("disposition = %q", disposition)
	}

	// 一次性：同一张票据第二次使用被拒。
	rec = do(router, http.MethodGet, ticket.URL, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second use status = %d", rec.Code)
	}
}

func TestExportDownloadRefusesBadToken(t *testing.T) {
	router := newRouter(Services{Images: &fakeExportImageService{}})
	rec := do(router, http.MethodGet, "/api/images/export-download?token=forged", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

// 票据自行过期：不需要有请求来触发清理。
func TestExportTicketsExpire(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	tickets := newExportTickets(func() time.Time { return now })
	token, err := tickets.Issue("win11", false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := tickets.Consume(token); ok {
		t.Fatal("expired ticket accepted")
	}
}

// 要求压缩导出时必须产出真正的 .gz：文件名是 .zfs.gz、内容是 gzip，且选择随票据带给不带请求体的浏览器 GET。
func TestExportDownloadCompressed(t *testing.T) {
	service := &fakeExportImageService{name: "教学镜像.zfs.gz", stream: "zfs-raw-stream"}
	router := newRouter(Services{Images: service})

	rec := do(router, http.MethodPost, "/api/images/win11/export-ticket?compress=gzip", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket status = %d, body %s", rec.Code, rec.Body.String())
	}
	var ticket struct {
		URL      string `json:"url"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.FileName != "教学镜像.zfs.gz" {
		t.Fatalf("file name = %q", ticket.FileName)
	}
	if !service.compressAsked {
		t.Fatal("service was not told the export is compressed")
	}

	rec = do(router, http.MethodGet, ticket.URL, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/gzip" {
		t.Fatalf("content type = %q", got)
	}
	// 不能设 Content-Encoding：浏览器会静默解压，存下一个不是 gzip 的 .gz；载荷本身就是归档。
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding = %q, want none", got)
	}
	if !service.streamCompressed {
		t.Fatal("StreamImage was not asked to compress")
	}
}

// 默认不压缩：裸 send 已带着池自身的压缩，在快速局域网上 gzip 反而更慢。
func TestExportDownloadUncompressedByDefault(t *testing.T) {
	service := &fakeExportImageService{name: "教学镜像.zfs", stream: "zfs-raw-stream"}
	router := newRouter(Services{Images: service})

	rec := do(router, http.MethodPost, "/api/images/win11/export-ticket", "")
	var ticket struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	rec = do(router, http.MethodGet, ticket.URL, "")
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content type = %q", got)
	}
	if service.streamCompressed {
		t.Fatal("StreamImage compressed an export nobody asked to compress")
	}
}

// 流到一半失败的导出服务：已经写出一部分字节后报错。
type brokenStreamExportService struct{ fakeExportImageService }

func (s *brokenStreamExportService) StreamImage(_ context.Context, _ string, w io.Writer, _ bool) error {
	_, _ = io.WriteString(w, "partial-zfs-stream")
	return errors.New("zfs send: I/O error")
}

// 响应头发出后 zfs send 中途失败，连接必须异常断开；正常结束的话浏览器会把残缺文件标成下载完成。
func TestExportDownloadAbortsConnectionWhenStreamFails(t *testing.T) {
	service := &brokenStreamExportService{fakeExportImageService{name: "win11.zfs"}}
	router := newRouter(Services{Images: service})
	srv := httptest.NewServer(router)
	defer srv.Close()

	rec := do(router, http.MethodPost, "/api/images/win11/export-ticket", "")
	var ticket struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil || ticket.URL == "" {
		t.Fatalf("ticket status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp, err := srv.Client().Get(srv.URL + ticket.URL)
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("流中途失败，客户端却读到了正常结束的响应")
	}
}
