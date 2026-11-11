package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

func TestFetchNodeVolumesCarriesTheTokenAndEscapesThePoolName(t *testing.T) {
	var auth, pool string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, pool = r.Header.Get("Authorization"), r.URL.Query().Get("pool")
		_ = json.NewEncoder(w).Encode(storage.PoolFacts{Pool: pool, Used: 42})
	}))
	defer server.Close()
	facts, err := FetchNodeVolumes(context.Background(), server.URL, "tok", "a&b")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || pool != "a&b" || facts.Used != 42 {
		t.Fatalf("auth=%q pool=%q facts=%+v", auth, pool, facts)
	}
}

func TestFetchNodeVolumesBringsBackTheNodesReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "这台服务器上没有存储池 gone", http.StatusInternalServerError)
	}))
	defer server.Close()
	_, err := FetchNodeVolumes(context.Background(), server.URL, "tok", "gone")
	if err == nil || !strings.Contains(err.Error(), "没有存储池 gone") {
		t.Fatalf("err = %v", err)
	}
}
