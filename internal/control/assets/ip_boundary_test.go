package assets

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/dhcp"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
	"github.com/tianwei/diskless/internal/xlsx"
)

// IP 是一台机器只能占一个的资源，分配错误几乎都出在端点上：区间头尾、区间之间的缝、
// 掩码切出的网络与广播地址、区间缩小后已发出的地址。现象离原因很远，只能逐个钉住端点。
// 分组之间的重叠见 group_test.go。

// —— 一、单个区间的头尾 ——

func TestTerminalIPOnTheRangeEndpoints(t *testing.T) {
	// seedTerminalGroup 的区间从 192.168.1.10 起，这里取 3 个：.10 .11 .12
	for _, tc := range []struct {
		name, ip string
		want     error
	}{
		{"首址可用", "192.168.1.10", nil},
		{"末址可用", "192.168.1.12", nil},
		{"首址前一位被拒", "192.168.1.9", ErrTerminalInvalidIP},
		{"末址后一位被拒", "192.168.1.13", ErrTerminalInvalidIP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			group := seedTerminalGroup(t, ctx, st, 3)
			svc := TerminalService{Store: st}
			got, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: tc.ip, GroupID: group.ID})
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v，想要 %v", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.IP != tc.ip {
				t.Fatalf("IP = %s，想要 %s", got.IP, tc.ip)
			}
		})
	}
}

// 容量为 1 的分组：能装下一台，第二台无论自报地址还是自动分配都必须被挡住。
func TestSingleAddressGroupTakesExactlyOneTerminal(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 1)
	svc := TerminalService{Store: st}

	first, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:01", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if first.IP != "192.168.1.10" {
		t.Fatalf("IP = %s，想要区间里唯一那个地址 192.168.1.10", first.IP)
	}
	if _, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:02", GroupID: group.ID}); !errors.Is(err, ErrTerminalNoAvailableIP) {
		t.Fatalf("自动分配 err = %v，想要 ErrTerminalNoAvailableIP", err)
	}
	if _, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:03", IP: "192.168.1.11", GroupID: group.ID}); err == nil {
		t.Fatal("第二台自报了区间外的地址，却被接受了")
	}
}

// 手工静态地址和自动分配共用区间、互相可见：自动分配取最低空位，不顶到手工地址，也不因中间有洞而提前报满。
func TestAutoAllocationAlwaysTakesTheLowestFreeAddress(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 4) // .10 .11 .12 .13
	svc := TerminalService{Store: st}
	mk := func(mac, ip string) domain.Terminal {
		t.Helper()
		term, err := svc.Create(ctx, TerminalRequest{MAC: mac, IP: ip, GroupID: group.ID})
		if err != nil {
			t.Fatalf("建终端 %s(%q): %v", mac, ip, err)
		}
		return term
	}

	// 首址先被手工占掉，自动分配得从第二个地址开始。
	mk("00:11:22:33:44:01", "192.168.1.10")
	if got := mk("00:11:22:33:44:02", "").IP; got != "192.168.1.11" {
		t.Fatalf("IP = %s，想要 192.168.1.11", got)
	}
	// 手工在末址插一台，中间留出 .12 这个洞。
	mk("00:11:22:33:44:03", "192.168.1.13")
	if got := mk("00:11:22:33:44:04", "").IP; got != "192.168.1.12" {
		t.Fatalf("IP = %s，想要填上 .12 这个洞", got)
	}
	// 四个地址全满。
	if _, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:05", GroupID: group.ID}); !errors.Is(err, ErrTerminalNoAvailableIP) {
		t.Fatalf("err = %v，想要 ErrTerminalNoAvailableIP", err)
	}
}

// 换机器后旧地址必须立刻回到池子，否则换几轮就耗干区间，而界面仍显示有空位。
func TestADeletedTerminalsAddressGoesBackIntoThePool(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 2) // .10 .11
	svc := TerminalService{Store: st}

	first, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:01", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:02", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:03", GroupID: group.ID})
	if err != nil {
		t.Fatalf("换上的新机器分不到地址：%v", err)
	}
	if replacement.IP != first.IP {
		t.Fatalf("IP = %s，想要拿回被删终端的 %s", replacement.IP, first.IP)
	}
}

// —— 二、掩码切出来的两个不可用地址 ——

// 区间是 [StartIP, StartIP+ClientMax-1] 的纯算术，必须再按掩码排除网络地址和广播地址，
// 否则 /24 下 .250 起 6 个会把广播地址 .255 发给终端。判定按掩码而非末段是否为 255：/23 下 x.10.255 是合法主机地址。
func TestGroupRejectsARangeCoveringTheNetworkOrBroadcastAddress(t *testing.T) {
	for _, tc := range []struct {
		name, startIP, gateway, netmask string
		max                             int
		wantErr                         bool
	}{
		{"区间尾部盖住广播地址", "192.168.1.250", "192.168.1.1", "255.255.255.0", 6, true},
		{"区间头部盖住网络地址", "192.168.1.0", "192.168.1.254", "255.255.255.0", 10, true},
		{"停在广播地址前一位", "192.168.1.250", "192.168.1.1", "255.255.255.0", 5, false},
		{"从网络地址后一位起", "192.168.1.1", "192.168.1.254", "255.255.255.0", 10, false},
		{"/23 下 .10.255 是可用地址", "192.168.10.250", "192.168.10.1", "255.255.254.0", 10, false},
		{"/23 下盖住真正的广播地址", "192.168.11.250", "192.168.10.1", "255.255.254.0", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
			svc := newGroupService(st)
			req := validGroupRequest(tc.name)
			req.StartIP, req.Gateway, req.Netmask, req.ClientMax = tc.startIP, tc.gateway, tc.netmask, tc.max
			_, err := svc.Create(ctx, req)
			if tc.wantErr && !errors.Is(err, ErrGroupRangeReserved) {
				t.Fatalf("err = %v，想要 ErrGroupRangeReserved", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("合法网段被拒：%v", err)
			}
		})
	}
}

// 网关可与终端同网段，但不能落在终端区间里，否则迟早分给某台终端，整个分组失去出口；只能在建组时挡住。
func TestGroupRejectsAGatewayInsideItsOwnClientRange(t *testing.T) {
	for _, tc := range []struct {
		name, gateway string
		wantErr       bool
	}{
		{"网关落在区间中间", "192.168.1.20", true},
		{"网关正好是区间首址", "192.168.1.10", true},
		{"网关正好是区间末址", "192.168.1.39", true},
		{"网关在区间前一位", "192.168.1.9", false},
		{"网关在区间后一位", "192.168.1.40", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
			svc := newGroupService(st)
			req := validGroupRequest(tc.name)
			req.StartIP, req.ClientMax, req.Gateway = "192.168.1.10", 30, tc.gateway // .10–.39
			_, err := svc.Create(ctx, req)
			if tc.wantErr && !errors.Is(err, ErrGroupGatewayInRange) {
				t.Fatalf("err = %v，想要 ErrGroupGatewayInRange", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("区间外的网关被拒：%v", err)
			}
		})
	}
}

// 区间不能跨出掩码子网，网关也不能在子网外，防回归。
func TestGroupRejectsARangeOrGatewayOutsideTheSubnet(t *testing.T) {
	for _, tc := range []struct {
		name, startIP, gateway, netmask string
		max                             int
	}{
		{"区间跨到下一个 /24", "192.168.1.250", "192.168.1.1", "255.255.255.0", 20},
		{"网关在另一个网段", "192.168.1.10", "192.168.2.1", "255.255.255.0", 20},
		{"掩码不连续", "192.168.1.10", "192.168.1.1", "255.0.255.0", 20},
		{"容量为零", "192.168.1.10", "192.168.1.1", "255.255.255.0", 0},
		{"容量为负", "192.168.1.10", "192.168.1.1", "255.255.255.0", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
			svc := newGroupService(st)
			req := validGroupRequest(tc.name)
			req.StartIP, req.Gateway, req.Netmask, req.ClientMax = tc.startIP, tc.gateway, tc.netmask, tc.max
			if _, err := svc.Create(ctx, req); !errors.Is(err, ErrGroupInvalidNetwork) {
				t.Fatalf("err = %v，想要 ErrGroupInvalidNetwork", err)
			}
		})
	}
}

// —— 三、改区间与已经发出去的地址 ——

// 缩小容量或上移起始地址可能把已发地址甩到区间外：仍在区间内的不动，甩出去的落到空位，真装不下才拒。
// 边界要精确：刚好包住最大地址时谁也不挪。
func TestGroupResizeMovesTheAddressesItAlreadyIssued(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	groups := newGroupService(st)

	req := func(start string, max int) GroupRequest {
		r := validGroupRequest("一班")
		r.StartIP, r.Gateway, r.ClientMax, r.IsDefault = start, "192.168.1.1", max, true
		return r
	}
	group, err := groups.Create(ctx, req("192.168.1.10", 30)) // .10–.39
	if err != nil {
		t.Fatal(err)
	}
	// 一台在区间头、一台在中段，缩容边界按后者算。
	terminals := TerminalService{Store: st}
	for i, ip := range []string{"192.168.1.10", "192.168.1.25"} {
		mac := fmt.Sprintf("00:11:22:33:44:%02d", i+1)
		if _, err := terminals.Create(ctx, TerminalRequest{MAC: mac, IP: ip, GroupID: group.ID}); err != nil {
			t.Fatalf("建终端 %s: %v", ip, err)
		}
	}
	ipsNow := func() map[string]string {
		list, err := st.Terminals().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, term := range list {
			out[term.MAC] = term.IP
		}
		return out
	}

	for _, tc := range []struct {
		name, startIP string
		max           int
		want          map[string]string
	}{
		{"缩到刚好包住最大的那个地址，谁也不挪", "192.168.1.10", 16, map[string]string{"001122334401": "192.168.1.10", "001122334402": "192.168.1.25"}},   // .10–.25
		{"再缩一位，.25 落到最近的空位", "192.168.1.10", 15, map[string]string{"001122334401": "192.168.1.10", "001122334402": "192.168.1.11"}},     // .10–.24
		{"起始地址上移一位，.10 跟着挪到新起点", "192.168.1.11", 30, map[string]string{"001122334401": "192.168.1.12", "001122334402": "192.168.1.11"}}, // .11–.40：.11 已被上一步的机器占着，.10 取最近空位 .12
		{"扩大区间不动任何人", "192.168.1.11", 50, map[string]string{"001122334401": "192.168.1.12", "001122334402": "192.168.1.11"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := groups.Update(ctx, group.ID, req(tc.startIP, tc.max)); err != nil {
				t.Fatalf("改区间被拒：%v", err)
			}
			if got := ipsNow(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("地址 = %v，想要 %v", got, tc.want)
			}
		})
	}
	// 装不下才拒：两台机器、一个地址。
	if _, err := groups.Update(ctx, group.ID, req("192.168.1.11", 1)); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("err = %v，想要 ErrInvalid", err)
	}
}

// —— 四、跨组搬迁 ——

// 一批机器搬进已有机器的分组：不能撞上目标组已有地址，批次内也不能重复；事务内逐台分配，占用表须即时更新。
func TestMoveGivesEachRelocatedTerminalAFreeAddressInTheTarget(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.10.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.20.10", 30)
	svc := TerminalService{Store: st}

	// 目标组已占 .10 和 .11。
	for _, ip := range []string{"192.168.20.10", "192.168.20.11"} {
		if _, err := svc.Create(ctx, TerminalRequest{
			MAC: "00:11:22:33:55:" + ip[strings.LastIndex(ip, ".")+1:], IP: ip, GroupID: to.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for i := 1; i <= 3; i++ {
		term, err := svc.Create(ctx, TerminalRequest{
			MAC: fmt.Sprintf("00:11:22:33:44:%02d", i), GroupID: from.ID, State: domain.TerminalStateOffline,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, term.ID)
	}

	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: ids, GroupID: to.ID}); err != nil {
		t.Fatalf("move: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		term, err := st.Terminals().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureTerminalIPInGroup(term.IP, to); err != nil {
			t.Fatalf("终端 %s 拿到 %s，不在目标区间里：%v", id, term.IP, err)
		}
		if term.IP == "192.168.20.10" || term.IP == "192.168.20.11" {
			t.Fatalf("终端 %s 抢了目标组已有终端的地址 %s", id, term.IP)
		}
		if got[term.IP] {
			t.Fatalf("批次里两台终端都拿到了 %s", term.IP)
		}
		got[term.IP] = true
	}
}

// 目标组只剩一个空位却搬三台：整批拒绝、三台都留原地。半途搬迁会让界面显示已搬完而实际仍用旧网段。
func TestMoveIntoATooSmallTargetLeavesEveryTerminalInPlace(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.10.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.20.10", 1)
	svc := TerminalService{Store: st}

	var ids []string
	for i := 1; i <= 3; i++ {
		term, err := svc.Create(ctx, TerminalRequest{
			MAC: fmt.Sprintf("00:11:22:33:66:%02d", i), GroupID: from.ID, State: domain.TerminalStateOffline,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, term.ID)
	}
	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: ids, GroupID: to.ID}); err == nil {
		t.Fatal("目标组装不下却接受了搬迁")
	}
	for _, id := range ids {
		term, err := st.Terminals().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if term.GroupID != from.ID {
			t.Fatalf("终端 %s 被搬走了一半：分组 = %s", id, term.GroupID)
		}
		if err := ensureTerminalIPInGroup(term.IP, from); err != nil {
			t.Fatalf("终端 %s 的地址 %s 已经被改成目标组的了：%v", id, term.IP, err)
		}
	}
}

// —— 五、分组之间的地址争用 ——

// 区间不重叠不代表地址不冲突：一班的网关可能落在二班客户机区间里，两组各自看都合法，
// 而 dnsmasq 不报错，只在请求时静默拒发该地址。
func TestGroupRejectsAGatewayAnotherGroupWouldHandOut(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	svc := newGroupService(st)
	mk := func(name, start, gateway string, max int) GroupRequest {
		r := validGroupRequest(name)
		r.StartIP, r.Gateway, r.ClientMax = start, gateway, max
		return r
	}
	// 一班 .10–.39，网关 .1。
	if _, err := svc.Create(ctx, mk("一班", "192.168.1.10", "192.168.1.1", 30)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, start, gateway string
		max                  int
		wantErr              bool
	}{
		{"新分组的区间盖住了一班的网关", "192.168.1.1", "192.168.1.254", 9, true},
		{"新分组的网关落在一班的区间里", "192.168.1.40", "192.168.1.20", 10, true},
		{"两边都不相干", "192.168.1.40", "192.168.1.254", 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, mk("组-"+tc.name, tc.start, tc.gateway, tc.max))
			if tc.wantErr && !errors.Is(err, ErrGroupGatewayConflict) {
				t.Fatalf("err = %v，想要 ErrGroupGatewayConflict", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("互不相干的分组被拒：%v", err)
			}
		})
	}
}

// 服务器自身地址落进客户机区间（整个网段都写成客户机范围）时，dnsmasq 会拒发该地址
// （"in use by the server or relay"），轮到它的那台静默起不来。应在建组时就说清。
func TestGroupRejectsARangeCoveringTheServersOwnAddress(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		max         int
		addrs       []netip.Addr
		addrsErr    error
		wantErr     bool
	}{
		{"区间盖住本机地址", "192.168.1.1", 10, []netip.Addr{netip.MustParseAddr("192.168.1.5")}, nil, true},
		{"本机地址在区间外", "192.168.1.10", 10, []netip.Addr{netip.MustParseAddr("192.168.1.5")}, nil, false},
		{"本机有多个地址，命中其中一个", "192.168.1.10", 10,
			[]netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("192.168.1.15")}, nil, true},
		// 读不到网卡不等于冲突：因此拦下每次建组的风险比放行更大。
		{"读不到本机地址时放行", "192.168.1.10", 10, nil, errors.New("no interfaces"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
			svc := newGroupService(st)
			svc.LocalAddrs = func() ([]netip.Addr, error) { return tc.addrs, tc.addrsErr }
			req := validGroupRequest(tc.name)
			req.StartIP, req.ClientMax, req.Gateway = tc.start, tc.max, "192.168.1.254"
			_, err := svc.Create(ctx, req)
			if tc.wantErr && !errors.Is(err, ErrGroupRangeCoversServer) {
				t.Fatalf("err = %v，想要 ErrGroupRangeCoversServer", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不该被拒：%v", err)
			}
		})
	}
}

// —— 六、改分组与发地址同时发生 ——

// 缩容校验当前终端是否装得下，发地址走另一把锁；两者并发可能让终端落到区间外，
// 这条数据入库后每次 dnsmasq 同步都整份失败，重启直接起不来。
func TestShrinkingAGroupWhileTerminalsAreRegisteredKeepsEveryoneInRange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedGroupTriple(t, ctx, st, "img-1", "cfg-1", "red-1")
	groups := newGroupService(st)
	req := func(max int) GroupRequest {
		r := validGroupRequest("一班")
		r.StartIP, r.Gateway, r.ClientMax, r.IsDefault = "192.168.1.10", "192.168.1.1", max, true
		return r
	}
	group, err := groups.Create(ctx, req(20)) // .10–.29
	if err != nil {
		t.Fatal(err)
	}
	terminals := TerminalService{Store: st}
	for i := 0; i < 10; i++ { // 占满 .10–.19
		if _, err := terminals.Create(ctx, TerminalRequest{
			MAC: fmt.Sprintf("00:11:22:33:00:%02d", i), GroupID: group.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 卡住这个窗口，让第 11 台在缩容通过校验、尚未落库时挤进来拿到 .20，
	// 随后缩容把区间定为 .10–.19；两个动作各自合法，结果这台永远在区间外。
	done := make(chan struct{})
	var once sync.Once
	afterGroupFitCheck = func() {
		once.Do(func() {
			go func() {
				defer close(done)
				terminals.Create(ctx, TerminalRequest{MAC: "00:11:22:33:00:99", GroupID: group.ID}) //nolint:errcheck
			}()
			select {
			case <-done: // 挤进来了，只有两条路互不排斥时才可能
			case <-time.After(300 * time.Millisecond): // 被挡在锁外，正是要的结果
			}
		})
	}
	defer func() { afterGroupFitCheck = nil }()

	groups.Update(ctx, group.ID, req(10)) //nolint:errcheck // 装不下就该失败
	<-done

	// 唯一要成立的：库里每台终端都在自己分组当前区间内，渲染器按此判定，dnsmasq 才装得下。
	final, err := st.Groups().Get(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.Terminals().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range list {
		if err := ensureTerminalIPInGroup(term.IP, final); err != nil {
			t.Fatalf("终端 %s 的 %s 落在分组区间 %s+%d 外：%v",
				term.MAC, term.IP, final.StartIP, final.ClientMax, err)
		}
	}
}

// —— 七、批量导入 ——

// 导入一次写几十上百台，文件行之间也会冲突：同 IP、同 MAC、空 IP 自动分配填满区间。任一行有问题整份不写，避免半份导入。
func TestTerminalImportCatchesCollisionsInsideTheFile(t *testing.T) {
	header := terminalImportLegacyHeader
	for _, tc := range []struct {
		name string
		rows [][]string
	}{
		{"文件里两行同一个 IP", [][]string{
			{"00:11:22:33:44:01", "192.168.1.11", "", "", "unknown"},
			{"00:11:22:33:44:02", "192.168.1.11", "", "", "unknown"},
		}},
		{"文件里两行同一个 MAC", [][]string{
			{"00:11:22:33:44:01", "192.168.1.11", "", "", "unknown"},
			{"00:11:22:33:44:01", "192.168.1.12", "", "", "unknown"},
		}},
		{"同一个 MAC 换了写法", [][]string{
			{"00:11:22:33:44:01", "192.168.1.11", "", "", "unknown"},
			{"00-11-22-33-44-01", "192.168.1.12", "", "", "unknown"},
		}},
		{"行数超过分组容量", [][]string{
			{"00:11:22:33:44:01", "", "", "", "unknown"},
			{"00:11:22:33:44:02", "", "", "", "unknown"},
			{"00:11:22:33:44:03", "", "", "", "unknown"},
			{"00:11:22:33:44:04", "", "", "", "unknown"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newImageTestStore(t)
			group := seedTerminalGroup(t, ctx, st, 3) // .10 .11 .12
			svc := TerminalService{Store: st}
			rows := [][]string{header}
			for _, row := range tc.rows {
				row[2] = group.ID
				rows = append(rows, row)
			}
			data, err := xlsx.Write(rows)
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.Import(ctx, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) == 0 {
				t.Fatalf("互相冲突的行被全盘接受了：%#v", result)
			}
			list, err := svc.List(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if list.Total != 0 {
				t.Fatalf("有错的导入写进去了 %d 台", list.Total)
			}
		})
	}
}

// 反过来：无冲突的文件中空 IP 与显式 IP 混写也要全部写入，各拿各的地址。
func TestTerminalImportMixesBlankAndExplicitAddresses(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 4) // .10 .11 .12 .13
	svc := TerminalService{Store: st}
	data, err := xlsx.Write([][]string{
		terminalImportLegacyHeader,
		{"00:11:22:33:44:01", "192.168.1.12", group.ID, "", "unknown"},
		{"00:11:22:33:44:02", "", group.ID, "", "unknown"},
		{"00:11:22:33:44:03", "", group.ID, "", "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Import(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("没有冲突的文件被拒：%#v", result.Errors)
	}
	list, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 3 {
		t.Fatalf("写进去 %d 台，想要 3 台", list.Total)
	}
	seen := map[string]bool{}
	for _, term := range list.Items {
		if seen[term.IP] {
			t.Fatalf("两台终端拿到了同一个地址 %s", term.IP)
		}
		seen[term.IP] = true
		if err := ensureTerminalIPInGroup(term.IP, group); err != nil {
			t.Fatalf("终端 %s 的 %s 不在分组区间内：%v", term.MAC, term.IP, err)
		}
	}
	if !seen["192.168.1.12"] {
		t.Fatalf("显式写的地址没被尊重：%v", seen)
	}
}

// 留空的行不能抢走文件里靠后的行显式写死的地址，否则会报「终端 IP 已存在」而该地址实际无人使用。
func TestTerminalImportKeepsBlankRowsOffAddressesPinnedFurtherDown(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3) // .10 .11 .12
	svc := TerminalService{Store: st}
	data, err := xlsx.Write([][]string{
		terminalImportLegacyHeader,
		{"00:11:22:33:44:01", "", group.ID, "", "unknown"},             // 留空，最低空位是 .10
		{"00:11:22:33:44:02", "192.168.1.10", group.ID, "", "unknown"}, // 但 .10 是下面这行钉死的
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Import(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("留空行抢走了下面钉死的地址：%#v", result.Errors)
	}
	byMAC := map[string]string{}
	list, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range list.Items {
		byMAC[term.MAC] = term.IP
	}
	if byMAC["001122334402"] != "192.168.1.10" {
		t.Fatalf("钉死的地址没给对：%v", byMAC)
	}
	if byMAC["001122334401"] == "192.168.1.10" || byMAC["001122334401"] == "" {
		t.Fatalf("留空行的地址不对：%v", byMAC)
	}
}

// —— 八、地址换主人时的陈旧租约 ——

// dnsmasq 还持有上一任的租约时会拒发保留地址，另挑一个（not using configured address X because it is leased to Y）。
// 机器照样能起（启动链路认 MAC），但实际地址与库里不符，且该地址下次还会被分给别人。所以地址易主时必须释放租约。
func TestReleasingTheLeaseWhenAnAddressChangesHands(t *testing.T) {
	newSvc := func(t *testing.T) (context.Context, store.Store, domain.Group, *fakeLeaseSyncer, TerminalService) {
		t.Helper()
		ctx := context.Background()
		st := newImageTestStore(t)
		group := seedTerminalGroup(t, ctx, st, 10)
		syncer := &fakeLeaseSyncer{}
		return ctx, st, group, syncer, TerminalService{Store: st, DHCP: syncer}
	}

	t.Run("删终端", func(t *testing.T) {
		ctx, _, group, syncer, svc := newSvc(t)
		term, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:01", IP: "192.168.1.15", GroupID: group.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Delete(ctx, term.ID); err != nil {
			t.Fatal(err)
		}
		syncer.mustRelease(t, "192.168.1.15", "001122334401")
	})

	t.Run("改终端的 IP", func(t *testing.T) {
		ctx, _, group, syncer, svc := newSvc(t)
		term, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:02", IP: "192.168.1.15", GroupID: group.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, term.ID, TerminalRequest{
			MAC: "00:11:22:33:44:02", IP: "192.168.1.16", GroupID: group.ID,
		}); err != nil {
			t.Fatal(err)
		}
		// 释放的是它腾出的旧地址，不是新地址。
		syncer.mustRelease(t, "192.168.1.15", "001122334402")
		syncer.mustNotRelease(t, "192.168.1.16")
	})

	t.Run("跨组搬迁", func(t *testing.T) {
		ctx, st, from, syncer, svc := newSvc(t)
		to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.2.10", 10)
		term, err := svc.Create(ctx, TerminalRequest{
			MAC: "00:11:22:33:44:03", IP: "192.168.1.15", GroupID: from.ID, State: domain.TerminalStateOffline,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{term.ID}, GroupID: to.ID}); err != nil {
			t.Fatal(err)
		}
		syncer.mustRelease(t, "192.168.1.15", "001122334403")
	})

	// dhcp_release 尽力而为：没人监听也返回 0，未装 dnsmasq-utils 时没有该命令；少一层保护可以，删不掉终端不行。
	t.Run("释放失败不影响操作本身", func(t *testing.T) {
		ctx, _, group, syncer, svc := newSvc(t)
		syncer.releaseErr = errors.New("dhcp_release: command not found")
		term, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:04", IP: "192.168.1.15", GroupID: group.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Delete(ctx, term.ID); err != nil {
			t.Fatalf("释放租约失败把删除也拖垮了：%v", err)
		}
	})
}

type fakeLeaseSyncer struct {
	released   [][2]string
	releaseErr error
}

func (f *fakeLeaseSyncer) Sync(context.Context, []dhcp.GroupConfig) error { return nil }

func (f *fakeLeaseSyncer) ReleaseLease(_ context.Context, ip, mac string) error {
	f.released = append(f.released, [2]string{ip, mac})
	return f.releaseErr
}

func (f *fakeLeaseSyncer) mustRelease(t *testing.T, ip, mac string) {
	t.Helper()
	for _, got := range f.released {
		if got[0] == ip && got[1] == mac {
			return
		}
	}
	t.Fatalf("没有释放 %s(%s) 的租约；实际释放了 %v", ip, mac, f.released)
}

func (f *fakeLeaseSyncer) mustNotRelease(t *testing.T, ip string) {
	t.Helper()
	for _, got := range f.released {
		if got[0] == ip {
			t.Fatalf("不该释放 %s：%v", ip, f.released)
		}
	}
}
