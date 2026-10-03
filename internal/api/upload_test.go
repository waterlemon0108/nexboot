package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/assets"
)

type fakeUploads struct {
	begun  assets.BeginUploadRequest
	chunks []string
	offset int64
}

func (f *fakeUploads) Begin(_ context.Context, r assets.BeginUploadRequest) (assets.UploadStatus, error) {
	f.begun = r
	return assets.UploadStatus{UploadID: "nodeA-1", FileName: r.FileName, SizeBytes: r.SizeBytes, NodeID: "nodeA"}, nil
}
func (f *fakeUploads) Status(context.Context, string) (assets.UploadStatus, error) {
	return assets.UploadStatus{UploadID: "nodeA-1", Received: f.offset, SizeBytes: 20, NodeID: "nodeA"}, nil
}
func (f *fakeUploads) Append(_ context.Context, _ string, off int64, body io.Reader) (assets.UploadStatus, error) {
	b, _ := io.ReadAll(body)
	f.chunks = append(f.chunks, fmt.Sprintf("%d:%s", off, string(b)))
	f.offset = off + int64(len(b))
	return assets.UploadStatus{UploadID: "nodeA-1", Received: f.offset, SizeBytes: 20,
		Complete: f.offset >= 20, Path: "/imports/a.zfs", NodeID: "nodeA"}, nil
}
func (f *fakeUploads) Abort(context.Context, string) error { return nil }

// 续传全靠 Content-Range 里的起点。解析错了分片会写到错误偏移：长度对得上、内容是坏的，到导入体检时才暴露。
func TestUploadChunkRoutesTheOffsetFromContentRange(t *testing.T) {
	up := &fakeUploads{}
	r := newRouter(Services{Uploads: up})

	rec := do(r, http.MethodPost, "/api/images/uploads", `{"file_name":"a.zfs","size_bytes":20}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建会话应 201，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var st assets.UploadStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.UploadID == "" || st.NodeID != "nodeA" {
		t.Fatalf("要带出会话与所在节点：%+v", st)
	}

	patch := func(rangeHdr, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/images/uploads/nodeA-1", strings.NewReader(body))
		if rangeHdr != "" {
			req.Header.Set("Content-Range", rangeHdr)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	if code := patch("bytes 0-9/20", "0123456789").Code; code != http.StatusOK {
		t.Fatalf("第一段应 200，得到 %d", code)
	}
	if code := patch("bytes 10-19/20", "abcdefghij").Code; code != http.StatusOK {
		t.Fatalf("第二段应 200，得到 %d", code)
	}
	want := []string{"0:0123456789", "10:abcdefghij"}
	if len(up.chunks) != 2 || up.chunks[0] != want[0] || up.chunks[1] != want[1] {
		t.Fatalf("偏移没按 Content-Range 传下去：%v", up.chunks)
	}

	// 缺少 Content-Range 时不能默认从 0 写，那会把续传变成覆盖。
	if code := patch("", "xxxx").Code; code != http.StatusBadRequest {
		t.Fatalf("没有 Content-Range 应 400，得到 %d", code)
	}
	if code := patch("bytes abc/20", "xxxx").Code; code != http.StatusBadRequest {
		t.Fatalf("Content-Range 格式错应 400，得到 %d", code)
	}
}

// 浏览器重连后先问「你收到哪了」，这条查询就是续传的起点。
func TestUploadStatusAndAbort(t *testing.T) {
	up := &fakeUploads{offset: 7}
	r := newRouter(Services{Uploads: up})

	rec := do(r, http.MethodGet, "/api/images/uploads/nodeA-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询应 200，得到 %d", rec.Code)
	}
	var st assets.UploadStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Received != 7 {
		t.Fatalf("要带出已收字节数：%+v", st)
	}
	if code := do(r, http.MethodDelete, "/api/images/uploads/nodeA-1", "").Code; code != http.StatusNoContent {
		t.Fatalf("放弃上传应 204，得到 %d", code)
	}
}
