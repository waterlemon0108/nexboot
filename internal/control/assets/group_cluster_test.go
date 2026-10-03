package assets

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// 集群下只查本机网卡不够：覆盖其他节点地址（如 192.168.10.4）的区间必须被拒绝，
// 否则 dnsmasq 把它分给客户机，那台备机掉线而控制台看不出异常。
func TestGroupRangeMustNotCoverAnyClusterNode(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	seedNode(t, st, "node-a", "192.168.10.3", "192.168.10.250")
	seedNode(t, st, "node-b", "192.168.10.4", "192.168.10.4")
	seedNode(t, st, "node-c", "192.168.10.5", "192.168.10.5")

	svc := GroupService{Store: st,
		// 本机只知道自己那张网卡——别的节点的地址它一无所知
		LocalAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.10.3")}, nil },
	}

	// 区间 .4 - .8：故意避开本机 .3，只盖住别的节点——本机那条线守不住的地方
	_, err := svc.Create(ctx, GroupRequest{
		Name: "教学一班", StartIP: "192.168.10.4", ClientMax: 5,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
	})
	if err == nil {
		t.Fatal("区间盖住了别的节点，却被收下了")
	}
	if !strings.Contains(err.Error(), "192.168.10.4") {
		t.Fatalf("报错没说清撞的是哪台：%v", err)
	}
}

// VIP 是集群入口。不能依赖「VIP 挂在主机网卡上」来拦：故障切换空档里 VIP 不在任何网卡上。
// 要用名册记录的 portal_ip 判定。
func TestGroupRangeMustNotCoverTheVIPEvenWhenNoOneHoldsIt(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	seedNode(t, st, "node-a", "192.168.10.3", "192.168.10.250")

	svc := GroupService{Store: st,
		// 切换空档：本机网卡上没有 VIP
		LocalAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.10.3")}, nil },
	}
	_, err := svc.Create(ctx, GroupRequest{
		Name: "教学二班", StartIP: "192.168.10.245", ClientMax: 10,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
	})
	if err == nil {
		t.Fatal("区间盖住了 VIP，却被收下了")
	}
	if !strings.Contains(err.Error(), "192.168.10.250") {
		t.Fatalf("报错没说清撞的是哪个地址：%v", err)
	}
}

// 读不到本机网卡不该让整个检查失效：花名册还在，别的节点和 VIP 照样要挡。
func TestClusterAddressesStillCheckedWhenInterfacesAreUnreadable(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	seedNode(t, st, "node-a", "192.168.10.3", "192.168.10.250")

	svc := GroupService{Store: st,
		LocalAddrs: func() ([]netip.Addr, error) { return nil, context.DeadlineExceeded },
	}
	_, err := svc.Create(ctx, GroupRequest{
		Name: "教学三班", StartIP: "192.168.10.245", ClientMax: 10,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
	})
	if err == nil || !strings.Contains(err.Error(), "192.168.10.250") {
		t.Fatalf("网卡读不到时，花名册这条线也没守住：%v", err)
	}
}

func seedNode(t *testing.T, st store.Store, id, ip, portalIP string) {
	t.Helper()
	if err := st.Servers().Create(context.Background(), domain.Server{
		ID: id, Name: id, IP: ip, PortalIP: portalIP,
		Role: domain.ServerRoleAll, Status: domain.ServerStatusUp,
	}); err != nil {
		t.Fatal(err)
	}
}

func seedGroupSystem(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{
		ID: "img-1", Name: "img-1", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{
		ID: "cfg-1", ImageID: "img-1", Name: "default", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 分组必须绑定还原点，而还原点挂在配置的「应用点」上。
	if err := st.Reductions().Create(ctx, domain.Reduction{
		ID: "red-1", ConfigID: "cfg-1", Name: "@base", DisplayName: "base",
		CreatedAt: now, Status: domain.ReductionStatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	id := "red-1"
	cfg.DefaultReductionID = &id
	if err := st.Configs().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

// 在线机器的租约未到期，改区间当下看不出问题；续租时 dnsmasq 拒绝旧地址，Windows 换 IP 会断开 iSCSI 会话。
// 预览必须标出正在运行的机器，不能只写「下次开机生效」。
func TestNetworkPreviewFlagsMachinesThatAreRunning(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	seedNode(t, st, "node-a", "192.168.10.3", "192.168.10.250")

	svc := GroupService{Store: st,
		LocalAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.10.3")}, nil },
	}
	group, err := svc.Create(ctx, GroupRequest{
		Name: "教学一班", StartIP: "192.168.10.100", ClientMax: 10,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTerminalIn(t, st, group.ID, "AABBCCDDEE01", "192.168.10.100", domain.TerminalStateOnline)
	seedTerminalIn(t, st, group.ID, "AABBCCDDEE02", "192.168.10.101", domain.TerminalStateOffline)

	// 把窗口整体挪走，两台都要换地址
	preview, err := svc.PreviewNetwork(ctx, group.ID, GroupNetworkRequest{
		StartIP: "192.168.10.150", ClientMax: 10, Netmask: "255.255.255.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Terminals) != 2 {
		t.Fatalf("预览里有 %d 台，应当是 2 台", len(preview.Terminals))
	}
	if preview.OnlineCount != 1 {
		t.Fatalf("在线台数 = %d，应当是 1", preview.OnlineCount)
	}
	byMAC := map[string]GroupNetworkPreviewTerminal{}
	for _, t2 := range preview.Terminals {
		byMAC[t2.MAC] = t2
	}
	if !byMAC["AABBCCDDEE01"].Online {
		t.Fatal("在线的那台没被标出来")
	}
	if byMAC["AABBCCDDEE02"].Online {
		t.Fatal("关机的那台被标成在线了")
	}
}

func seedTerminalIn(t *testing.T, st store.Store, groupID, mac, ip string, state domain.TerminalState) {
	t.Helper()
	if err := st.Terminals().Create(context.Background(), domain.Terminal{
		ID: "term-" + mac, Name: mac, MAC: mac, IP: ip, GroupID: groupID,
		State: state,
	}); err != nil {
		t.Fatal(err)
	}
}

// 给运行中的单台机器改地址后果同改区间（续租换 IP 断 iSCSI），且看似小操作更需要拦。
// 在线由 iSCSI 会话判定，关机后放行。
func TestTerminalAddressIsFrozenWhileTheMachineIsRunning(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	seedNode(t, st, "node-a", "192.168.10.3", "192.168.10.250")
	gsvc := GroupService{Store: st,
		LocalAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.10.3")}, nil },
	}
	group, err := gsvc.Create(ctx, GroupRequest{
		Name: "教学一班", StartIP: "192.168.10.100", ClientMax: 10,
		Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTerminalIn(t, st, group.ID, "AABBCCDDEE01", "192.168.10.100", domain.TerminalStateOnline)
	seedTerminalIn(t, st, group.ID, "AABBCCDDEE02", "192.168.10.101", domain.TerminalStateOffline)

	svc := TerminalService{Store: st}

	// 在线的那台：改地址被拒
	_, err = svc.Update(ctx, "term-AABBCCDDEE01", TerminalRequest{
		Name: "01号机", MAC: "AABBCCDDEE01", IP: "192.168.10.105", GroupID: group.ID,
	})
	if err == nil {
		t.Fatal("正在运行的机器被改了地址")
	}
	if !strings.Contains(err.Error(), "运行") {
		t.Fatalf("报错没说清为什么：%v", err)
	}

	// 关机的那台：照常可以改
	if _, err := svc.Update(ctx, "term-AABBCCDDEE02", TerminalRequest{
		Name: "02号机", MAC: "AABBCCDDEE02", IP: "192.168.10.106", GroupID: group.ID,
	}); err != nil {
		t.Fatalf("关机的机器也被拦了：%v", err)
	}

	// 在线但只改名字：地址没动，不该拦
	if _, err := svc.Update(ctx, "term-AABBCCDDEE01", TerminalRequest{
		Name: "一号机", MAC: "AABBCCDDEE01", IP: "192.168.10.100", GroupID: group.ID,
	}); err != nil {
		t.Fatalf("只改名字也被拦了：%v", err)
	}
}

// 拒绝时要说清是谁的地址：本机、哪台节点，还是集群虚拟 IP。
func TestGroupRangeRefusalSaysWhoseAddressItIs(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupSystem(t, st)
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-a", Name: "node-a", IP: "192.168.10.3", PortalIP: "192.168.10.250",
		HAState: "active", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	seedNode(t, st, "node-b", "192.168.10.4", "192.168.10.4")
	seedNode(t, st, "node-c", "192.168.10.5", "192.168.10.9") // 非高可用：只是它的服务地址
	svc := GroupService{Store: st, LocalAddrs: func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.168.10.3"), netip.MustParseAddr("192.168.10.250")}, nil
	}}
	for start, want := range map[string]string{
		"192.168.10.3":   "本机地址 192.168.10.3",
		"192.168.10.4":   "节点 192.168.10.4",
		"192.168.10.250": "集群虚拟 IP 192.168.10.250",
		"192.168.10.9":   "服务地址 192.168.10.9",
	} {
		_, err := svc.Create(ctx, GroupRequest{Name: "g-" + start, StartIP: start, ClientMax: 1,
			Netmask: "255.255.255.0", SystemImageID: "img-1", SystemConfigID: "cfg-1"})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "（") {
			t.Errorf("%s: err = %v，想要含「%s」", start, err, want)
		}
	}
}

// 分组先建好、节点后加进来时，区间里可能已经有服务器地址；新客户机不能再分到它。
func TestAllocationSkipsAClusterAddressInsideAnOlderGroup(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedNode(t, st, "node-late", "192.168.1.10", "192.168.1.10")
	terminal, err := TerminalService{Store: st}.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.IP != "192.168.1.11" {
		t.Fatalf("allocated ip = %s，分到了节点的地址", terminal.IP)
	}
}

func TestAddressUsersNamesGroupsAndClients(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5) // 192.168.1.10 - .14
	if _, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.12", Name: "教师机", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(AddressUsers(ctx, st, "192.168.1.12"), "、")
	if !strings.Contains(got, "分组「"+group.Name+"」") || !strings.Contains(got, "客户机「教师机」") {
		t.Fatalf("users = %s", got)
	}
	if users := AddressUsers(ctx, st, "192.168.1.200"); len(users) != 0 {
		t.Fatalf("free address reported in use: %v", users)
	}
}
