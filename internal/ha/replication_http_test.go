package ha

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

type fakeReplicationSource struct {
	inv     zfs.GUIDInventory
	err     error
	streams []string // 每次调用记录 "<from>-><to>"
}

func (f *fakeReplicationSource) ListGUIDs(_ context.Context, root string) (zfs.GUIDInventory, error) {
	return f.inv, f.err
}

func (f *fakeReplicationSource) SendReplication(_ context.Context, root, from, to string, w io.Writer) error {
	f.streams = append(f.streams, from+"->"+to)
	_, err := io.WriteString(w, "STREAM:"+from+"->"+to)
	return err
}

func (f *fakeReplicationSource) SendDataset(_ context.Context, dataset, from, origin, to string, w io.Writer) error {
	f.streams = append(f.streams, fmt.Sprintf("dataset:%s from:%s origin:%s to:%s", dataset, from, origin, to))
	_, err := io.WriteString(w, "DATASET:"+dataset)
	return err
}

func (f *fakeReplicationSource) SendResume(_ context.Context, token string, w io.Writer) error {
	f.streams = append(f.streams, "resume:"+token)
	_, err := io.WriteString(w, "RESUME:"+token)
	return err
}

// 复制端点在节点间搬运目录，只对持有集群令牌的对端开放：流里是本机全部镜像。
func TestReplicationHandlerAuthAndRoundTrip(t *testing.T) {
	source := &fakeReplicationSource{inv: zfs.GUIDInventory{Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "11"},
	}}}
	var served, pullers []string
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "tank/nd", Token: "s3cret",
		OnServed: func(from, to string) { pullers = append(pullers, from); served = append(served, to) }})
	defer srv.Close()

	// 令牌错误：读取任何东西前就拒绝
	bad := Peer{BaseURL: srv.URL, Token: "wrong"}
	if _, err := bad.Inventory(context.Background()); err == nil {
		t.Fatal("wrong token accepted")
	}

	// Self 是本备机的身份，主机据此分别记录各备机进度；否则计划切换可能把 VIP 交给持有陈旧副本的那台。
	peer := Peer{BaseURL: srv.URL, Token: "s3cret", Self: "http://10.0.0.4:8080"}
	inv, err := peer.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Root != "tank/nd" || len(inv.Entries) != 1 || inv.Entries[0].GUID != "11" {
		t.Fatalf("inventory = %#v", inv)
	}

	// 先全量流（无基准），再增量
	rc, err := peer.Stream(context.Background(), "", "rep-1")
	if err != nil {
		t.Fatal(err)
	}
	full, _ := io.ReadAll(rc)
	rc.Close()
	if string(full) != "STREAM:->rep-1" {
		t.Fatalf("full stream = %q", full)
	}
	rc, err = peer.Stream(context.Background(), "rep-1", "rep-2")
	if err != nil {
		t.Fatal(err)
	}
	incr, _ := io.ReadAll(rc)
	rc.Close()
	if string(incr) != "STREAM:rep-1->rep-2" {
		t.Fatalf("incremental stream = %q", incr)
	}
	rc, err = peer.StreamResume(context.Background(), "1-tok")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := io.ReadAll(rc)
	rc.Close()
	if string(res) != "RESUME:1-tok" {
		t.Fatalf("resume stream = %q", res)
	}
	if strings.Join(source.streams, ",") != "->rep-1,rep-1->rep-2,resume:1-tok" {
		t.Fatalf("streams = %v", source.streams)
	}
	// 主机对备机的唯一了解就是它服务的请求。流报告所带快照，清单轮询报 ""，也算心跳，
	// 否则同步中的备机会被当成已死、滞后告警误报。
	if strings.Join(served, ",") != ",rep-1,rep-2" {
		t.Fatalf("served = %v", served)
	}
	for _, p := range pullers {
		if p != "http://10.0.0.4:8080" {
			t.Fatalf("pullers = %v, want the standby's own identity on every request", pullers)
		}
	}
	// 不报身份的客户端（旧版本）也要记录：用其地址，绝不记成空名。
	anon := Peer{BaseURL: srv.URL, Token: "s3cret"}
	pullers = nil
	if _, err := anon.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pullers) != 1 || pullers[0] == "" {
		t.Fatalf("anonymous puller recorded as %v, want its address", pullers)
	}
}

// HA 控制端点：status 供对端激活守卫读取，activate/standby 供 keepalived 通知脚本调用。全部需集群令牌。
func TestControlHandlerStatusAndSwitch(t *testing.T) {
	dir := t.TempDir()
	exits := 0
	ctl := &Controller{
		RoleFile:        dir + "/ha-role",
		Gate:            NewOpenGate(),
		Exit:            func(int) { exits++ },
		PrepareActiveDB: func(context.Context) error { return nil },
	}
	_ = WriteRoleState(ctl.RoleFile, RoleState{Role: "standby", Epoch: 2})
	srv := httptest.NewServer(ControlHandler{Controller: ctl, NodeID: "node-b", Token: "s3cret"})
	defer srv.Close()

	// 对端读取状态（激活守卫调用的）
	peer := Peer{BaseURL: srv.URL, Token: "s3cret"}
	st, err := peer.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.NodeID != "node-b" || st.Role != "standby" || st.Epoch != 2 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := (Peer{BaseURL: srv.URL, Token: "wrong"}).Status(context.Background()); err == nil {
		t.Fatal("wrong token accepted")
	}

	// 经通知激活
	req, _ := http.NewRequest("POST", srv.URL+"/internal/ha/activate", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || exits != 1 {
		t.Fatalf("activate: code=%d exits=%d", resp.StatusCode, exits)
	}
	rs, _ := ReadRoleState(ctl.RoleFile)
	if rs.Role != "active" || rs.Epoch != 3 {
		t.Fatalf("state = %+v", rs)
	}

	// 被拒的激活以 409 返回并带上原因
	_ = WriteRoleState(ctl.RoleFile, RoleState{Role: "standby", Epoch: 3})
	ctl.Peer = fakePeerStatus{status: PeerStatus{Role: "active", Epoch: 99}}
	resp2, err := http.DefaultClient.Do(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("refused activate code = %d", resp2.StatusCode)
	}
}

type observingSource struct {
	fakeReplicationSource
	during func()
}

func (o *observingSource) SendReplication(ctx context.Context, root, from, to string, w io.Writer) error {
	o.during()
	return o.fakeReplicationSource.SendReplication(ctx, root, from, to, w)
}

func (o *observingSource) SendDataset(ctx context.Context, dataset, from, origin, to string, w io.Writer) error {
	o.during()
	return o.fakeReplicationSource.SendDataset(ctx, dataset, from, origin, to, w)
}

func (o *observingSource) SendResume(ctx context.Context, token string, w io.Writer) error {
	o.during()
	return o.fakeReplicationSource.SendResume(ctx, token, w)
}

// 整体发送要几分钟，期间整棵目录被 hold 住什么都删不掉。写入者要知道哪台备机在拉整份，删配置时才能如实说明；增量只有几秒，不算。
func TestReplicationHandlerTracksWhoIsCopyingTheWholeCatalogue(t *testing.T) {
	sends := &FullSends{}
	var seen [][]string
	source := &observingSource{during: func() { seen = append(seen, sends.Active()) }}
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "tank/nd", Token: "t", FullSends: sends})
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "t", Self: "http://10.0.0.4:8080"}

	read := func(rc io.ReadCloser, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(rc)
		rc.Close()
	}
	read(peer.Stream(context.Background(), "", "rep-1"))
	read(peer.StreamResume(context.Background(), "tok"))
	read(peer.Stream(context.Background(), "rep-1", "rep-2"))

	want := []string{"[10.0.0.4]", "[10.0.0.4]", "[]"}
	for i, w := range want {
		if got := fmt.Sprint(seen[i]); got != w {
			t.Fatalf("第 %d 次发送期间 Active() = %s，应为 %s", i+1, got, w)
		}
	}
	if got := sends.Active(); len(got) != 0 {
		t.Fatalf("发送结束后仍登记着 %v", got)
	}
}

// 逐个数据集追平：备机传相对名，写入者按自己的目录根拼出完整名（各台池名不同）。名字要校验，不能借此读目录以外的数据集。
// 复制进度只在目录根发完时才记：根收到了这一轮才算完整，计划切换排空和标记保留都靠它。
func TestReplicationHandlerStreamsOneDataset(t *testing.T) {
	source := &fakeReplicationSource{}
	var served []string
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "data/nd", Token: "t",
		OnServed: func(from, to string) { served = append(served, to) }})
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "t", Self: "http://10.0.0.4:8080"}
	read := func(rc io.ReadCloser, err error) error {
		if err != nil {
			return err
		}
		_, _ = io.ReadAll(rc)
		return rc.Close()
	}
	if err := read(peer.StreamDataset(context.Background(), "/cfg", "1", "", "rep-2")); err != nil {
		t.Fatal(err)
	}
	if err := read(peer.StreamDataset(context.Background(), "/fork", "", "/img@0", "0")); err != nil {
		t.Fatal(err)
	}
	if len(served) != 0 {
		t.Fatalf("子数据集不该记复制进度: %v", served)
	}
	if err := read(peer.StreamDataset(context.Background(), "", "rep-1", "", "rep-2")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dataset:data/nd/cfg from:data/nd/cfg@1 origin: to:rep-2",
		"dataset:data/nd/fork from: origin:data/nd/img@0 to:0",
		"dataset:data/nd from:data/nd@rep-1 origin: to:rep-2",
	}
	if fmt.Sprint(source.streams) != fmt.Sprint(want) {
		t.Fatalf("streams =\n%v\nwant\n%v", source.streams, want)
	}
	if fmt.Sprint(served) != "[rep-2]" {
		t.Fatalf("根收完才记进度: %v", served)
	}
	for _, bad := range []string{"../etc", "/cfg/../../x", "cfg", "/a b"} {
		if err := read(peer.StreamDataset(context.Background(), bad, "", "", "rep-2")); err == nil {
			t.Fatalf("非法数据集名 %q 应被拒", bad)
		}
	}
	if err := read(peer.StreamDataset(context.Background(), "/cfg", "", "/../x@0", "rep-2")); err == nil {
		t.Fatal("非法克隆源应被拒")
	}
}

// 追平完成的备机报告「这一轮我收全了」：只认写入者目录根上确实存在的标记，其余一律当普通心跳，
// 报错的名字不能让写入者把不存在的标记当作确认点保住。
func TestConfirmRecordsOnlyTheWritersOwnMarkers(t *testing.T) {
	source := &fakeReplicationSource{inv: zfs.GUIDInventory{Entries: []zfs.GUIDEntry{
		{Name: "tank/nd@rep-2", GUID: "22"},
		{Name: "tank/nd/cfg@v1", GUID: "33"},
	}}}
	var served []string
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "tank/nd", Token: "s3cret",
		OnServed: func(_, to string) { served = append(served, to) }})
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "s3cret", Self: "http://10.0.0.4:8080"}
	for _, m := range []string{"rep-2", "rep-9", "v1", "rep-2&x=1"} {
		if err := peer.Confirm(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(served) != "[rep-2   ]" {
		t.Fatalf("served = %q, want only rep-2 recorded", served)
	}
}

// 发了一部分数据后连接失效（如 VIP 移走、对端不再应答）：接收方等不到下一块数据，读取要在限定时间内失败而不是永远挂着。
func TestStreamGivesUpWhenThePeerStopsSending(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall) // 先放行 handler，srv.Close 才等得到它返回
	peer := Peer{BaseURL: srv.URL, Token: "s3cret", StreamIdle: 80 * time.Millisecond}

	rc, err := peer.Stream(context.Background(), "rep-1", "rep-2")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(rc); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("对端不再发送时应报错")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("对端不再发送时读取卡住了")
	}
}

// 数据一直在来只是慢：不能当成失效。计时只算等数据的那一段。
func TestStreamKeepsGoingWhileDataArrives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "s3cret", StreamIdle: 150 * time.Millisecond}
	rc, err := peer.Stream(context.Background(), "rep-1", "rep-2")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var got []byte
	buf := make([]byte, 1)
	for {
		n, err := rc.Read(buf)
		got = append(got, buf[:n]...)
		time.Sleep(200 * time.Millisecond) // 下游写盘慢：这段不该计入
		if err != nil {
			if err != io.EOF {
				t.Fatalf("数据一直在来，不该失败: %v", err)
			}
			break
		}
	}
	if string(got) != "xxxxxx" {
		t.Fatalf("got %q", got)
	}
}

// 集群可以先建、池后建：主机还没池时，备机要看到该做什么，而不是 zfs 原文。
func TestInventoryExplainsAWriterWithoutAPool(t *testing.T) {
	source := &fakeReplicationSource{err: storage.CommandError{Name: "zfs",
		Output: "cannot open 'tank/nd': dataset does not exist", Err: fmt.Errorf("exit status 1")}}
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "tank/nd", Token: "t"})
	defer srv.Close()
	_, err := Peer{BaseURL: srv.URL, Token: "t"}.Inventory(context.Background())
	if err == nil || !strings.Contains(err.Error(), "存储池管理") || strings.Contains(err.Error(), "cannot open") {
		t.Fatalf("err = %v", err)
	}
}

// 追平时逐个数据集发送，大镜像要十几分钟，期间删这个镜像只会撞 busy。
func TestReplicationHandlerTracksWhichDatasetIsBeingSent(t *testing.T) {
	sends := &FullSends{}
	var img, other, whole []string
	source := &observingSource{during: func() {
		img, other, whole = sends.Sending("ubuntu-vmdk"), sends.Sending("win11"), sends.Active()
	}}
	srv := httptest.NewServer(ReplicationHandler{Source: source, Root: "tank/nd", Token: "t", FullSends: sends})
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "t", Self: "http://10.0.0.5:8080"}
	rc, err := peer.StreamDataset(context.Background(), "/ubuntu-vmdk", "", "", "rep-17")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(rc)
	rc.Close()
	if fmt.Sprint(img) != "[10.0.0.5]" || len(other) != 0 || len(whole) != 0 {
		t.Fatalf("发送期间 Sending(ubuntu-vmdk)=%v Sending(win11)=%v Active()=%v", img, other, whole)
	}
	if got := sends.Sending("ubuntu-vmdk"); len(got) != 0 {
		t.Fatalf("发送结束后仍登记着 %v", got)
	}
}

func TestFullSendsSendingCoversTheWholeCatalogue(t *testing.T) {
	sends := &FullSends{}
	done := sends.begin("http://10.0.0.4:8080")
	if got := sends.Sending("any"); fmt.Sprint(got) != "[10.0.0.4]" {
		t.Fatalf("整份发送期间 Sending = %v", got)
	}
	done()
}

// 确认请求的应答会被丢弃，不必为它计算完整性（那要拿目录锁、跑几次 zfs list）。
func TestInventoryConfirmSkipsTheCompletenessCheck(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(ReplicationHandler{Source: &fakeReplicationSource{}, Root: "tank/nd", Token: "t",
		Incomplete: func(context.Context) []string { calls++; return nil }})
	defer srv.Close()
	peer := Peer{BaseURL: srv.URL, Token: "t", Self: "http://10.0.0.4:8080"}
	if _, err := peer.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := peer.Confirm(context.Background(), "rep-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("completeness computed %d times, want 1 (inventory only)", calls)
	}
}
