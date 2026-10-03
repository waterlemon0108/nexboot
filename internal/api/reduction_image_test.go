package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/control/assets"
)

type fakeRestorePointImages struct {
	fakeExportImageService
	reductionStreamed string
	savedFrom         string
	savedReq          assets.SaveAsImageRequest
}

func (s *fakeRestorePointImages) ReductionExportName(_ context.Context, id string, compress bool) (string, error) {
	return "vmdk-还原点1.zfs", nil
}

func (s *fakeRestorePointImages) StreamReduction(_ context.Context, id string, w io.Writer, compress bool) error {
	s.reductionStreamed = id
	_, err := io.WriteString(w, "restore-point-stream")
	return err
}

func (s *fakeRestorePointImages) SaveReductionAsImage(_ context.Context, id string, req assets.SaveAsImageRequest) (assets.ImportImageResult, error) {
	s.savedFrom, s.savedReq = id, req
	return assets.ImportImageResult{TaskID: "task-copy"}, nil
}

type fakeOverwriteConfigs struct {
	fakeConfigService
	overwritten string
}

func (s *fakeOverwriteConfigs) OverwriteImage(_ context.Context, id string) (assets.ConfigTaskResult, error) {
	s.overwritten = id
	return assets.ConfigTaskResult{TaskID: "task-merge"}, nil
}

func TestRestorePointDownloadTicketFlow(t *testing.T) {
	images := &fakeRestorePointImages{}
	router := newRouter(Services{Images: images})
	rec := do(router, http.MethodPost, "/api/reductions/vmdk_default_r1/export-ticket", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket status = %d body %s", rec.Code, rec.Body.String())
	}
	var ticket struct {
		URL      string `json:"url"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil || ticket.FileName != "vmdk-还原点1.zfs" {
		t.Fatalf("ticket = %#v err=%v", ticket, err)
	}
	rec = do(router, http.MethodGet, ticket.URL, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "restore-point-stream" || images.reductionStreamed != "vmdk_default_r1" {
		t.Fatalf("download status=%d body=%q streamed=%q", rec.Code, rec.Body.String(), images.reductionStreamed)
	}
	if images.streamedID != "" {
		t.Fatal("a restore point ticket must not stream the image")
	}
}

func TestSaveRestorePointAsImage(t *testing.T) {
	images := &fakeRestorePointImages{}
	router := newRouter(Services{Images: images})
	rec := do(router, http.MethodPost, "/api/reductions/vmdk_default_r1/save-as-image", `{"name":"办公版"}`)
	if rec.Code != http.StatusAccepted || images.savedFrom != "vmdk_default_r1" || images.savedReq.Name != "办公版" || !strings.Contains(rec.Body.String(), "task-copy") {
		t.Fatalf("status=%d from=%q req=%#v body=%s", rec.Code, images.savedFrom, images.savedReq, rec.Body.String())
	}
}

func TestOverwriteImageFromRestorePoint(t *testing.T) {
	configs := &fakeOverwriteConfigs{}
	router := newRouter(Services{Configs: configs})
	rec := do(router, http.MethodPost, "/api/reductions/vmdk_default_r1/overwrite-image", "")
	if rec.Code != http.StatusAccepted || configs.overwritten != "vmdk_default_r1" {
		t.Fatalf("status=%d overwritten=%q body=%s", rec.Code, configs.overwritten, rec.Body.String())
	}
}
