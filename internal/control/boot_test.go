package control

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/boot"
	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/remote"
	"github.com/tianwei/diskless/internal/store"
)

func TestBootServiceBuildsScriptAndPreparesLUN(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sanhook --drive 0x80 iscsi:server-a:::0:target-a") {
		t.Fatalf("script = %s", script)
	}
	if got := strings.Join(agent.calls, ","); got != "create:AABBCCDDEEFF" {
		t.Fatalf("calls = %s", got)
	}
	// 没有数据盘的 Windows 克隆也要带开机脚本，用于恢复 C-1 清掉的网关/DNS。
	if !strings.Contains(string(agent.req.MountScript), "/boot/net-config") {
		t.Fatalf("startup script missing for Windows boot: %q", agent.req.MountScript)
	}
}

func TestBootServiceNetConfig(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	group, err := st.Groups().Get(ctx, "grp-1")
	if err != nil {
		t.Fatal(err)
	}
	group.DNS1 = "223.5.5.5"
	group.DNS2 = " "
	if err := st.Groups().Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	service := BootService{Store: st}

	cfg, err := service.NetConfig(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway != "192.168.1.1" || len(cfg.DNS) != 1 || cfg.DNS[0] != "223.5.5.5" {
		t.Fatalf("cfg = %#v", cfg)
	}

	if _, err := service.NetConfig(ctx, "11:22:33:44:55:66"); !errors.Is(err, ErrUnknownTerminal) {
		t.Fatalf("unknown mac err = %v", err)
	}
}

func TestBootServicePassesGroupDataDisksToStorage(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	seedBootDataDisk(t, ctx, st, "disk-1", "D")
	seedBootDataDisk(t, ctx, st, "disk-2", "E")
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(script, "sanhook --drive 0x80") != 1 {
		t.Fatalf("script = %s", script)
	}
	if len(agent.req.DataDisks) != 2 {
		t.Fatalf("req = %#v", agent.req)
	}
	want := []storage.ClientSource{
		{ImageID: "img-disk-1", ConfigID: "cfg-disk-1", SnapshotName: "0", MountTarget: "D", LUN: 1},
		{ImageID: "img-disk-2", ConfigID: "cfg-disk-2", SnapshotName: "0", MountTarget: "E", LUN: 2},
	}
	if agent.req.DataDisks[0] != want[0] || agent.req.DataDisks[1] != want[1] {
		t.Fatalf("data disks = %#v, want %#v", agent.req.DataDisks, want)
	}
	// 有数据盘时 Windows 克隆带盘符脚本。
	if !strings.Contains(string(agent.req.MountScript), "/boot/data-disks") {
		t.Fatalf("mount script not passed to storage: %q", agent.req.MountScript)
	}
}

func TestBootServiceDataDiskLettersMatchLUNOrder(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	seedBootDataDisk(t, ctx, st, "disk-1", "D:")
	seedBootDataDisk(t, ctx, st, "disk-2", "e")
	service := BootService{Store: st}

	letters, err := service.DataDiskLetters(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	// LUN 按数据盘列表顺序编号（index+1）；盘符规整为大写字母。
	want := []DataDiskLetter{{LUN: 1, Letter: "D"}, {LUN: 2, Letter: "E"}}
	if len(letters) != 2 || letters[0] != want[0] || letters[1] != want[1] {
		t.Fatalf("letters = %#v, want %#v", letters, want)
	}

	if _, err := service.DataDiskLetters(ctx, "11:22:33:44:55:66"); !errors.Is(err, ErrUnknownTerminal) {
		t.Fatalf("unknown mac err = %v", err)
	}
}

func TestBootServiceDataDisksKeepLinuxPaths(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	img, err := st.Images().Get(ctx, "img-1")
	if err != nil {
		t.Fatal(err)
	}
	img.OSType = domain.OSTypeLinux
	if err := st.Images().Update(ctx, img); err != nil {
		t.Fatal(err)
	}
	seedBootDataDisk(t, ctx, st, "disk-1", "/data/games")
	letters, err := BootService{Store: st}.DataDiskLetters(ctx, "AABBCCDDEEFF")
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 1 || letters[0] != (DataDiskLetter{LUN: 1, Letter: "/data/games"}) {
		t.Fatalf("letters = %#v", letters)
	}
}

// 客户机脚本设的盘符与开机导出的 LUN 必须指向同一块盘，否则 D: 会落到别的盘上。
func TestBootServiceLettersAndExportedLUNsDescribeTheSameDisks(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	for _, d := range []struct{ id, letter string }{{"disk-1", "D:"}, {"disk-2", "e"}, {"disk-3", "F"}} {
		seedBootDataDisk(t, ctx, st, d.id, d.letter)
	}
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	letters, err := service.DataDiskLetters(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 3 {
		t.Fatalf("letters = %#v", letters)
	}
	// 按盘符声明的 LUN 查盘而不是按位置，位置一致正是要验证的假设。
	for _, l := range letters {
		var source storage.ClientSource
		for _, d := range agent.req.DataDisks {
			if d.LUN == l.LUN {
				source = d
			}
		}
		if source.ConfigID == "" {
			t.Fatalf("no disk exported as LUN %d, but a letter %q was served for it", l.LUN, l.Letter)
		}
		if want := strings.ToUpper(strings.TrimSuffix(source.MountTarget, ":")); want != l.Letter {
			t.Fatalf("LUN %d is exported for %q but lettered %q", l.LUN, source.MountTarget, l.Letter)
		}
	}
}

func TestBootServiceSuperUsesLatestReductionAndPersistentBranch(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	latest := domain.Reduction{ID: "red-2", ConfigID: "cfg-1", Name: "@2", CreatedAt: time.Now().UTC().Add(time.Second), Status: domain.ReductionStatusReady}
	if err := st.Reductions().Create(ctx, latest); err != nil {
		t.Fatal(err)
	}
	terminal, err := st.Terminals().Get(ctx, "term-1")
	if err != nil {
		t.Fatal(err)
	}
	terminal.IsSuper = true
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(agent.calls, ","); got != "create:AABBCCDDEEFF" {
		t.Fatalf("calls = %s", got)
	}
	if !agent.req.Super || agent.req.System.SnapshotName != "2" {
		t.Fatalf("req = %#v", agent.req)
	}
}

// 普通机按开机时配置的当前应用点开机，不用分组缓存的 id，刚做的应用或回退立即生效。
func TestBootServiceBootsFromTheConfigsAppliedReduction(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	newer := domain.Reduction{ID: "red-2", ConfigID: "cfg-1", Name: "@2", CreatedAt: time.Now().UTC().Add(time.Second), Status: domain.ReductionStatusReady}
	if err := st.Reductions().Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultReductionID = &newer.ID // 已应用；分组仍缓存 red-1
	if err := st.Configs().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}
	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if agent.req.System.SnapshotName != "2" {
		t.Fatalf("booted from %q, want the config's applied @2", agent.req.System.SnapshotName)
	}
}

func TestBootServiceSuperRejectsStartupSingletonConflict(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	first, err := st.Terminals().Get(ctx, "term-1")
	if err != nil {
		t.Fatal(err)
	}
	first.IsSuper = true
	if err := st.Terminals().Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := domain.Terminal{ID: "term-2", MAC: "001122334466", IP: "192.168.1.11", GroupID: "grp-1", IsSuper: true, State: domain.TerminalStateUnknown}
	if err := st.Terminals().Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	service := BootService{Store: st, Storage: &fakeStorage{}, Builder: boot.Builder{}}

	_, err = service.BuildBootScript(ctx, "00:11:22:33:44:66")
	if !errors.Is(err, assets.ErrTerminalSuperConflict) || !strings.Contains(err.Error(), first.MAC) {
		t.Fatalf("err = %v", err)
	}
}

func TestBootServiceCollapsesConcurrentSameMACBoots(t *testing.T) {
	st := seedBootStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	agent := &fakeStorage{started: started, release: release}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	type result struct {
		script string
		err    error
	}
	results := make(chan result, 2)
	run := func() {
		script, err := service.BuildBootScript(context.Background(), "aa:bb:cc:dd:ee:ff")
		results <- result{script, err}
	}
	go run()
	<-started // 第一个请求已进入 CreateClientLUN
	go run()
	// 放行前留时间让重试并入进行中的执行。
	time.Sleep(100 * time.Millisecond)
	close(release)

	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatal(first.err, second.err)
	}
	if first.script != second.script {
		t.Fatalf("scripts differ:\n%s\n%s", first.script, second.script)
	}
	if got := len(agent.calls); got != 1 {
		t.Fatalf("storage runs = %d, want 1 (an iPXE retry must share the in-flight run)", got)
	}
}

func TestBootServiceDetachesFromCallerCancellation(t *testing.T) {
	service := BootService{Store: seedBootStore(t), Storage: &fakeStorage{}, Builder: boot.Builder{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // iPXE 放弃了请求，共享执行仍须跑完

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sanboot") {
		t.Fatalf("script = %s", script)
	}
}

func TestBootServiceUnknownMAC(t *testing.T) {
	service := BootService{Store: seedBootStore(t), Storage: &fakeStorage{}, Builder: boot.Builder{}}

	_, err := service.BuildBootScript(context.Background(), "missing")
	if !errors.Is(err, ErrUnknownTerminal) {
		t.Fatalf("err = %v", err)
	}
}

func seedBootStore(t *testing.T) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "file:"+filepath.Join(t.TempDir(), "boot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	img := domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	redID := "red-1"
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", DefaultReductionID: &redID, CreatedAt: now}
	red := domain.Reduction{ID: redID, ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	group := domain.Group{
		ID:                "grp-1",
		Name:              "default",
		StartIP:           "192.168.1.10",
		ClientMax:         10,
		Gateway:           "192.168.1.1",
		Netmask:           "255.255.255.0",
		SystemImageID:     img.ID,
		SystemConfigID:    cfg.ID,
		SystemReductionID: red.ID,
	}
	terminal := domain.Terminal{ID: "term-1", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: group.ID, State: domain.TerminalStateUnknown}
	for _, step := range []struct {
		name string
		err  error
	}{
		{"image", st.Images().Create(ctx, img)},
		{"config", st.Configs().Create(ctx, cfg)},
		{"reduction", st.Reductions().Create(ctx, red)},
		{"group", st.Groups().Create(ctx, group)},
		{"terminal", st.Terminals().Create(ctx, terminal)},
	} {
		if step.err != nil {
			t.Fatalf("%s: %v", step.name, step.err)
		}
	}
	return st
}

func seedBootDataDisk(t *testing.T, ctx context.Context, st *store.SQLStore, id, mountTarget string) {
	t.Helper()
	now := time.Now()
	imgID := "img-" + id
	cfgID := "cfg-" + id
	redID := "red-" + id
	if err := st.Images().Create(ctx, domain.Image{ID: imgID, Name: id, OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: cfgID, ImageID: imgID, Name: "default", DefaultReductionID: &redID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: redID, ConfigID: cfgID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: id, GroupID: "grp-1", MountTarget: mountTarget, ImageID: imgID, ConfigID: cfgID}); err != nil {
		t.Fatal(err)
	}
}

// fakeStorage 只实现 CreateClientLUN：开机路径不应调用其它存储方法，
// 调到就经内嵌的 nil 接口 panic。
type fakeStorage struct {
	storage.StorageAgent
	mu        sync.Mutex
	calls     []string
	req       storage.ClientReq
	started   chan struct{}
	release   chan struct{}
	createErr error
}

func (s *fakeStorage) CreateClientLUN(_ context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "create:"+req.MAC)
	s.req = req
	started, release := s.started, s.release
	s.started = nil
	s.mu.Unlock()
	if started != nil {
		close(started)
		<-release
	}
	if s.createErr != nil {
		return storage.LUNInfo{}, s.createErr
	}
	return storage.LUNInfo{
		Server: "server-a",
		Target: "target-a",
		System: storage.LUN{LUN: 0, Target: "target-a"},
	}, nil
}

func TestBootServiceSkipsInjectionForBakedImage(t *testing.T) {
	// 镜像已烘焙当前脚本时 MountScript 必须为空，省掉每台客户机一次挂载和分区扫描。
	ctx := context.Background()
	st := seedBootStore(t)
	img, err := st.Images().Get(ctx, "img-1")
	if err != nil {
		t.Fatal(err)
	}
	img.MountScriptVersion = adapt.MountScriptVersion("8080")
	if err := st.Images().Update(ctx, img); err != nil {
		t.Fatal(err)
	}
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}, APIPort: "8080"}

	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if len(agent.req.MountScript) != 0 {
		t.Fatalf("baked image still injected per client: %q", agent.req.MountScript)
	}
}

func TestBootServiceInjectsWhenBakedVersionIsStale(t *testing.T) {
	// 脚本或 API 端口变化后必须退回逐台注入，不能让客户机沿用镜像里的旧版本。
	ctx := context.Background()
	st := seedBootStore(t)
	img, err := st.Images().Get(ctx, "img-1")
	if err != nil {
		t.Fatal(err)
	}
	img.MountScriptVersion = adapt.MountScriptVersion("8080")
	if err := st.Images().Update(ctx, img); err != nil {
		t.Fatal(err)
	}
	agent := &fakeStorage{}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}, APIPort: "18080"}

	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent.req.MountScript), "18080") {
		t.Fatalf("stale bake was trusted; script = %q", agent.req.MountScript)
	}
}

// sanhook 地址由控制面决定：配置了 PortalAddr 就覆盖，未配置时保留 agent 的地址（单机行为）。
func TestBootServicePortalAddrOverridesSanhookAddress(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	service := BootService{Store: st, Storage: &fakeStorage{}, Builder: boot.Builder{}, PortalAddr: "192.168.50.250"}

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sanhook --drive 0x80 iscsi:192.168.50.250:::0:target-a") {
		t.Fatalf("script = %s", script)
	}
	if strings.Contains(script, "iscsi:server-a") {
		t.Fatalf("agent identity leaked into the script: %s", script)
	}
}

// 分组指定在线存储节点时，克隆在该节点上做、sanhook 写该节点地址，客户机记下落点；
// 未指定的分组走本机路径。
func TestBootServicePlacesOnPinnedNode(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	nodeB := &fakeStorage{}
	srv := httptest.NewServer(remote.Handler{Agent: nodeB, Token: "tok"})
	defer srv.Close()
	seen := time.Now().UTC()
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-b", Name: "b", IP: "10.9.0.2", PortalIP: "10.9.0.2",
		APIURL: srv.URL, Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &seen}); err != nil {
		t.Fatal(err)
	}
	g, err := st.Groups().Get(ctx, "grp-1")
	if err != nil {
		t.Fatal(err)
	}
	pin := "node-b"
	g.StorageServerID = &pin
	if err := st.Groups().Update(ctx, g); err != nil {
		t.Fatal(err)
	}

	local := &fakeStorage{}
	router := &place.Router{Store: st, NodeID: "node-a", Local: local, LocalPortal: "192.168.50.250", Token: "tok"}
	service := BootService{Store: st, Storage: local, Builder: boot.Builder{}, PortalAddr: "192.168.50.250", Place: router}

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if len(local.calls) != 0 || len(nodeB.calls) != 1 {
		t.Fatalf("local=%v remote=%v", local.calls, nodeB.calls)
	}
	if !strings.Contains(script, "iscsi:10.9.0.2") || strings.Contains(script, "192.168.50.250:::") {
		t.Fatalf("script = %s", script)
	}
	term, err := st.Terminals().GetByMAC(ctx, storage.NormalizeMAC("aa:bb:cc:dd:ee:ff"))
	if err != nil || term.StorageServerID == nil || *term.StorageServerID != "node-b" {
		t.Fatalf("terminal placement record: %+v err=%v", term.StorageServerID, err)
	}
}

func TestBootServiceUnpinnedStaysLocalAndRecordsSelf(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	local := &fakeStorage{}
	router := &place.Router{Store: st, NodeID: "node-a", Local: local, LocalPortal: "192.168.50.250", Token: "tok"}
	service := BootService{Store: st, Storage: local, Builder: boot.Builder{}, PortalAddr: "192.168.50.250", Place: router}

	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatal(err)
	}
	if len(local.calls) != 1 || !strings.Contains(script, "iscsi:192.168.50.250") {
		t.Fatalf("calls=%v script=%s", local.calls, script)
	}
	term, _ := st.Terminals().GetByMAC(ctx, storage.NormalizeMAC("aa:bb:cc:dd:ee:ff"))
	if term.StorageServerID == nil || *term.StorageServerID != "node-a" {
		t.Fatalf("placement record: %v", term.StorageServerID)
	}
}

// 还原点记录在、池上快照没了时，报错要点名还原点，不能直接抛 zfs 原文。
func TestBootExplainsAMissingReductionSnapshot(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	agent := &fakeStorage{createErr: fmt.Errorf(
		"zfs [clone tank/nd/win_default@gone tank/run/CLIENT-X] failed: exit status 1: "+
			"cannot open %s: dataset does not exist", "'tank/nd/win_default@gone'")}
	service := BootService{Store: st, Storage: agent, Builder: boot.Builder{}}

	_, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("快照缺失时必须报错")
	}
	msg := err.Error()
	for _, want := range []string{"还原点", "快照"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("报错要说清是还原点的快照丢了，实际：%v", err)
		}
	}
	if !strings.Contains(msg, "@gone") && !strings.Contains(msg, "gone") {
		t.Fatalf("要点名是哪个还原点：%v", err)
	}
}

// 落点节点还没复制到快照时，开机应回落到写入者，而不是失败等下一轮复制。
func TestBootFallsBackToTheWriterWhenTheRemoteLacksTheSnapshot(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	remote := &fakeStorage{createErr: fmt.Errorf(
		"zfs [clone tank/nd/win_default@0 tank/run/CLIENT-X] failed: exit status 1: "+
			"cannot open %s: dataset does not exist", "'tank/nd/win_default@0'")}
	local := &fakeStorage{}
	service := BootService{
		Store: st, Storage: local, Builder: boot.Builder{},
		Place: &place.Router{
			Store: st, NodeID: "node-local", Local: remote, LocalPortal: "10.9.0.1",
			Now: time.Now,
		},
	}
	// 放置落到注入的失败代理，失败后应改用 s.Storage
	script, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("远端没有快照时应当回落到写入者，而不是让客户机开不了机：%v", err)
	}
	if !strings.Contains(script, "sanhook") {
		t.Fatalf("回落之后要给出可用的启动脚本：%s", script)
	}
	if len(local.calls) == 0 {
		t.Fatal("没有回落到写入者的存储代理")
	}
}

// 落点节点缺快照、改由本机开机时，记录要改回本机；否则删除或移动会去远端清理，本机会话和克隆残留。
func TestBootFallbackRecordsTheLocalNodeAsPlacement(t *testing.T) {
	ctx := context.Background()
	st := seedBootStore(t)
	nodeB := &fakeStorage{createErr: fmt.Errorf(
		"zfs [clone tank/nd/win_default@0 tank/run/CLIENT-X] failed: exit status 1: "+
			"cannot open %s: dataset does not exist", "'tank/nd/win_default@0'")}
	srv := httptest.NewServer(remote.Handler{Agent: nodeB, Token: "tok"})
	defer srv.Close()
	seen := time.Now().UTC()
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-b", Name: "b", IP: "10.9.0.2", PortalIP: "10.9.0.2",
		APIURL: srv.URL, Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &seen}); err != nil {
		t.Fatal(err)
	}
	g, err := st.Groups().Get(ctx, "grp-1")
	if err != nil {
		t.Fatal(err)
	}
	pin := "node-b"
	g.StorageServerID = &pin
	if err := st.Groups().Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	local := &fakeStorage{}
	router := &place.Router{Store: st, NodeID: "node-a", Local: local, LocalPortal: "192.168.50.250", Token: "tok"}
	service := BootService{Store: st, Storage: local, Builder: boot.Builder{}, PortalAddr: "192.168.50.250", Place: router}

	if _, err := service.BuildBootScript(ctx, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if len(local.calls) != 1 {
		t.Fatalf("应回落本机开机：local=%v", local.calls)
	}
	term, err := st.Terminals().GetByMAC(ctx, storage.NormalizeMAC("aa:bb:cc:dd:ee:ff"))
	if err != nil || term.StorageServerID == nil || *term.StorageServerID != "node-a" {
		t.Fatalf("落点应记为实际提供盘的本机：%v err=%v", term.StorageServerID, err)
	}
}
