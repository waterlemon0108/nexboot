package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/remote"
	"github.com/tianwei/diskless/internal/store"
	"github.com/tianwei/diskless/internal/xlsx"
	"net/http/httptest"
)

var terminalImportLegacyHeader = []string{"MAC", "IP", "GroupID", "IsSuper", "State"}

func TestTerminalServiceNormalizesMACAndRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}

	terminal, err := service.Create(ctx, TerminalRequest{MAC: "aa:bb:cc:dd:ee:ff", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	// 没连过的机器就是离线：界面上只有在线、离线两种状态。
	if terminal.MAC != "AABBCCDDEEFF" || terminal.State != domain.TerminalStateOffline {
		t.Fatalf("terminal = %#v", terminal)
	}

	_, err = service.Create(ctx, TerminalRequest{MAC: "aa-bb-cc-dd-ee-ff", IP: "192.168.1.11", GroupID: group.ID})
	if !errors.Is(err, ErrTerminalMACExists) {
		t.Fatalf("duplicate mac err = %v", err)
	}
}

func TestTerminalServiceValidatesIPUniquenessAndRange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 2)
	service := TerminalService{Store: st}

	if _, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	_, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:66", IP: "192.168.1.10", GroupID: group.ID})
	if !errors.Is(err, ErrTerminalIPExists) {
		t.Fatalf("duplicate ip err = %v", err)
	}
	_, err = service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:77", IP: "192.168.1.12", GroupID: group.ID})
	if !errors.Is(err, ErrTerminalInvalidIP) {
		t.Fatalf("range err = %v", err)
	}
}

func TestTerminalServiceRejectsFullGroup(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 1)
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-seed", MAC: "AABBCCDDEE00", IP: "192.168.1.99", GroupID: group.ID, State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}

	_, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if !errors.Is(err, ErrTerminalGroupFull) {
		t.Fatalf("capacity err = %v", err)
	}
}

func TestTerminalServiceSyncsDHCPOnCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	syncer := &fakeDHCPSyncer{}
	service := TerminalService{Store: st, DHCP: syncer}

	terminal, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 1 || len(syncer.calls[0]) != 1 || len(syncer.calls[0][0].Terminals) != 1 {
		t.Fatalf("create sync calls = %#v", syncer.calls)
	}

	terminal, err = service.Update(ctx, terminal.ID, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.11", GroupID: group.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.IP != "192.168.1.11" || terminal.State != domain.TerminalStateOffline {
		t.Fatalf("updated terminal = %#v", terminal)
	}
	if len(syncer.calls) != 2 || syncer.calls[1][0].Terminals[0].IP != "192.168.1.11" {
		t.Fatalf("update sync calls = %#v", syncer.calls)
	}

	if err := service.Delete(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if len(syncer.calls) != 3 || len(syncer.calls[2][0].Terminals) != 0 {
		t.Fatalf("delete sync calls = %#v", syncer.calls)
	}
}

func TestTerminalServiceCreateUpdateTerminalName(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}

	terminal, err := service.Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, Name: "WorkStation01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Name != "WorkStation01" {
		t.Fatalf("created terminal = %#v", terminal)
	}
	got, err := service.Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "WorkStation01" {
		t.Fatalf("persisted terminal = %#v", got)
	}

	terminal, err = service.Update(ctx, terminal.ID, TerminalRequest{
		MAC:     "00:11:22:33:44:55",
		IP:      "192.168.1.10",
		GroupID: group.ID,
		Name:    "WorkStation01-UPDATED",
		State:   domain.TerminalStateUnknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Name != "WorkStation01-UPDATED" {
		t.Fatalf("updated terminal = %#v", terminal)
	}

	terminal, err = service.Update(ctx, terminal.ID, TerminalRequest{
		MAC:     "00:11:22:33:44:55",
		IP:      "192.168.1.10",
		GroupID: group.ID,
		Name:    "",
		State:   domain.TerminalStateUnknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Name != "" {
		t.Fatalf("cleared terminal name = %#v", terminal)
	}
}

func TestTerminalServiceUpdateDemotingSuperClearsPendingBundle(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}

	terminal, err := service.Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundleID := "bundle-1"
	now := time.Now().UTC()
	terminal.PendingBundleID = &bundleID
	terminal.PendingBundleAt = &now
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	// 走编辑路径取消超管（不经 DisableSuper）也必须清掉暂存驱动包，否则普通机仍显示矛盾的「待固化」。
	updated, err := service.Update(ctx, terminal.ID, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.IsSuper || updated.PendingBundleID != nil || updated.PendingBundleAt != nil {
		t.Fatalf("demoted terminal = %#v", updated)
	}
	got, err := service.Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingBundleID != nil || got.PendingBundleAt != nil {
		t.Fatalf("persisted terminal = %#v", got)
	}
}

func TestTerminalServiceAutoAllocatesNextAvailableIP(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}

	if _, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	terminal, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:66", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.IP != "192.168.1.11" {
		t.Fatalf("allocated ip = %s", terminal.IP)
	}
}

func TestTerminalServiceAutoAllocateReportsFullRange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 1)
	service := TerminalService{Store: st}

	if _, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	_, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:66", GroupID: group.ID})
	if !errors.Is(err, ErrTerminalNoAvailableIP) {
		t.Fatalf("full range err = %v", err)
	}
}

func TestTerminalServiceConcurrentAutoAllocateUniqueIPs(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 20)
	service := TerminalService{Store: st}

	const n = 8
	var wg sync.WaitGroup
	results := make(chan domain.Terminal, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			terminal, err := service.Create(ctx, TerminalRequest{MAC: fmt.Sprintf("00:11:22:33:44:%02X", i), GroupID: group.ID})
			if err != nil {
				errs <- err
				return
			}
			results <- terminal
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for terminal := range results {
		if seen[terminal.IP] {
			t.Fatalf("duplicate ip allocated: %s", terminal.IP)
		}
		seen[terminal.IP] = true
	}
	if len(seen) != n {
		t.Fatalf("allocated %d terminals", len(seen))
	}
}

func TestTerminalServiceImportReportsRowErrorsAndDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}
	if _, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	data, err := xlsx.Write([][]string{
		terminalImportLegacyHeader,
		{"00:11:22:33:44:55", "192.168.1.11", group.ID, "", "unknown"},
		{"00:11:22:33:44:66", "192.168.1.12", "missing", "", "unknown"},
		{"00:11:22:33:44:77", "192.168.1.99", group.ID, "", "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Import(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 3 || result.Created != 0 {
		t.Fatalf("result = %#v", result)
	}
	list, err := service.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 {
		t.Fatalf("import should not partially write, total=%d", list.Total)
	}
}

func TestTerminalServiceImportAndExportRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}
	data, err := xlsx.Write([][]string{
		terminalImportHeader,
		{"00:11:22:33:44:55", "192.168.1.10", "desk-a", group.ID, "false", "unknown"},
		{"00:11:22:33:44:66", "", "desk-b", group.ID, "true", "offline"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Import(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 2 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	second, err := service.Get(ctx, "terminal-001122334466")
	if err != nil {
		t.Fatal(err)
	}
	if second.IP != "192.168.1.11" || !second.IsSuper || second.State != domain.TerminalStateOffline || second.Name != "desk-b" {
		t.Fatalf("auto imported terminal = %#v", second)
	}

	exported, err := service.Export(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	st2 := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st2, 3)
	result, err = (TerminalService{Store: st2}).Import(ctx, exported)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 2 || len(result.Errors) != 0 {
		t.Fatalf("round trip result = %#v", result)
	}
}

func TestTerminalServiceImportOldTemplateCompatible(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}
	data, err := xlsx.Write([][]string{
		terminalImportLegacyHeader,
		{"00:11:22:33:44:55", "192.168.1.10", group.ID, "false", "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Import(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	terminal, err := service.Get(ctx, "terminal-001122334455")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Name != "" {
		t.Fatalf("legacy template should not set name, got=%q", terminal.Name)
	}
}

func TestTerminalServiceMoveSyncsDHCPAndCleansOldClones(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	source := seedTerminalGroup(t, ctx, st, 3)
	target := seedExtraGroup(t, ctx, st, "grp-2", "target", "192.168.1.10", 3)
	if err := st.Servers().Create(ctx, domain.Server{ID: "srv-1", Name: "srv", IP: "192.168.1.2", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp}); err != nil {
		t.Fatal(err)
	}
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClientClones().Create(ctx, domain.ClientClone{ID: "clone-1", TerminalMAC: terminal.MAC, Kind: domain.CloneKindEphemeral, ConfigID: "cfg-1", ReductionID: "red-1", ServerID: "srv-1", Target: "target", VolPath: "/dev/zvol/tank/client"}); err != nil {
		t.Fatal(err)
	}
	syncer := &fakeDHCPSyncer{}
	storage := &fakeMoveStorage{}
	service := TerminalService{Store: st, DHCP: syncer, Storage: storage}

	result, err := service.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{terminal.ID}, GroupID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Moved != 1 || len(storage.cleaned) != 1 || storage.cleaned[0] != terminal.MAC {
		t.Fatalf("result=%#v cleaned=%#v", result, storage.cleaned)
	}
	moved, err := st.Terminals().Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.GroupID != target.ID {
		t.Fatalf("terminal group = %s", moved.GroupID)
	}
	if _, err := st.ClientClones().Get(ctx, "clone-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("clone should be removed, err=%v", err)
	}
	if len(syncer.calls) != 1 || len(syncer.calls[0]) != 2 || len(syncer.calls[0][1].Terminals) != 1 {
		t.Fatalf("sync calls = %#v", syncer.calls)
	}
}

// 目标分组空位不够时整批拒绝，而不是只移一部分。（地址不在目标段内不再拒绝，改为重新分配，见 TestTerminalMoveReallocatesIPsIntoTheTargetRange。）
func TestTerminalServiceMoveRejectsWhenTargetIsFull(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	source := seedTerminalGroup(t, ctx, st, 3)
	target := seedExtraGroup(t, ctx, st, "grp-2", "target", "192.168.2.10", 1)
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{
		ID: "term-existing", MAC: "AABBCCDDEE00", IP: "192.168.2.10",
		GroupID: target.ID, State: domain.TerminalStateUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	service := TerminalService{Store: st, Storage: &fakeMoveStorage{}}

	_, err = service.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{terminal.ID}, GroupID: target.ID})
	if err == nil {
		t.Fatal("expected the full target to refuse the move")
	}
	unchanged, _ := st.Terminals().Get(ctx, terminal.ID)
	if unchanged.GroupID != source.ID || unchanged.IP != "192.168.1.10" {
		t.Fatalf("terminal changed despite the refusal: %#v", unchanged)
	}
}

func TestTerminalServiceSuperSingletonPerConfig(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	service := TerminalService{Store: st}
	first, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:66", IP: "192.168.1.11", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}

	enabled, err := service.EnableSuper(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.IsSuper {
		t.Fatalf("enabled = %#v", enabled)
	}
	_, err = service.EnableSuper(ctx, second.ID)
	if !errors.Is(err, ErrTerminalSuperConflict) || !strings.Contains(err.Error(), first.MAC) {
		t.Fatalf("conflict err = %v", err)
	}
	unchanged, err := service.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.IsSuper {
		t.Fatalf("second should remain normal: %#v", unchanged)
	}

	// 超管机上已暂存未固化的驱动包不能在 DisableSuper 后留下：机器离开超管后它指向的克隆已不作数。
	pendingID, pendingAt := "bundle-x", time.Now().UTC()
	firstWithPending, err := st.Terminals().Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstWithPending.PendingBundleID = &pendingID
	firstWithPending.PendingBundleAt = &pendingAt
	if err := st.Terminals().Update(ctx, firstWithPending); err != nil {
		t.Fatal(err)
	}
	disabled, err := service.DisableSuper(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.PendingBundleID != nil || disabled.PendingBundleAt != nil {
		t.Fatalf("pending bundle should be cleared on disable-super: %#v", disabled)
	}
	enabled, err = service.EnableSuper(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.IsSuper {
		t.Fatalf("second enabled = %#v", enabled)
	}
}

func TestTerminalServiceStopSuperSavesReduction(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC:     "00:11:22:33:44:55",
		IP:      "192.168.1.10",
		GroupID: group.ID,
		IsSuper: true,
		State:   domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingID, pendingAt := "bundle-x", time.Now().UTC()
	terminal.PendingBundleID = &pendingID
	terminal.PendingBundleAt = &pendingAt
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	storage := &fakeSuperStopStorage{reduction: domain.Reduction{ID: "cfg-1_saved", ConfigID: "cfg-1", Name: "@saved", CreatedAt: time.Now().UTC(), Status: domain.ReductionStatusReady}}
	triggered := make(chan struct{}, 1)
	service := TerminalService{Store: st, Storage: storage, BackupTrigger: func(context.Context) error {
		triggered <- struct{}{}
		return nil
	}}

	result, err := service.StopSuper(ctx, terminal.ID, SuperStopRequest{ReductionName: "saved"})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || result.Reduction == nil || result.Reduction.ID != "cfg-1_saved" {
		t.Fatalf("result = %#v", result)
	}
	if storage.req.MAC != terminal.MAC || storage.req.ConfigID != "cfg-1" || storage.req.ReductionName != "@saved" {
		t.Fatalf("storage req = %#v", storage.req)
	}
	saved, err := st.Reductions().Get(ctx, "cfg-1_saved")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "@saved" || saved.Status != domain.ReductionStatusReady {
		t.Fatalf("saved = %#v", saved)
	}
	cfg, err := st.Configs().Get(ctx, "cfg-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReductionID == nil || *cfg.DefaultReductionID != "cfg-1_saved" {
		t.Fatalf("config = %#v", cfg)
	}
	updated, err := st.Terminals().Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 存还原点不动超管状态：存完中间点还要接着装，不该被踢出超管；结束编辑是「取消超管」的事。
	if !updated.IsSuper {
		t.Fatalf("存还原点不该把这台踢出超管：%#v", updated)
	}
	if updated.PendingBundleID != nil || updated.PendingBundleAt != nil {
		t.Fatalf("pending bundle should be cleared on stop-super: %#v", updated)
	}
	task, err := st.Tasks().Get(ctx, result.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskStatusSuccess || task.Progress != 100 || task.Result != "cfg-1_saved" {
		t.Fatalf("task = %#v", task)
	}
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("backup trigger was not called")
	}
}

func TestTerminalServiceStopSuperRejectsDuplicateAndOnline(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC:     "00:11:22:33:44:55",
		IP:      "192.168.1.10",
		GroupID: group.ID,
		IsSuper: true,
		State:   domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	storage := &fakeSuperStopStorage{}
	service := TerminalService{Store: st, Storage: storage}

	_, err = service.StopSuper(ctx, terminal.ID, SuperStopRequest{ReductionName: "@0"})
	if !errors.Is(err, ErrReductionExists) || storage.called {
		t.Fatalf("duplicate err=%v called=%v", err, storage.called)
	}
	terminal.State = domain.TerminalStateOnline
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	_, err = service.StopSuper(ctx, terminal.ID, SuperStopRequest{})
	if !errors.Is(err, ErrTerminalOfflineNeeded) || storage.called {
		t.Fatalf("online err=%v called=%v", err, storage.called)
	}
}

func TestTerminalServiceHeartbeatTimeoutMarksOffline(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	service := TerminalService{Store: st, Now: func() time.Time { return now }}
	terminal, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}

	online, err := service.Heartbeat(ctx, terminal.MAC)
	if err != nil {
		t.Fatal(err)
	}
	if online.State != domain.TerminalStateOnline || online.OnlineSince == nil || online.LastHeartbeatAt == nil || !online.OnlineSince.Equal(now) || !online.LastHeartbeatAt.Equal(now) {
		t.Fatalf("online = %#v", online)
	}
	now = now.Add(3 * time.Minute)
	changed, err := service.SweepOffline(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d", changed)
	}
	offline, err := service.Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if offline.State != domain.TerminalStateOffline || offline.OfflineAt == nil || !offline.OfflineAt.Equal(now) {
		t.Fatalf("offline = %#v", offline)
	}
}

func seedTerminalGroup(t *testing.T, ctx context.Context, st store.Store, clientMax int) domain.Group {
	t.Helper()
	now := time.Now().UTC()
	img := domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: "cfg-1", ImageID: img.ID, Name: "default", CreatedAt: now}
	red := domain.Reduction{ID: "red-1", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	cfg.DefaultReductionID = &red.ID
	group := domain.Group{
		ID:                "grp-1",
		Name:              "default",
		IsDefault:         true,
		StartIP:           "192.168.1.10",
		ClientMax:         clientMax,
		Gateway:           "192.168.1.1",
		Netmask:           "255.255.255.0",
		SystemImageID:     img.ID,
		SystemConfigID:    cfg.ID,
		SystemReductionID: red.ID,
	}
	for _, step := range []struct {
		name string
		err  error
	}{
		{"image", st.Images().Create(ctx, img)},
		{"config", st.Configs().Create(ctx, cfg)},
		{"reduction", st.Reductions().Create(ctx, red)},
		{"group", st.Groups().Create(ctx, group)},
	} {
		if step.err != nil {
			t.Fatalf("%s: %v", step.name, step.err)
		}
	}
	return group
}

func seedExtraGroup(t *testing.T, ctx context.Context, st store.Store, id, name, startIP string, clientMax int) domain.Group {
	t.Helper()
	group := domain.Group{
		ID:                id,
		Name:              name,
		StartIP:           startIP,
		ClientMax:         clientMax,
		Gateway:           "192.168.1.1",
		Netmask:           "255.255.255.0",
		SystemImageID:     "img-1",
		SystemConfigID:    "cfg-1",
		SystemReductionID: "red-1",
	}
	if err := st.Groups().Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	return group
}

type fakeMoveStorage struct {
	storage.StorageAgent
	cleaned []string
	err     error
}

func (s *fakeMoveStorage) CleanupClientClones(_ context.Context, mac string, _ []string) error {
	s.cleaned = append(s.cleaned, mac)
	return s.err
}

type fakeSuperStopStorage struct {
	storage.StorageAgent
	req       storage.SuperStopReq
	reduction domain.Reduction
	dataDisks []storage.SuperStopSavedDisk
	called    bool
	err       error
}

func (s *fakeSuperStopStorage) SuperStop(_ context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	s.req = req
	s.called = true
	return storage.SuperStopResult{System: s.reduction, DataDisks: s.dataDisks}, s.err
}

// seedDataDisk 给分组加一块数据用途镜像的数据盘，带独立配置和基线还原点。
func seedDataDisk(t *testing.T, ctx context.Context, st store.Store, group domain.Group, diskID, imageID, configID, mount string) domain.GroupDisk {
	t.Helper()
	now := time.Now().UTC()
	img := domain.Image{ID: imageID, Name: imageID, OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeData, State: domain.ImageStateNormal, CreatedAt: now}
	cfg := domain.Config{ID: configID, ImageID: img.ID, Name: "default", CreatedAt: now}
	red := domain.Reduction{ID: configID + "_0", ConfigID: cfg.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	cfg.DefaultReductionID = &red.ID
	disk := domain.GroupDisk{ID: diskID, GroupID: group.ID, MountTarget: mount, ImageID: img.ID, ConfigID: cfg.ID}
	for _, step := range []struct {
		name string
		err  error
	}{
		{"image", st.Images().Create(ctx, img)},
		{"config", st.Configs().Create(ctx, cfg)},
		{"reduction", st.Reductions().Create(ctx, red)},
		{"disk", st.GroupDisks().Create(ctx, disk)},
	} {
		if step.err != nil {
			t.Fatalf("%s: %v", step.name, step.err)
		}
	}
	return disk
}

// 超管机的数据盘随系统盘一起保存：每块命名的盘成为其数据配置的还原点并被应用，分组普通机下次开机即用上。
func TestTerminalServiceStopSuperSavesDataDisks(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	// 两块数据盘，LUN 按分组盘顺序为 1、2。
	seedDataDisk(t, ctx, st, group, "disk-d", "img-d", "cfg-d", "D:")
	seedDataDisk(t, ctx, st, group, "disk-e", "img-e", "cfg-e", "E:")
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true, State: domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fake := &fakeSuperStopStorage{
		reduction: domain.Reduction{ID: "cfg-1_saved", ConfigID: "cfg-1", Name: "@saved", CreatedAt: now, Status: domain.ReductionStatusReady},
		dataDisks: []storage.SuperStopSavedDisk{{LUN: 2, Reduction: domain.Reduction{ID: "cfg-e_games", ConfigID: "cfg-e", Name: "@games", CreatedAt: now, Status: domain.ReductionStatusReady}}},
	}
	service := TerminalService{Store: st, Storage: fake}

	// 只保存 E:；D: 未命名，会话被丢弃。
	result, err := service.StopSuper(ctx, terminal.ID, SuperStopRequest{
		ReductionName: "saved",
		DataDisks:     []SuperStopDiskRequest{{DiskID: "disk-e", ReductionName: "games"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.req.DataDisks) != 1 || fake.req.DataDisks[0].LUN != 2 || fake.req.DataDisks[0].ConfigID != "cfg-e" || fake.req.DataDisks[0].ReductionName != "@games" {
		t.Fatalf("storage req data disks = %#v", fake.req.DataDisks)
	}
	if len(result.DataReductions) != 1 || result.DataReductions[0].ID != "cfg-e_games" || result.DataReductions[0].DisplayName != "games" {
		t.Fatalf("result = %#v", result)
	}
	saved, err := st.Reductions().Get(ctx, "cfg-e_games")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ConfigID != "cfg-e" || saved.DisplayName != "games" || saved.Status != domain.ReductionStatusReady {
		t.Fatalf("saved = %#v", saved)
	}
	cfgE, err := st.Configs().Get(ctx, "cfg-e")
	if err != nil {
		t.Fatal(err)
	}
	if cfgE.DefaultReductionID == nil || *cfgE.DefaultReductionID != "cfg-e_games" {
		t.Fatalf("data config should apply the saved point: %#v", cfgE)
	}
	cfgD, err := st.Configs().Get(ctx, "cfg-d")
	if err != nil {
		t.Fatal(err)
	}
	if cfgD.DefaultReductionID == nil || *cfgD.DefaultReductionID != "cfg-d_0" {
		t.Fatalf("unsaved data config must be untouched: %#v", cfgD)
	}
}

// 指定分组没有的盘时，在动机器会话之前就拒绝。
func TestTerminalServiceStopSuperRefusesUnknownDataDisk(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedDataDisk(t, ctx, st, group, "disk-d", "img-d", "cfg-d", "D:")
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true, State: domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSuperStopStorage{}
	service := TerminalService{Store: st, Storage: fake}
	_, err = service.StopSuper(ctx, terminal.ID, SuperStopRequest{DataDisks: []SuperStopDiskRequest{{DiskID: "disk-x", ReductionName: "x"}}})
	if !errors.Is(err, errs.ErrInvalid) || fake.called {
		t.Fatalf("err = %v, storage called = %v", err, fake.called)
	}
	// 数据盘还原点名与其它还原点同样受命名规则约束。
	_, err = service.StopSuper(ctx, terminal.ID, SuperStopRequest{DataDisks: []SuperStopDiskRequest{{DiskID: "disk-d", ReductionName: "0"}}})
	if !errors.Is(err, errs.ErrConflict) && !errors.Is(err, errs.ErrInvalid) || fake.called {
		t.Fatalf("duplicate name err = %v, storage called = %v", err, fake.called)
	}
}

// 勾选了机器从未持有的盘（超管开机后才加入分组）时，按盘符报出并说明怎么做。
func TestTerminalServiceStopSuperNamesTheDiskWithoutASession(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedDataDisk(t, ctx, st, group, "disk-d", "img-d", "cfg-d", "D:")
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true, State: domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSuperStopStorage{err: fmt.Errorf("tank/SCLIENT-X-DATA-1 does not exist: %w", storage.SuperSessionMissing{LUN: 1})}
	service := TerminalService{Store: st, Storage: fake}
	_, err = service.StopSuper(ctx, terminal.ID, SuperStopRequest{DataDisks: []SuperStopDiskRequest{{DiskID: "disk-d", ReductionName: "games"}}})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "数据盘 D:") || !strings.Contains(err.Error(), terminal.MAC) {
		t.Fatalf("err = %v", err)
	}
	still, err := st.Terminals().Get(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !still.IsSuper {
		t.Fatalf("a refused shutdown must leave the machine super: %#v", still)
	}
}

// 两个分组共用一个数据配置时不能同时开超管机，否则互相覆盖保存；冲突信息与系统配置规则一样点名配置和占用的机器。
func TestTerminalServiceSuperExclusiveOverDataConfigs(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	seedDataDisk(t, ctx, st, group, "disk-d", "img-d", "cfg-d", "D:")
	other := seedExtraGroup(t, ctx, st, "grp-2", "second", "192.168.2.10", 3)
	// 第二个分组用不同的系统配置但同一块数据盘。
	now := time.Now().UTC()
	cfg2 := domain.Config{ID: "cfg-2", ImageID: "img-1", Name: "second", CreatedAt: now}
	red2 := domain.Reduction{ID: "cfg-2_0", ConfigID: cfg2.ID, Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}
	cfg2.DefaultReductionID = &red2.ID
	if err := st.Configs().Create(ctx, cfg2); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, red2); err != nil {
		t.Fatal(err)
	}
	other.SystemConfigID, other.SystemReductionID = cfg2.ID, red2.ID
	if err := st.Groups().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "disk-d2", GroupID: other.ID, MountTarget: "D:", ImageID: "img-d", ConfigID: "cfg-d"}); err != nil {
		t.Fatal(err)
	}
	service := TerminalService{Store: st}
	first, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:66", IP: "192.168.2.10", GroupID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.EnableSuper(ctx, second.ID)
	if !errors.Is(err, ErrTerminalSuperConflict) || !strings.Contains(err.Error(), first.MAC) || !strings.Contains(err.Error(), "cfg-d") {
		t.Fatalf("conflict err = %v", err)
	}
}

type fakeSessionStorage struct {
	storage.StorageAgent
	macs []string
	err  error
}

func (s *fakeSessionStorage) ActiveClientMACs(_ context.Context) ([]string, error) {
	return s.macs, s.err
}

func TestReconcileOnlineFromSessions(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	// t1 有会话但当前离线，应转为在线；t2 在线但无会话，应转为离线；t3 有会话且已在线，不变。
	seed := []domain.Terminal{
		{ID: "term-1", MAC: "00505625442C", IP: "192.168.1.10", GroupID: group.ID, State: domain.TerminalStateOffline},
		{ID: "term-2", MAC: "AABBCCDDEEFF", IP: "192.168.1.11", GroupID: group.ID, State: domain.TerminalStateOnline},
		{ID: "term-3", MAC: "001122334455", IP: "192.168.1.12", GroupID: group.ID, State: domain.TerminalStateOnline},
	}
	for _, term := range seed {
		if err := st.Terminals().Create(ctx, term); err != nil {
			t.Fatal(err)
		}
	}

	// LIO 报告的会话混用大小写与分隔符，用以验证归一化。
	sess := &fakeSessionStorage{macs: []string{"00:50:56:25:44:2c", "001122334455"}}
	service := TerminalService{Store: st, Storage: sess, Now: func() time.Time { return now }}
	misses := map[string]int{}

	// 第一轮：t1 立即上线；t2 单次空读还不可信（去抖），本轮保持在线。
	changed, err := service.ReconcileOnline(ctx, misses)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("first scan changed = %d, want 1 (t1 online; t2 offline needs confirmation)", changed)
	}
	if t2, _ := service.Get(ctx, "term-2"); t2.State != domain.TerminalStateOnline {
		t.Fatalf("t2 went offline on a single empty scan despite debounce: %#v", t2)
	}

	// 第二次连续空扫确认 t2 离线。
	changed, err = service.ReconcileOnline(ctx, misses)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("second scan changed = %d, want 1 (t2 offline)", changed)
	}

	t1, _ := service.Get(ctx, "term-1")
	if t1.State != domain.TerminalStateOnline || t1.OnlineSince == nil || !t1.OnlineSince.Equal(now) || t1.OfflineAt != nil {
		t.Fatalf("t1 = %#v", t1)
	}
	t2, _ := service.Get(ctx, "term-2")
	if t2.State != domain.TerminalStateOffline || t2.OfflineAt == nil || !t2.OfflineAt.Equal(now) {
		t.Fatalf("t2 = %#v", t2)
	}
	t3, _ := service.Get(ctx, "term-3")
	if t3.State != domain.TerminalStateOnline {
		t.Fatalf("t3 = %#v", t3)
	}
}

func TestReconcileOnlineDebouncesFlappingSession(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-1", MAC: "001122334455", IP: "192.168.1.12", GroupID: group.ID, State: domain.TerminalStateOnline}); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSessionStorage{}
	service := TerminalService{Store: st, Storage: sess, Now: func() time.Time { return now }}
	misses := map[string]int{}

	// 第 1 次空读：还不离线，一次抖动不算。
	if _, err := service.ReconcileOnline(ctx, misses); err != nil {
		t.Fatal(err)
	}
	if t1, _ := service.Get(ctx, "term-1"); t1.State != domain.TerminalStateOnline {
		t.Fatalf("offline after a single empty read: %#v", t1)
	}
	// 第二次确认前会话恢复：计数清零，保持在线。
	sess.macs = []string{"001122334455"}
	if _, err := service.ReconcileOnline(ctx, misses); err != nil {
		t.Fatal(err)
	}
	if misses["001122334455"] != 0 {
		t.Fatalf("miss counter not reset by reappearing session: %d", misses["001122334455"])
	}
	// 此后又需要连续两次空读。
	sess.macs = nil
	if _, err := service.ReconcileOnline(ctx, misses); err != nil {
		t.Fatal(err)
	}
	if t1, _ := service.Get(ctx, "term-1"); t1.State != domain.TerminalStateOnline {
		t.Fatalf("offline after counter reset + single empty read: %#v", t1)
	}
	if _, err := service.ReconcileOnline(ctx, misses); err != nil {
		t.Fatal(err)
	}
	if t1, _ := service.Get(ctx, "term-1"); t1.State != domain.TerminalStateOffline {
		t.Fatalf("still online after two consecutive empty reads: %#v", t1)
	}
}

func TestReconcileOnlineSkipsWhenSessionsUnavailable(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 5)
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-1", MAC: "001122334455", IP: "192.168.1.12", GroupID: group.ID, State: domain.TerminalStateOnline}); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSessionStorage{err: storage.ErrNotImplemented}
	service := TerminalService{Store: st, Storage: sess}

	if _, err := service.ReconcileOnline(ctx, map[string]int{}); err == nil {
		t.Fatal("expected error to be propagated so the cycle is skipped")
	}
	// 查询不到会话时状态必须不变。
	t1, _ := service.Get(ctx, "term-1")
	if t1.State != domain.TerminalStateOnline {
		t.Fatalf("t1 state changed despite unavailable sessions: %#v", t1)
	}
}

// 删除终端是明确意图，不像离线检测那样需要宽限期；克隆若留给回收器，会在机器已从界面消失后仍挡住配置合并和镜像删除。
func TestTerminalServiceDeleteReclaimsCloneImmediately(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	storage := &fakeMoveStorage{}
	svc := TerminalService{Store: st, DHCP: &fakeDHCPSyncer{}, Storage: storage}
	created, err := svc.Create(ctx, TerminalRequest{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if len(storage.cleaned) != 1 || storage.cleaned[0] != "AABBCCDDEEFF" {
		t.Fatalf("cleaned = %#v, want the terminal's clones reclaimed", storage.cleaned)
	}
	if _, err := st.Terminals().Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("terminal err = %v", err)
	}
}

// 超管机的持久克隆（SCLIENT-*，系统盘与数据盘）不属于客户机克隆：删除终端也要删掉，否则它们一直钉住来源配置。
func TestTerminalServiceDeleteDropsSuperSession(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	fake := &fakeDeleteSuperStorage{}
	svc := TerminalService{Store: st, DHCP: &fakeDHCPSyncer{}, Storage: fake}
	created, err := svc.Create(ctx, TerminalRequest{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10", GroupID: group.ID, IsSuper: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	// 丢弃而非保存：没有还原点名，也没有数据盘。
	if !fake.stopped || fake.stopReq.MAC != "AABBCCDDEEFF" || fake.stopReq.ReductionName != "" || len(fake.stopReq.DataDisks) != 0 {
		t.Fatalf("super session not discarded: stopped=%v req=%#v", fake.stopped, fake.stopReq)
	}
	if len(fake.cleaned) != 1 {
		t.Fatalf("cleaned = %#v", fake.cleaned)
	}
}

type fakeDeleteSuperStorage struct {
	fakeMoveStorage
	stopped bool
	stopReq storage.SuperStopReq
}

func (s *fakeDeleteSuperStorage) SuperStop(_ context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	s.stopped = true
	s.stopReq = req
	return storage.SuperStopResult{}, nil
}

// 克隆回收不了（机器仍在运行、zvol 忙）时删除必须失败，不能静默留下孤儿数据集。
func TestTerminalServiceDeleteStopsWhenCloneCannotBeReclaimed(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	svc := TerminalService{Store: st, DHCP: &fakeDHCPSyncer{}, Storage: &fakeMoveStorage{err: errors.New("dataset is busy")}}
	created, err := svc.Create(ctx, TerminalRequest{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, created.ID); err == nil {
		t.Fatal("expected the delete to fail")
	}
	if _, err := st.Terminals().Get(ctx, created.ID); err != nil {
		t.Fatalf("terminal must survive a failed reclaim: %v", err)
	}
}

// 每个分组有自己的 IP 段，从别组移来的终端地址必然不属于目标组：批量移动应在目标段内重新分配，而不是报「终端 IP 无效」。
func TestTerminalMoveReallocatesIPsIntoTheTargetRange(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.10.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.20.10", 30)
	svc := TerminalService{Store: st}

	var ids []string
	for _, mac := range []string{"00:11:22:33:44:01", "00:11:22:33:44:02"} {
		term, err := svc.Create(ctx, TerminalRequest{MAC: mac, GroupID: from.ID, State: domain.TerminalStateOffline})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, term.ID)
	}

	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: ids, GroupID: to.ID}); err != nil {
		t.Fatalf("move: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		term, err := st.Terminals().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if term.GroupID != to.ID {
			t.Fatalf("terminal %s still in %s", id, term.GroupID)
		}
		if err := ensureTerminalIPInGroup(term.IP, to); err != nil {
			t.Fatalf("terminal %s kept %s, outside the target range: %v", id, term.IP, err)
		}
		if seen[term.IP] {
			t.Fatalf("two moved terminals share %s", term.IP)
		}
		seen[term.IP] = true
	}
}

// 已在目标段内的终端保留原地址，来回移动不能给机房重新编号。
func TestTerminalMoveKeepsAnIPTheTargetAlreadyOwns(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.30.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.30.100", 30)
	svc := TerminalService{Store: st}
	term, err := svc.Create(ctx, TerminalRequest{
		MAC: "00:11:22:33:44:03", IP: "192.168.30.105", GroupID: to.ID, State: domain.TerminalStateOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 移出再移回。
	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{term.ID}, GroupID: from.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{term.ID}, GroupID: to.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Terminals().Get(ctx, term.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureTerminalIPInGroup(got.IP, to); err != nil {
		t.Fatalf("ip = %s: %v", got.IP, err)
	}
}

// 目标分组地址用完时拒绝，不能静默只移一半。
func TestTerminalMoveRefusesWhenTheTargetRangeIsFull(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.40.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.50.10", 1)
	svc := TerminalService{Store: st}
	if _, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:04", GroupID: to.ID, State: domain.TerminalStateOffline}); err != nil {
		t.Fatal(err)
	}
	term, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:05", GroupID: from.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{term.ID}, GroupID: to.ID})
	if err == nil {
		t.Fatal("expected the full target group to refuse the move")
	}
	got, _ := st.Terminals().Get(ctx, term.ID)
	if got.GroupID != from.ID {
		t.Fatalf("terminal moved anyway: %#v", got)
	}
}

// fakeSuperToggleStorage 记录切换超管时对池发出的请求。
type fakeSuperToggleStorage struct {
	storage.StorageAgent
	active  []string
	stops   []storage.SuperStopReq
	stopErr error
}

func (f *fakeSuperToggleStorage) ActiveClientMACs(context.Context) ([]string, error) {
	return f.active, nil
}

func (f *fakeSuperToggleStorage) SuperStop(_ context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	f.stops = append(f.stops, req)
	return storage.SuperStopResult{}, f.stopErr
}

func setTerminalState(t *testing.T, ctx context.Context, st store.Store, id string, super bool, state domain.TerminalState) {
	t.Helper()
	terminal, err := st.Terminals().Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	terminal.IsSuper, terminal.State = super, state
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
}

// 超管开关只改标记、下次开机才生效，机器开着时改会让界面与实际运行不符，三个入口都要挡。
func TestSuperModeCannotChangeWhileTheMachineRuns(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	pool := &fakeSuperToggleStorage{}
	service := TerminalService{Store: st, Storage: pool}
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, Name: "teacher"})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(super bool) error {
		_, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, IP: terminal.IP, GroupID: group.ID, Name: "teacher", IsSuper: super})
		return err
	}
	refused := func(what string, err error, wantSuper bool) {
		t.Helper()
		var running TerminalRunningSuperChange
		if !errors.As(err, &running) || !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "teacher") || !strings.Contains(err.Error(), "关机") {
			t.Fatalf("%s：在线时要拒绝并叫人先关机，得到 %v", what, err)
		}
		got, _ := st.Terminals().Get(ctx, terminal.ID)
		if got.IsSuper != wantSuper {
			t.Fatalf("%s：被拒绝了标记却变了：IsSuper=%v", what, got.IsSuper)
		}
		if len(pool.stops) != 0 {
			t.Fatalf("%s：被拒绝了却动了超管盘：%+v", what, pool.stops)
		}
	}

	setTerminalState(t, ctx, st, terminal.ID, false, domain.TerminalStateOnline)
	_, err = service.EnableSuper(ctx, terminal.ID)
	refused("在线设为超管", err, false)
	refused("在线经编辑表单设为超管", edit(true), false)

	setTerminalState(t, ctx, st, terminal.ID, true, domain.TerminalStateOnline)
	_, err = service.DisableSuper(ctx, terminal.ID)
	refused("在线取消超管", err, true)
	refused("在线经编辑表单取消超管", edit(false), true)

	// 在线时改名之类不碰超管开关的编辑照常。
	if _, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, IP: terminal.IP, GroupID: group.ID, Name: "teacher-2", IsSuper: true}); err != nil {
		t.Fatalf("不改超管开关的编辑不该被挡：%v", err)
	}
}

// 「取消超管」是丢弃改动，必须真把盘删掉；否则下次设为超管开机会复用旧盘，丢弃过的改动又回来。
func TestDisableSuperDiscardsTheSuperDisk(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	pool := &fakeSuperToggleStorage{}
	service := TerminalService{Store: st, Storage: pool}
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	setTerminalState(t, ctx, st, terminal.ID, true, domain.TerminalStateOffline)

	// 删不掉就不算取消：标记保留，操作者可重试或改为保存。
	pool.stopErr = errors.New("dataset is busy")
	if _, err := service.DisableSuper(ctx, terminal.ID); err == nil {
		t.Fatal("超管盘没删掉却报了成功")
	}
	if got, _ := st.Terminals().Get(ctx, terminal.ID); !got.IsSuper {
		t.Fatal("超管盘没删掉，标记却已取消")
	}

	pool.stopErr, pool.stops = nil, nil
	disabled, err := service.DisableSuper(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.IsSuper {
		t.Fatalf("disabled = %#v", disabled)
	}
	// 只带 MAC、不带还原点名表示不保存、整块丢弃（系统盘和数据盘）。
	if len(pool.stops) != 1 || pool.stops[0].MAC != terminal.MAC || pool.stops[0].ReductionName != "" || len(pool.stops[0].DataDisks) != 0 {
		t.Fatalf("要丢弃这台的超管盘：%+v", pool.stops)
	}

	// 编辑表单里把「终端类型」改回普通，是同一件事。
	setTerminalState(t, ctx, st, terminal.ID, true, domain.TerminalStateOffline)
	pool.stops = nil
	if _, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, IP: terminal.IP, GroupID: group.ID, IsSuper: false}); err != nil {
		t.Fatal(err)
	}
	if len(pool.stops) != 1 || pool.stops[0].ReductionName != "" {
		t.Fatalf("经编辑表单降级也要丢弃超管盘：%+v", pool.stops)
	}
}

// 刚开机的机器库里可能还没标在线，删盘前以实时会话为准，否则当场蓝屏。
func TestDisableSuperRefusesALiveSessionTheDatabaseHasNotSeen(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID, Name: "teacher"})
	if err != nil {
		t.Fatal(err)
	}
	setTerminalState(t, ctx, st, terminal.ID, true, domain.TerminalStateOffline)
	pool := &fakeSuperToggleStorage{active: []string{"00:11:22:33:44:55"}}
	service := TerminalService{Store: st, Storage: pool}

	_, err = service.DisableSuper(ctx, terminal.ID)
	var running TerminalRunningSuperChange
	if !errors.As(err, &running) {
		t.Fatalf("有实时会话就要拒绝：%v", err)
	}
	if len(pool.stops) != 0 {
		t.Fatalf("有实时会话还删了超管盘：%+v", pool.stops)
	}
}

// 设为超管时池里若已有它的超管盘，只能是残留，先清掉；已是超管的再点一次不能清，那是未保存的工作。
func TestEnableSuperDropsALeftoverSuperDiskButNeverAnActiveOne(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	pool := &fakeSuperToggleStorage{}
	service := TerminalService{Store: st, Storage: pool}
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}

	enabled, err := service.EnableSuper(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.IsSuper || len(pool.stops) != 1 || pool.stops[0].MAC != terminal.MAC || pool.stops[0].ReductionName != "" {
		t.Fatalf("设为超管前要清掉遗留的超管盘：enabled=%v stops=%+v", enabled.IsSuper, pool.stops)
	}

	pool.stops = nil
	if _, err := service.EnableSuper(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, IP: terminal.IP, GroupID: group.ID, Name: "改个名", IsSuper: true}); err != nil {
		t.Fatal(err)
	}
	if len(pool.stops) != 0 {
		t.Fatalf("已经是超管的机器不能清它的超管盘：%+v", pool.stops)
	}
}

// 超管盘只在写入者上（旧写入者降级时已清掉自己那份），所以删盘找本机；按上次开机落点找会找错，那台宕机时还会整体失败。
func TestSuperSwitchDropsTheDiskOnTheWriterNotWhereTheLastBootLanded(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	now := time.Now().UTC()
	local := &fakeSuperToggleStorage{}
	peer := &fakeSuperToggleStorage{}
	srv := httptest.NewServer(remote.Handler{Agent: peer, Token: "tok"})
	defer srv.Close()
	if err := st.Servers().Create(ctx, domain.Server{ID: "node-b", Name: "b", IP: "b", PortalIP: "10.9.0.2",
		APIURL: srv.URL, Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &now}); err != nil {
		t.Fatal(err)
	}
	service := TerminalService{Store: st, Storage: local, Now: func() time.Time { return now },
		Place: &place.Router{Store: st, NodeID: "node-a", Local: local, LocalPortal: "vip", Token: "tok",
			Now: func() time.Time { return now }}}
	terminal, err := (TerminalService{Store: st}).Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:55", IP: "192.168.1.10", GroupID: group.ID})
	if err != nil {
		t.Fatal(err)
	}
	onPeer := "node-b"
	stored, _ := st.Terminals().Get(ctx, terminal.ID)
	stored.StorageServerID = &onPeer // 上次开机落在别的节点
	if err := st.Terminals().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if _, err := service.EnableSuper(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DisableSuper(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if len(local.stops) != 2 || len(peer.stops) != 0 {
		t.Fatalf("超管盘要在本机（写入者）上删：本机 %d 次，别台 %d 次", len(local.stops), len(peer.stops))
	}
}

// 换分组时 IP 留空：原地址不在新分组范围内，应在新分组里自动分配，而不是拿旧地址去报错。
func TestTerminalServiceUpdateMovingGroupWithBlankIPAllocatesInTheNewGroup(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	group := seedTerminalGroup(t, ctx, st, 3)
	other := seedExtraGroup(t, ctx, st, "grp-2", "ubuntu", "192.168.1.40", 30)
	service := TerminalService{Store: st}
	terminal, err := service.Create(ctx, TerminalRequest{MAC: "00:50:56:2E:F6:4E", GroupID: group.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}

	moved, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, GroupID: other.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatalf("换分组留空 IP 应自动分配: %v", err)
	}
	if !ipInRange(t, moved.IP, "192.168.1.40", 30) {
		t.Fatalf("应分配到新分组范围内，得到 %s", moved.IP)
	}

	// 不换分组时留空仍保持原地址。
	kept, err := service.Update(ctx, terminal.ID, TerminalRequest{MAC: terminal.MAC, GroupID: other.ID, State: domain.TerminalStateOffline})
	if err != nil || kept.IP != moved.IP {
		t.Fatalf("不换分组留空应保持原 IP: %v %s → %s", err, moved.IP, kept.IP)
	}
}

func ipInRange(t *testing.T, ip, start string, count int) bool {
	t.Helper()
	a, err := parseTerminalIP(ip)
	if err != nil {
		return false
	}
	s, _ := parseTerminalIP(start)
	return ipv4Uint32(a) >= ipv4Uint32(s) && ipv4Uint32(a) < ipv4Uint32(s)+uint32(count)
}

// 旧版本导出表格里的 unknown 照收，按离线入库。
func TestTerminalStateUnknownReadsAsOffline(t *testing.T) {
	for _, in := range []domain.TerminalState{"", domain.TerminalStateUnknown} {
		got, err := normalizeTerminalState(in, "")
		if err != nil || got != domain.TerminalStateOffline {
			t.Fatalf("%q -> %q, %v", in, got, err)
		}
	}
}

// 批量里混有本来就在目标组、地址在区间内的机器：它的地址要先算作已占，新移入的机器不能分到它，整批也不能因唯一约束失败。
func TestTerminalMoveDoesNotHandOutAnAddressASelectedMemberAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.60.10", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.70.10", 30)
	fake := &fakeMoveStorage{}
	svc := TerminalService{Store: st, Storage: fake}
	newcomer, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:a1", GroupID: from.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	member, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:a2", IP: "192.168.70.10", GroupID: to.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{newcomer.ID, member.ID}, GroupID: to.ID}); err != nil {
		t.Fatalf("move: %v", err)
	}
	gotNew, _ := st.Terminals().Get(ctx, newcomer.ID)
	gotMember, _ := st.Terminals().Get(ctx, member.ID)
	if gotMember.IP != "192.168.70.10" || gotNew.IP == gotMember.IP || gotNew.GroupID != to.ID {
		t.Fatalf("newcomer=%s member=%s", gotNew.IP, gotMember.IP)
	}
}

// 被选中、本就在目标组的机器也占名额：这里目标组只容 2 台，组里已有一台地址在区间外的机器和一台被选中的机器，
// 再移入一台就超员，必须整批拒绝，不能因漏算被选中的那台而放行。
func TestTerminalMoveCountsSelectedMembersAgainstTheLimit(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	seedTerminalGroup(t, ctx, st, 30)
	other := seedExtraGroup(t, ctx, st, "grp-other", "三班", "192.168.91.5", 30)
	from := seedExtraGroup(t, ctx, st, "grp-from", "一班", "192.168.90.11", 30)
	to := seedExtraGroup(t, ctx, st, "grp-to", "二班", "192.168.90.10", 2)
	svc := TerminalService{Store: st}
	stray, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:b0", IP: "192.168.91.5", GroupID: other.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	stray.GroupID = to.ID // 分组区间改过后留在区间外的机器
	if err := st.Terminals().Update(ctx, stray); err != nil {
		t.Fatal(err)
	}
	member, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:b1", IP: "192.168.90.10", GroupID: to.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	newcomer, err := svc.Create(ctx, TerminalRequest{MAC: "00:11:22:33:44:b2", IP: "192.168.90.11", GroupID: from.ID, State: domain.TerminalStateOffline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Move(ctx, MoveTerminalsRequest{TerminalIDs: []string{newcomer.ID, member.ID}, GroupID: to.ID})
	if !errors.Is(err, ErrTerminalGroupFull) {
		t.Fatalf("目标组只容 2 台、移完会有 3 台，应拒绝，得到 %v", err)
	}
	if got, _ := st.Terminals().Get(ctx, newcomer.ID); got.GroupID != from.ID {
		t.Fatalf("被拒绝的移动仍改了分组：%#v", got)
	}
}
