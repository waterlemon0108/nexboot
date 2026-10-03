package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// fakeHost 记录收到的请求并返回预设结果；契约测试让每个 RPC 走一遍 Handler+Client，
// 断言参数、结果和错误类型都原样传递。
type fakeHost struct {
	gotClient    storage.ClientReq
	gotCleanup   []string
	gotReclaim   string
	gotDecidedAt time.Time
	gotStop      storage.SuperStopReq
	failStop     error
}

func (f *fakeHost) CreateClientLUN(_ context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	f.gotClient = req
	return storage.LUNInfo{Server: "10.0.0.3", Target: "iqn.t", System: storage.LUN{LUN: 0, Target: "iqn.t", VolPath: "/dev/zvol/x"}}, nil
}
func (f *fakeHost) CleanupClientClones(_ context.Context, pool string, macs []string) error {
	f.gotCleanup = append([]string{pool}, macs...)
	return nil
}
func (f *fakeHost) ReclaimIdleClientClones(_ context.Context, mac string, decidedAt time.Time) (bool, error) {
	f.gotReclaim, f.gotDecidedAt = mac, decidedAt
	return true, nil
}
func (f *fakeHost) ActiveClientMACs(context.Context) ([]string, error) {
	return []string{"aa:bb:cc:dd:ee:01"}, nil
}
func (f *fakeHost) ClientCloneMACs(context.Context) ([]string, error) {
	return []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, nil
}
func (f *fakeHost) SuperStart(_ context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	f.gotClient = req
	return storage.LUNInfo{Server: "10.0.0.3", Target: "iqn.s"}, nil
}
func (f *fakeHost) SuperStop(_ context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	f.gotStop = req
	if f.failStop != nil {
		return storage.SuperStopResult{}, f.failStop
	}
	return storage.SuperStopResult{System: domain.Reduction{ID: "red-1", Name: "r"}}, nil
}
func (f *fakeHost) PrepareSuperAdaptation(_ context.Context, req storage.SuperAdaptationReq) error {
	if req.MAC == "" || len(req.BundleZip) == 0 {
		return errors.New("lost fields")
	}
	return nil
}
func (f *fakeHost) ReadSuperAdaptationResult(_ context.Context, mac string) (storage.SuperAdaptationResult, error) {
	return storage.SuperAdaptationResult{Done: true}, nil
}
func (f *fakeHost) SpaceUsage(context.Context) (storage.SpaceUsage, error) {
	return storage.SpaceUsage{}, nil
}

func newPair(t *testing.T) (*fakeHost, Client) {
	t.Helper()
	host := &fakeHost{}
	srv := httptest.NewServer(Handler{Agent: host, Token: "tok"})
	t.Cleanup(srv.Close)
	return host, Client{BaseURL: srv.URL, Token: "tok"}
}

func TestRoundTrip(t *testing.T) {
	host, client := newPair(t)
	ctx := context.Background()

	req := storage.ClientReq{MAC: "aa:bb", IP: "1.2.3.4", GroupID: "g1",
		System:    storage.ClientSource{ImageID: "img", ConfigID: "cfg", SnapshotName: "final"},
		DataDisks: []storage.ClientSource{{ImageID: "d1", ConfigID: "d1c"}}, MountScript: []byte("ps1")}
	info, err := client.CreateClientLUN(ctx, req)
	if err != nil || info.Server != "10.0.0.3" || info.System.VolPath != "/dev/zvol/x" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if !reflect.DeepEqual(host.gotClient, req) {
		t.Fatalf("req mangled: %+v", host.gotClient)
	}

	if err := client.CleanupClientClones(ctx, "tank", []string{"m1", "m2"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host.gotCleanup, []string{"tank", "m1", "m2"}) {
		t.Fatalf("cleanup args: %v", host.gotCleanup)
	}

	// 传的是「决定过去多久」，对端按自己的时钟换算：两台机器的时钟不必一致。
	decided := time.Now().Add(-time.Minute)
	if done, err := client.ReclaimIdleClientClones(ctx, "m1", decided); err != nil || !done {
		t.Fatalf("reclaim done=%v err=%v", done, err)
	}
	if host.gotReclaim != "m1" || host.gotDecidedAt.Sub(decided).Abs() > 5*time.Second {
		t.Fatalf("reclaim args: %q %v, want m1 %v", host.gotReclaim, host.gotDecidedAt, decided)
	}

	macs, err := client.ActiveClientMACs(ctx)
	if err != nil || len(macs) != 1 {
		t.Fatalf("macs=%v err=%v", macs, err)
	}
	clones, err := client.ClientCloneMACs(ctx)
	if err != nil || len(clones) != 2 {
		t.Fatalf("clones=%v err=%v", clones, err)
	}

	if err := client.PrepareSuperAdaptation(ctx, storage.SuperAdaptationReq{MAC: "aa:bb", BundleZip: []byte{1}, AdaptScript: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	res, err := client.ReadSuperAdaptationResult(ctx, "aa:bb")
	if err != nil || !res.Done {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// 调用方据以分支的类型化错误必须跨网络原样还原。
func TestSuperSessionMissingSurvivesWire(t *testing.T) {
	host, client := newPair(t)
	host.failStop = storage.SuperSessionMissing{LUN: 2}
	_, err := client.SuperStop(context.Background(), storage.SuperStopReq{MAC: "aa:bb", ConfigID: "c"})
	var missing storage.SuperSessionMissing
	if !errors.As(err, &missing) || missing.LUN != 2 {
		t.Fatalf("err=%v", err)
	}
	if host.gotStop.ConfigID != "c" {
		t.Fatalf("stop req mangled: %+v", host.gotStop)
	}
}

func TestTokenRequired(t *testing.T) {
	_, client := newPair(t)
	client.Token = "wrong"
	if _, err := client.ActiveClientMACs(context.Background()); err == nil {
		t.Fatal("want auth error")
	}
}

// 编译期检查：本地代理和远程客户端都实现放置接缝。
var _ storage.ClientHostAgent = Client{}
