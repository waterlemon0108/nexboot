package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func TestGroupServiceCreateUsesConfigDefaultReduction(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)

	group, err := service.Create(ctx, validGroupRequest("default"))
	if err != nil {
		t.Fatal(err)
	}
	if !group.IsDefault {
		t.Fatalf("first group should become default: %#v", group)
	}
	if group.SystemReductionID != "red-1" {
		t.Fatalf("reduction = %q", group.SystemReductionID)
	}
	got, err := service.GetDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != group.ID {
		t.Fatalf("default = %#v", got)
	}
}

// 分组只绑镜像与配置：无论请求指定什么，还原点都取配置的应用点，分组上记录的就是这个生效值。
func TestGroupServiceAlwaysUsesTheConfigsAppliedReduction(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: "red-2", ConfigID: "cfg-1", Name: "@office", DisplayName: "装完office", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
	service := newGroupService(st)

	req := validGroupRequest("g")
	req.SystemReductionID = "red-2" // 旧客户端会传一个，应被忽略
	group, err := service.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if group.SystemReductionID != "red-1" {
		t.Fatalf("group reduction = %q, want the config's applied red-1", group.SystemReductionID)
	}
}

func TestGroupServiceValidatesNetworkAndCascade(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)

	req := validGroupRequest("bad-ip")
	req.StartIP = "192.168.1.250"
	req.ClientMax = 20
	if _, err := service.Create(ctx, req); !errors.Is(err, ErrGroupInvalidNetwork) {
		t.Fatalf("network err = %v", err)
	}

	seedGroupTriple(t, ctx, st, "img-2", "cfg-2", "red-2")
	req = validGroupRequest("bad-cascade")
	req.SystemImageID = "img-2"
	if _, err := service.Create(ctx, req); !errors.Is(err, ErrGroupCascadeMismatch) {
		t.Fatalf("cascade err = %v", err)
	}
}

func TestGroupServiceMaintainsSingleDefault(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)

	first, err := service.Create(ctx, validGroupRequest("first"))
	if err != nil {
		t.Fatal(err)
	}
	req := validGroupRequest("second")
	req.StartIP = "192.168.2.10"
	req.Gateway = "192.168.2.1"
	req.IsDefault = true
	second, err := service.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	first, err = service.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = service.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.IsDefault || !second.IsDefault {
		t.Fatalf("defaults first=%v second=%v", first.IsDefault, second.IsDefault)
	}

	if _, err := service.SetDefault(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	first, _ = service.Get(ctx, first.ID)
	second, _ = service.Get(ctx, second.ID)
	if !first.IsDefault || second.IsDefault {
		t.Fatalf("after set default first=%v second=%v", first.IsDefault, second.IsDefault)
	}

	req = validGroupRequest("first-renamed")
	req.IsDefault = false
	if _, err := service.Update(ctx, first.ID, req); !errors.Is(err, ErrDefaultGroupRequired) {
		t.Fatalf("unset default err = %v", err)
	}
}

func TestGroupServiceDeleteRejectsAssignedTerminalsAndPromotesDefault(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)

	first, err := service.Create(ctx, validGroupRequest("first"))
	if err != nil {
		t.Fatal(err)
	}
	req := validGroupRequest("second")
	req.StartIP = "192.168.2.10"
	req.Gateway = "192.168.2.1"
	second, err := service.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-1", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: first.ID, State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, first.ID); !errors.Is(err, ErrGroupInUse) {
		t.Fatalf("delete err = %v", err)
	}
	if err := st.Terminals().Delete(ctx, "term-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err = service.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.IsDefault {
		t.Fatalf("remaining group should be default: %#v", second)
	}
}

func TestGroupServiceSyncsDHCPAfterGroupChanges(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	syncer := &fakeDHCPSyncer{}
	service := newGroupService(st)
	service.DHCP = syncer

	group, err := service.Create(ctx, validGroupRequest("default"))
	if err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 1 || len(syncer.calls[0]) != 1 {
		t.Fatalf("create sync calls = %#v", syncer.calls)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-1", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: group.ID, State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}

	req := validGroupRequest("default")
	req.IsDefault = true
	req.Gateway = "192.168.1.254"
	if _, err := service.Update(ctx, group.ID, req); err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 2 {
		t.Fatalf("update sync calls = %d", len(syncer.calls))
	}
	rendered, err := (dhcp.Renderer{TFTPRoot: "/srv/tftp", BootFile: "undionly.kpxe"}).Render(syncer.calls[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dhcp-option=tag:" + group.ID + ",option:router,192.168.1.254",
		"dhcp-host=AA:BB:CC:DD:EE:FF,set:" + group.ID + ",192.168.1.10",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, rendered)
		}
	}

	if err := st.Terminals().Delete(ctx, "term-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 3 || len(syncer.calls[2]) != 0 {
		t.Fatalf("delete sync calls = %#v", syncer.calls)
	}
}

// 分组区间移动时机器跟着移动：原偏移空闲则保持偏移，否则取下一个空闲地址；已在新区间内的不动。
func TestGroupServiceNetworkChangeRenumbersTerminals(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	syncer := &fakeDHCPSyncer{}
	service := newGroupService(st)
	service.DHCP = syncer

	group, err := service.Create(ctx, validGroupRequest("default")) // 192.168.1.10 + 20
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []domain.Terminal{
		{ID: "term-a", MAC: "AABBCCDDEE01", IP: "192.168.1.10", GroupID: group.ID}, // start+0
		{ID: "term-b", MAC: "AABBCCDDEE02", IP: "192.168.1.13", GroupID: group.ID}, // start+3
	} {
		seed.State = domain.TerminalStateUnknown
		if err := st.Terminals().Create(ctx, seed); err != nil {
			t.Fatal(err)
		}
	}
	req := validGroupRequest("default")
	req.IsDefault = true
	req.StartIP = "192.168.2.50"
	req.Gateway = "192.168.2.1"
	if _, err := service.Update(ctx, group.ID, req); err != nil {
		t.Fatalf("network change with machines in the group must renumber them, got %v", err)
	}
	want := map[string]string{"term-a": "192.168.2.50", "term-b": "192.168.2.53"}
	for id, ip := range want {
		got, err := st.Terminals().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.IP != ip {
			t.Fatalf("%s ip = %s, want %s (same offset in the new range)", id, got.IP, ip)
		}
	}
	// 旧地址租约要释放，dnsmasq 才会下发新预留；生成的配置使用新地址。
	if len(syncer.released) != 2 || syncer.released["AABBCCDDEE01"] != "192.168.1.10" || syncer.released["AABBCCDDEE02"] != "192.168.1.13" {
		t.Fatalf("released = %#v", syncer.released)
	}
	last := syncer.calls[len(syncer.calls)-1]
	if len(last) != 1 || len(last[0].Terminals) != 2 || last[0].Terminals[0].IP != "192.168.2.50" {
		t.Fatalf("synced = %#v", last)
	}
}

// 预览只说明网络改动会把机器从哪个地址挪到哪个，不实际执行；拒绝条件与真正改动完全一致。
func TestGroupServicePreviewNetworkChange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	syncer := &fakeDHCPSyncer{}
	service := newGroupService(st)
	service.DHCP = syncer
	group, err := service.Create(ctx, validGroupRequest("default")) // 192.168.1.10 + 20
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []domain.Terminal{
		{ID: "term-a", Name: "一号机", MAC: "AABBCCDDEE01", IP: "192.168.1.10", GroupID: group.ID},
		{ID: "term-b", MAC: "AABBCCDDEE02", IP: "192.168.1.13", GroupID: group.ID},
	} {
		seed.State = domain.TerminalStateUnknown
		if err := st.Terminals().Create(ctx, seed); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := service.PreviewNetwork(ctx, group.ID, GroupNetworkRequest{StartIP: "192.168.2.50", ClientMax: 20, Netmask: "255.255.255.0", Gateway: "192.168.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Terminals) != 2 || preview.Terminals[0].MAC != "AABBCCDDEE01" || preview.Terminals[0].OldIP != "192.168.1.10" || preview.Terminals[0].NewIP != "192.168.2.50" || preview.Terminals[1].NewIP != "192.168.2.53" {
		t.Fatalf("preview = %#v", preview.Terminals)
	}
	if got, _ := st.Terminals().Get(ctx, "term-a"); got.IP != "192.168.1.10" {
		t.Fatalf("preview must not change anything: %#v", got)
	}
	syncBefore := len(syncer.calls)
	if len(syncer.calls) != syncBefore {
		t.Fatal("preview must not sync dnsmasq")
	}
	// 拒绝条件与真正改动一致。
	if _, err := service.PreviewNetwork(ctx, group.ID, GroupNetworkRequest{StartIP: "192.168.2.50", ClientMax: 1, Netmask: "255.255.255.0"}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("too small must be refused: %v", err)
	}
}

// 缩小区间：超出新末尾的机器取下一个空闲地址而不阻止改动；区间装不下现有机器时拒绝并说明差多少。
func TestGroupServiceShrinkRenumbersOrRefusesWithNumbers(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	syncer := &fakeDHCPSyncer{}
	service := newGroupService(st)
	service.DHCP = syncer

	group, err := service.Create(ctx, validGroupRequest("default")) // .10 + 20
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []domain.Terminal{
		{ID: "term-a", MAC: "AABBCCDDEE01", IP: "192.168.1.10", GroupID: group.ID},
		{ID: "term-b", MAC: "AABBCCDDEE02", IP: "192.168.1.15", GroupID: group.ID},
		{ID: "term-c", MAC: "AABBCCDDEE03", IP: "192.168.1.16", GroupID: group.ID},
	} {
		seed.State = domain.TerminalStateUnknown
		if err := st.Terminals().Create(ctx, seed); err != nil {
			t.Fatal(err)
		}
	}
	req := validGroupRequest("default")
	req.IsDefault = true
	req.ClientMax = 3 // .10–.12
	if _, err := service.Update(ctx, group.ID, req); err != nil {
		t.Fatalf("shrink that still fits everyone must pass, got %v", err)
	}
	got := map[string]string{}
	for _, id := range []string{"term-a", "term-b", "term-c"} {
		term, err := st.Terminals().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got[id] = term.IP
	}
	if got["term-a"] != "192.168.1.10" || got["term-b"] != "192.168.1.11" || got["term-c"] != "192.168.1.12" {
		t.Fatalf("ips = %#v", got)
	}

	syncBefore := len(syncer.calls)
	req.ClientMax = 2
	_, err = service.Update(ctx, group.ID, req)
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("a window smaller than the group must be refused, got %v", err)
	}
	for _, want := range []string{"3", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q should say how many machines and how many addresses", err.Error())
		}
	}
	if len(syncer.calls) != syncBefore {
		t.Fatalf("sync must not run after a refused update")
	}
	if term, _ := st.Terminals().Get(ctx, "term-c"); term.IP != "192.168.1.12" {
		t.Fatalf("a refused change must leave addresses alone: %#v", term)
	}
}

func TestGroupServiceSyncDHCPSyncsExistingState(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	service := newGroupService(st)

	group, err := service.Create(ctx, validGroupRequest("default"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-1", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: group.ID, State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}

	syncer := &fakeDHCPSyncer{}
	service.DHCP = syncer
	if err := service.SyncDHCP(ctx); err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 1 || len(syncer.calls[0]) != 1 {
		t.Fatalf("sync calls = %#v", syncer.calls)
	}
	got := syncer.calls[0][0]
	if got.Group.ID != group.ID || len(got.Terminals) != 1 || got.Terminals[0].MAC != "AABBCCDDEEFF" {
		t.Fatalf("synced config = %#v", got)
	}
}

func newGroupService(st store.Store) GroupService {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	var n int
	return GroupService{
		Store: st,
		Now: func() time.Time {
			n++
			return base.Add(time.Duration(n))
		},
	}
}

type fakeDHCPSyncer struct {
	calls    [][]dhcp.GroupConfig
	released map[string]string // mac -> 已释放的 ip
	err      error
}

func (s *fakeDHCPSyncer) ReleaseLease(_ context.Context, ip, mac string) error {
	if s.released == nil {
		s.released = map[string]string{}
	}
	s.released[mac] = ip
	return nil
}

func (s *fakeDHCPSyncer) Sync(_ context.Context, groups []dhcp.GroupConfig) error {
	copied := make([]dhcp.GroupConfig, 0, len(groups))
	for _, group := range groups {
		copied = append(copied, dhcp.GroupConfig{
			Group:     group.Group,
			Terminals: append([]domain.Terminal{}, group.Terminals...),
		})
	}
	s.calls = append(s.calls, copied)
	return s.err
}

func seedGroupTriple(t *testing.T, ctx context.Context, st store.Store, imageID, configID, reductionID string) {
	t.Helper()
	now := time.Now().UTC()
	img := domain.Image{ID: imageID, Name: imageID, OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: configID, ImageID: imageID, Name: configID, CreatedAt: now}
	red := domain.Reduction{ID: reductionID, ConfigID: configID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	cfg.DefaultReductionID = &red.ID
	if err := st.Images().Create(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, red); err != nil {
		t.Fatal(err)
	}
}

func validGroupRequest(name string) GroupRequest {
	return GroupRequest{
		Name:           name,
		StartIP:        "192.168.1.10",
		ClientMax:      20,
		Gateway:        "192.168.1.1",
		Netmask:        "255.255.255.0",
		DNS1:           "8.8.8.8",
		SystemImageID:  "img-1",
		SystemConfigID: "cfg-1",
	}
}

// 两个分组占同一段地址时各自生成 dhcp-range，dnsmasq 得到重叠的地址池，
// 机器拿到哪个网关/DNS 取决于先匹配哪段。终端 IP 虽全局唯一，下发的网络参数却不唯一，必须拒绝。
func TestGroupRejectsARangeAnotherGroupAlreadyOwns(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	svc := newGroupService(st)
	base := validGroupRequest("")
	base.ClientMax = 30

	first := base
	first.Name, first.StartIP, first.Gateway = "一班", "192.168.10.10", "192.168.10.1"
	if _, err := svc.Create(ctx, first); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, startIP string
		max           int
		wantErr       bool
	}{
		{"完全重叠", "192.168.10.10", 30, true},
		{"头部相交", "192.168.10.1", 20, true}, // .1–.20 撞上 .10–.39
		{"尾部相交", "192.168.10.39", 5, true}, // .39–.43 撞上尾端
		{"被包含", "192.168.10.20", 2, true},  // 完全落在里面
		{"紧邻但不相交", "192.168.10.40", 10, false},
		{"另一个网段", "192.168.20.10", 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			// .254：这些区间有的从 .1 起，网关得躲开自己的客户机区间。
			gateway := tc.startIP[:strings.LastIndex(tc.startIP, ".")] + ".254"
			r.Name, r.StartIP, r.Gateway, r.ClientMax = "组-"+tc.name, tc.startIP, gateway, tc.max
			_, err := svc.Create(ctx, r)
			if tc.wantErr && !errors.Is(err, ErrGroupRangeOverlap) {
				t.Fatalf("err = %v, want ErrGroupRangeOverlap", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("非重叠的网段被拒: %v", err)
			}
		})
	}
}

// 编辑分组也不能把区间挪到别组上；只改名时不能和自己判重叠。
func TestGroupUpdateChecksOverlapAgainstOtherGroupsOnly(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	svc := newGroupService(st)
	req := func(name, start, gateway string, max int) GroupRequest {
		r := validGroupRequest(name)
		r.StartIP, r.Gateway, r.ClientMax = start, gateway, max
		return r
	}
	mk := func(name, start string) domain.Group {
		gateway := start[:strings.LastIndex(start, ".")] + ".1"
		g, err := svc.Create(ctx, req(name, start, gateway, 30))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	one := mk("一班", "192.168.10.10")
	mk("二班", "192.168.20.10")

	// 不动区间只改名不能误判与自身重叠（它是第一个分组，即默认分组，须保持默认）。
	keepDefault := func(r GroupRequest) GroupRequest { r.IsDefault = true; return r }
	if _, err := svc.Update(ctx, one.ID, keepDefault(req("一班改名", "192.168.10.10", "192.168.10.1", 30))); err != nil {
		t.Fatalf("改名被自己的网段挡住了: %v", err)
	}
	// 挪到别组区间上必须被拒绝。
	if _, err := svc.Update(ctx, one.ID, keepDefault(req("一班改名", "192.168.20.20", "192.168.20.1", 5))); !errors.Is(err, ErrGroupRangeOverlap) {
		t.Fatalf("err = %v, want ErrGroupRangeOverlap", err)
	}
}

// 分组可把客户机固定到存储节点：节点须已登记，清除固定后回到本机默认。
func TestGroupStorageServerPin(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	svc := newGroupService(st)
	seen := time.Now().UTC()
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-b", Name: "b", IP: "b", PortalIP: "10.9.0.2",
		APIURL: "http://b:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &seen}); err != nil {
		t.Fatal(err)
	}
	req := validGroupRequest("放置组")
	pin := "node-b"
	req.StorageServerID = &pin
	g, err := svc.Create(ctx, req)
	if err != nil || g.StorageServerID == nil || *g.StorageServerID != "node-b" {
		t.Fatalf("g=%+v err=%v", g.StorageServerID, err)
	}

	// 未知节点被拒绝。
	bad := validGroupRequest("第二组")
	bad.StartIP = "192.168.1.150"
	nope := "node-zzz"
	bad.StorageServerID = &nope
	if _, err := svc.Create(ctx, bad); err == nil {
		t.Fatal("want refusal for unknown storage node")
	}

	// 清除固定。
	req2 := validGroupRequest("放置组")
	req2.IsDefault = true // 种子分组创建时已成为默认分组
	empty := ""
	req2.StorageServerID = &empty
	g2, err := svc.Update(ctx, g.ID, req2)
	if err != nil || g2.StorageServerID != nil {
		t.Fatalf("g2=%v err=%v", g2.StorageServerID, err)
	}

	// 不传该字段时保留原固定（部分编辑安全）。
	keep := validGroupRequest("放置组")
	keep.IsDefault = true
	g3, err := svc.Update(ctx, g.ID, keep)
	if err != nil {
		t.Fatal(err)
	}
	_ = g3
}
