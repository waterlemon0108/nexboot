package adapt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func seedAdaptationStore(t *testing.T) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	seedHealthImage(t, ctx, st, now)
	if err := st.Groups().Create(ctx, domain.Group{
		ID: "grp-1", Name: "g1", StartIP: "192.168.1.10", ClientMax: 10,
		Gateway: "192.168.1.1", Netmask: "255.255.255.0",
		SystemImageID: "win11", SystemConfigID: "win11_default", SystemReductionID: "win11_0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-super", MAC: "AABBCCDDEEFF", IP: "192.168.1.10", GroupID: "grp-1", IsSuper: true, State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := st.Terminals().Create(ctx, domain.Terminal{ID: "term-normal", MAC: "001122334455", IP: "192.168.1.11", GroupID: "grp-1", State: domain.TerminalStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := st.DriverPacks().Create(ctx, domain.DriverPack{
		ID: "pack-1", Name: "intel-i219", Category: domain.DriverCategoryBootCriticalNIC,
		OSType: domain.OSTypeWindows, Arch: "x64", Status: domain.DriverPackEnabled,
		HWIDs: []string{`PCI\VEN_8086&DEV_15B8`}, StoragePath: "/tmp/x", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DriverBundles().Create(ctx, domain.DriverBundle{ID: "bundle-1", Name: "b1", OSType: domain.OSTypeWindows, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.DriverBundlePacks().Create(ctx, domain.DriverBundlePack{ID: "bp-1", BundleID: "bundle-1", PackID: "pack-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DriverBundles().Create(ctx, domain.DriverBundle{ID: "bundle-empty", Name: "empty", OSType: domain.OSTypeWindows, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return st
}

// fakeInjectStorage 只实现 PrepareSuperAdaptation 和 ReadSuperAdaptationResult，其余经内嵌接口为 nil。
type fakeInjectStorage struct {
	storage.StorageAgent
	prepared     *storage.SuperAdaptationReq
	result       storage.SuperAdaptationResult
	resultErr    error
	resultForMAC string
	active       []string
	activeErr    error
}

func (f *fakeInjectStorage) PrepareSuperAdaptation(_ context.Context, req storage.SuperAdaptationReq) error {
	f.prepared = &req
	return nil
}

func (f *fakeInjectStorage) ActiveClientMACs(_ context.Context) ([]string, error) {
	return f.active, f.activeErr
}

func (f *fakeInjectStorage) ReadSuperAdaptationResult(_ context.Context, mac string) (storage.SuperAdaptationResult, error) {
	f.resultForMAC = mac
	return f.result, f.resultErr
}

func TestInjectDriverStagesBundleIntoSuperClone(t *testing.T) {
	ctx := context.Background()
	st := seedAdaptationStore(t)
	fake := &fakeInjectStorage{}
	svc := AdaptationService{
		Store: st, Now: func() time.Time { return time.Now() }, Storage: fake,
		BundleZip: func(_ context.Context, id string) ([]byte, error) { return []byte("ZIP:" + id), nil },
	}

	// 非超管机被拒绝。
	if _, err := svc.InjectDriver(ctx, "term-normal", "bundle-1", false); !errors.Is(err, ErrAdaptationNotSuper) {
		t.Fatalf("non-super err = %v", err)
	}
	online, err := st.Terminals().Get(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	online.State = domain.TerminalStateOnline
	if err := st.Terminals().Update(ctx, online); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-1", false); !errors.Is(err, ErrAdaptationOnline) {
		t.Fatalf("online err = %v", err)
	}
	online.State = domain.TerminalStateUnknown
	if err := st.Terminals().Update(ctx, online); err != nil {
		t.Fatal(err)
	}
	fake.active = []string{"AA:BB:CC:DD:EE:FF"}
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-1", false); !errors.Is(err, ErrAdaptationOnline) {
		t.Fatalf("active session err = %v", err)
	}
	fake.active = nil
	fake.activeErr = storage.ErrNotImplemented
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-1", false); !errors.Is(err, ErrAdaptationOnlineCheck) {
		t.Fatalf("active session check err = %v", err)
	}
	fake.activeErr = nil
	// 空驱动包组被拒绝。
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-empty", false); !errors.Is(err, ErrAdaptationEmptyBundle) {
		t.Fatalf("empty bundle err = %v", err)
	}

	res, err := svc.InjectDriver(ctx, "term-super", "bundle-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if fake.prepared == nil {
		t.Fatal("PrepareSuperAdaptation not called")
	}
	if string(fake.prepared.BundleZip) != "ZIP:bundle-1" {
		t.Fatalf("bundle zip = %q", fake.prepared.BundleZip)
	}
	if !strings.Contains(string(fake.prepared.AdaptScript), "pnputil") {
		t.Fatal("install-on-boot script not embedded")
	}
	// 脚本不再自动关机，由操作者控制关机。
	if strings.Contains(string(fake.prepared.AdaptScript), "shutdown /s") {
		t.Fatal("script must not self-shutdown")
	}
	if fake.prepared.Source.ConfigID != "win11_default" || fake.prepared.Source.SnapshotName != "0" {
		t.Fatalf("source = %#v", fake.prepared.Source)
	}
	// 盘符脚本随注入一起放入，并写好 API 端口。
	if !strings.Contains(string(fake.prepared.MountScript), "/boot/data-disks") {
		t.Fatal("mount script not staged")
	}
	if !strings.Contains(string(fake.prepared.MountScript), "$port = '8080'") {
		t.Fatalf("mount script port not substituted: %q", fake.prepared.MountScript)
	}
	if res.BundleName == "" || len(res.Steps) == 0 {
		t.Fatalf("result = %#v", res)
	}

	// 注入要留下客户机可见的「待固化」标记供界面展示，否则只存在于盘上的关联看不见。
	staged, err := st.Terminals().Get(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	if staged.PendingBundleID == nil || *staged.PendingBundleID != "bundle-1" {
		t.Fatalf("pending bundle id = %v", staged.PendingBundleID)
	}
	if staged.PendingBundleAt == nil {
		t.Fatal("pending bundle at should be set")
	}
	firstStagedAt := *staged.PendingBundleAt

	// 再注入第二个包组必须完全覆盖第一个标记：与离线文件注入语义一致，只保留最近一次注入。
	if err := st.DriverBundles().Create(ctx, domain.DriverBundle{ID: "bundle-2", Name: "b2", OSType: domain.OSTypeWindows, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.DriverBundlePacks().Create(ctx, domain.DriverBundlePack{ID: "bp-2", BundleID: "bundle-2", PackID: "pack-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-2", false); !errors.Is(err, ErrAdaptationOverwrite) {
		t.Fatalf("overwrite without confirmation err = %v", err)
	}
	if _, err := svc.InjectDriver(ctx, "term-super", "bundle-2", true); err != nil {
		t.Fatal(err)
	}
	if fake.prepared == nil || !fake.prepared.ResetClone {
		t.Fatalf("overwrite injection should reset clone, req=%#v", fake.prepared)
	}
	restaged, err := st.Terminals().Get(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	if restaged.PendingBundleID == nil || *restaged.PendingBundleID != "bundle-2" {
		t.Fatalf("pending bundle id after overwrite = %v", restaged.PendingBundleID)
	}
	if !restaged.PendingBundleAt.After(firstStagedAt) && !restaged.PendingBundleAt.Equal(firstStagedAt) {
		t.Fatalf("pending bundle at should advance: first=%v second=%v", firstStagedAt, restaged.PendingBundleAt)
	}
}

func TestInjectDriverWithoutBundleStagesMountScriptOnly(t *testing.T) {
	ctx := context.Background()
	st := seedAdaptationStore(t)
	fake := &fakeInjectStorage{}
	svc := AdaptationService{
		Store: st, Now: func() time.Time { return time.Now() }, Storage: fake, APIPort: "9090",
		BundleZip: func(_ context.Context, id string) ([]byte, error) { return []byte("ZIP:" + id), nil },
	}

	res, err := svc.InjectDriver(ctx, "term-super", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if fake.prepared == nil {
		t.Fatal("PrepareSuperAdaptation not called")
	}
	// 无驱动包组：一次性适配脚本照跑（写 done.txt）但不放 zip，盘符脚本带配置的端口。
	if len(fake.prepared.BundleZip) != 0 {
		t.Fatalf("bundle zip = %q, want empty", fake.prepared.BundleZip)
	}
	if len(fake.prepared.AdaptScript) == 0 {
		t.Fatal("adapt script missing")
	}
	if !strings.Contains(string(fake.prepared.MountScript), "$port = '9090'") {
		t.Fatalf("mount script port = %q", fake.prepared.MountScript)
	}
	if res.BundleName == "" || len(res.Steps) == 0 {
		t.Fatalf("result = %#v", res)
	}
	staged, err := st.Terminals().Get(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	if staged.PendingBundleID == nil || *staged.PendingBundleID != "" {
		t.Fatalf("pending bundle id = %v", staged.PendingBundleID)
	}
}

func TestCheckResultReadsPendingInjectionOutcome(t *testing.T) {
	ctx := context.Background()
	st := seedAdaptationStore(t)
	fake := &fakeInjectStorage{}
	svc := AdaptationService{Store: st, Now: func() time.Time { return time.Now() }, Storage: fake}

	// 非超管机被拒绝。
	if _, err := svc.CheckResult(ctx, "term-normal"); !errors.Is(err, ErrAdaptationNotSuper) {
		t.Fatalf("non-super err = %v", err)
	}
	// 没有待固化注入的超管机被拒绝，不必打快照。
	if _, err := svc.CheckResult(ctx, "term-super"); !errors.Is(err, ErrAdaptationNoPending) {
		t.Fatalf("no-pending err = %v", err)
	}

	terminal, err := st.Terminals().Get(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	bundleID, injectedAt := "bundle-1", time.Now().UTC()
	terminal.PendingBundleID = &bundleID
	terminal.PendingBundleAt = &injectedAt
	if err := st.Terminals().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	// 尚未运行：done.txt 不存在。
	fake.result = storage.SuperAdaptationResult{Done: false}
	view, err := svc.CheckResult(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	if fake.resultForMAC != terminal.MAC {
		t.Fatalf("checked mac = %q, want %q", fake.resultForMAC, terminal.MAC)
	}
	if view.Done || view.BundleID != bundleID || !view.InjectedAt.Equal(injectedAt) {
		t.Fatalf("view = %#v", view)
	}

	// 运行失败，如硬件不匹配（服务未注册）。
	fake.result = storage.SuperAdaptationResult{Done: true, OK: false, Log: "OK=False EXIT=1 AT=..."}
	view, err = svc.CheckResult(ctx, "term-super")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Done || view.OK || !strings.Contains(view.Log, "EXIT=1") {
		t.Fatalf("view = %#v", view)
	}

	// 存储读取错误原样上抛。
	fake.resultErr = fmt.Errorf("no windows partition found")
	if _, err := svc.CheckResult(ctx, "term-super"); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestMountScriptVersionTracksScriptIdentity(t *testing.T) {
	// 版本代表「镜像是否已带脚本」，必须恰好在烘焙内容变化时变化。
	v8080 := MountScriptVersion("8080")
	if v8080 == "" {
		t.Fatal("version is empty; an image could never be marked as baked")
	}
	if v8080 != MountScriptVersion("8080") {
		t.Fatal("version is not stable across calls; every boot would re-inject")
	}
	if v8080 != MountScriptVersion(" 8080 ") || v8080 != MountScriptVersion("") {
		t.Fatal("version ignores the port normalization RenderMountScript applies")
	}
	if v8080 == MountScriptVersion("18080") {
		t.Fatal("changing the API port left the version unchanged; clients would keep calling the old port")
	}
}

// 版本必须包含注入布局：只哈希脚本内容时，布局变化后已烘焙镜像仍自称最新，开机路径会跳过重新注入。
func TestMountScriptVersionCoversTheInjectionLayout(t *testing.T) {
	bodyOnly := sha256.Sum256(RenderMountScript("8080"))
	if MountScriptVersion("8080") == hex.EncodeToString(bodyOnly[:])[:16] {
		t.Fatal("version hashes the script body alone; a layout change would not invalidate baked images")
	}
}

func TestLinuxMountScriptCarriesThePortAndItsOwnVersion(t *testing.T) {
	script := string(RenderLinuxMountScript("18080"))
	if !strings.Contains(script, "port='18080'") || strings.Contains(script, "__ND_API_PORT__") {
		t.Fatalf("port not rendered:\n%s", script)
	}
	if LinuxMountScriptVersion("8080") == MountScriptVersion("8080") || LinuxMountScriptVersion("8080") != LinuxMountScriptVersion("") {
		t.Fatal("linux version must differ from the Windows one and normalize the port")
	}
	if LinuxMountScriptVersion("8080") == LinuxMountScriptVersion("18080") {
		t.Fatal("port change left the version unchanged")
	}
}
