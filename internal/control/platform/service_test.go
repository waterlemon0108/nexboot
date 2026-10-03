package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/systemd"
)

type fakeSystemd struct {
	statuses map[string]systemd.UnitStatus
	showErr  map[string]error
	actions  []string // "unit:action"
}

func (f *fakeSystemd) Show(_ context.Context, unit string) (systemd.UnitStatus, error) {
	if err := f.showErr[unit]; err != nil {
		return systemd.UnitStatus{}, err
	}
	return f.statuses[unit], nil
}
func (f *fakeSystemd) Action(_ context.Context, unit, action string) error {
	f.actions = append(f.actions, unit+":"+action)
	return nil
}
func (f *fakeSystemd) Logs(_ context.Context, unit string, _ int) (string, error) {
	return "log of " + unit, nil
}

func testUnits() []ManagedUnit {
	return []ManagedUnit{
		{Key: "dnsmasq", Unit: "dnsmasq.service", Label: "dns", Critical: true},
		{Key: "self", Unit: "ndiskless.service", Label: "self", ReadOnly: true},
	}
}

func TestListServicesDerivesStatus(t *testing.T) {
	fs := &fakeSystemd{statuses: map[string]systemd.UnitStatus{
		"dnsmasq.service":   {Load: "loaded", Active: "active", Sub: "running", UnitFile: "enabled", MainPID: 42, ActiveSince: time.Now()},
		"ndiskless.service": {Load: "not-found"},
	}}
	svc := ServiceService{Client: fs, Units: testUnits()}
	res, err := svc.ListServices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Items))
	}
	if res.Items[0].Status != "running" || !res.Items[0].Enabled || res.Items[0].MainPID != 42 || res.Items[0].ActiveSince == nil {
		t.Fatalf("unexpected dnsmasq item: %+v", res.Items[0])
	}
	if res.Items[1].Status != "not-installed" || res.Items[1].Installed {
		t.Fatalf("expected not-installed self, got %+v", res.Items[1])
	}
}

func TestListServicesUnknownOnShowError(t *testing.T) {
	fs := &fakeSystemd{showErr: map[string]error{"dnsmasq.service": errors.New("no systemctl")}}
	svc := ServiceService{Client: fs, Units: []ManagedUnit{{Key: "dnsmasq", Unit: "dnsmasq.service"}}}
	res, err := svc.ListServices(context.Background())
	if err != nil {
		t.Fatalf("listing must not fail when systemctl errors: %v", err)
	}
	if res.Items[0].Status != "unknown" {
		t.Fatalf("expected unknown status, got %q", res.Items[0].Status)
	}
}

func TestServiceActionAllowlistAndPolicy(t *testing.T) {
	fs := &fakeSystemd{}
	svc := ServiceService{Client: fs, Units: testUnits()}
	ctx := context.Background()

	if err := svc.ServiceAction(ctx, "dnsmasq", "restart"); err != nil {
		t.Fatalf("valid action failed: %v", err)
	}
	if len(fs.actions) != 1 || fs.actions[0] != "dnsmasq.service:restart" {
		t.Fatalf("unexpected actions: %v", fs.actions)
	}
	if err := svc.ServiceAction(ctx, "self", "restart"); !errors.Is(err, ErrServiceReadOnly) {
		t.Fatalf("expected read-only rejection, got %v", err)
	}
	if err := svc.ServiceAction(ctx, "dnsmasq", "nuke"); !errors.Is(err, ErrServiceAction) {
		t.Fatalf("expected invalid-action rejection, got %v", err)
	}
	if err := svc.ServiceAction(ctx, "ghost", "start"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("expected not-found rejection, got %v", err)
	}
	if len(fs.actions) != 1 {
		t.Fatalf("rejected actions must not reach systemd: %v", fs.actions)
	}
}

func TestServiceLogsNotFound(t *testing.T) {
	svc := ServiceService{Client: &fakeSystemd{}, Units: testUnits()}
	if _, err := svc.ServiceLogs(context.Background(), "ghost", 10); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

// 服务该不该运行取决于节点角色：备机上第二个权威 dnsmasq 会 NAK 主机的客户机，stopped 才正确；
// target.service 是 oneshot，开机加载 LIO 配置后退出，自身 inactive 说明不了 iSCSI 是否可用。
func TestListServicesJudgesAgainstTheExpectedStateForTheRole(t *testing.T) {
	ctx := context.Background()
	lioReady := true
	units := []ManagedUnit{
		{Key: "dnsmasq", Unit: "dnsmasq.service", Label: "DNSMASQ", Critical: true, ActiveOnly: true},
		{Key: "iscsi", Unit: "target.service", Label: "iSCSI", Critical: true, OneShot: true,
			Probe: func() bool { return lioReady }},
		{Key: "zfs", Unit: "zfs-zed.service", Label: "ZFS", Critical: true},
	}
	fake := &fakeSystemd{statuses: map[string]systemd.UnitStatus{
		"dnsmasq.service": {Load: "loaded", Active: "inactive", Sub: "dead"},
		"target.service":  {Load: "loaded", Active: "inactive", Sub: "dead"},
		"zfs-zed.service": {Load: "loaded", Active: "active", Sub: "running"},
	}}

	t.Run("standby", func(t *testing.T) {
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "standby" }}
		res, err := svc.ListServices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]ServiceItem{}
		for _, i := range res.Items {
			by[i.Key] = i
		}
		if !by["dnsmasq"].OK {
			t.Error("a stopped dnsmasq on a standby is correct, not a fault")
		}
		if by["dnsmasq"].Expected != "stopped" {
			t.Errorf("expected=%q, want stopped", by["dnsmasq"].Expected)
		}
		if !by["iscsi"].OK {
			t.Error("a oneshot unit that exited with the capability in place is fine")
		}
		if !by["zfs"].OK {
			t.Error("zfs is running; nothing to complain about")
		}
	})

	t.Run("active", func(t *testing.T) {
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "active" }}
		res, _ := svc.ListServices(ctx)
		by := map[string]ServiceItem{}
		for _, i := range res.Items {
			by[i.Key] = i
		}
		if by["dnsmasq"].OK {
			t.Error("the writer node must serve DHCP; stopped there IS a fault")
		}
		if by["dnsmasq"].Expected != "running" {
			t.Errorf("expected=%q, want running", by["dnsmasq"].Expected)
		}
	})

	t.Run("oneshot unit whose capability is missing", func(t *testing.T) {
		lioReady = false
		defer func() { lioReady = true }()
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "active" }}
		res, _ := svc.ListServices(ctx)
		for _, i := range res.Items {
			if i.Key == "iscsi" && i.OK {
				t.Error("the unit exited AND the capability is gone — that is a fault")
			}
		}
	})
}

// 每个单元先回答提供什么能力（客户机能否连上盘），systemd 单元细节退到排障详情里。
func TestListServicesSpeaksInCapabilities(t *testing.T) {
	ctx := context.Background()
	units := []ManagedUnit{
		{Key: "dnsmasq", Unit: "dnsmasq.service", Label: "DNSMASQ · DHCP/TFTP/PXE",
			Capability: "DHCP / PXE 引导", Critical: true, ActiveOnly: true},
		{Key: "iscsi", Unit: "target.service", Label: "iSCSI Target · LIO",
			Capability: "客户机磁盘", Critical: true, OneShot: true, Probe: func() bool { return true }},
	}
	fake := &fakeSystemd{statuses: map[string]systemd.UnitStatus{
		"dnsmasq.service": {Load: "loaded", Active: "inactive", Sub: "dead"},
		"target.service":  {Load: "loaded", Active: "inactive", Sub: "dead"},
	}}

	t.Run("备机的 DHCP 不是「已停止」，是「由主机提供」", func(t *testing.T) {
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "standby" }}
		res, _ := svc.ListServices(ctx)
		by := map[string]ServiceItem{}
		for _, i := range res.Items {
			by[i.Key] = i
		}
		dhcp := by["dnsmasq"]
		if dhcp.Capability != "DHCP / PXE 引导" {
			t.Errorf("capability=%q", dhcp.Capability)
		}
		// 本机不提供该能力：不是故障，也不是正常运行，而是不适用
		if dhcp.Provided {
			t.Error("备机不提供 DHCP，不该说成本机在提供")
		}
		if !dhcp.OK {
			t.Error("不提供 ≠ 有问题")
		}
	})

	t.Run("主机的 DHCP 是本机在提供", func(t *testing.T) {
		fake.statuses["dnsmasq.service"] = systemd.UnitStatus{Load: "loaded", Active: "active", Sub: "running"}
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "active" }}
		res, _ := svc.ListServices(ctx)
		for _, i := range res.Items {
			if i.Key == "dnsmasq" && !i.Provided {
				t.Error("主机的 DHCP 就是本机在提供")
			}
		}
	})

	t.Run("oneshot 单元照样是「本机提供这项能力」", func(t *testing.T) {
		svc := ServiceService{Client: fake, Units: units, Role: func() string { return "active" }}
		res, _ := svc.ListServices(ctx)
		for _, i := range res.Items {
			if i.Key == "iscsi" {
				if !i.Provided || !i.OK {
					t.Errorf("内核态就绪就是能提供磁盘：provided=%v ok=%v", i.Provided, i.OK)
				}
				if i.Capability != "客户机磁盘" {
					t.Errorf("capability=%q", i.Capability)
				}
			}
		}
	})
}
