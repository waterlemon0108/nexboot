package local

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

func TestInjectAdaptationLaysOutGPOStartupScript(t *testing.T) {
	// 临时目录充当已挂载的 Windows 分区（带 Windows\System32，能通过分区校验）。
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "Windows", "System32"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := storage.SuperAdaptationReq{
		MAC:         "00505625442C",
		BundleZip:   []byte("PK-fake-zip"),
		AdaptScript: []byte("Write-Output hi\nshutdown /s\n"),
	}
	if err := injectAdaptation(mnt, req); err != nil {
		t.Fatal(err)
	}

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	if got := read("ndadapt/bundle.zip"); got != "PK-fake-zip" {
		t.Fatalf("bundle.zip = %q", got)
	}
	// 适配脚本应转为 CRLF。
	ps1 := read("Windows/System32/GroupPolicy/Machine/Scripts/Startup/adapt.ps1")
	if !strings.Contains(ps1, "Write-Output hi\r\nshutdown /s\r\n") {
		t.Fatalf("adapt.ps1 not CRLF-normalized: %q", ps1)
	}
	// 组策略注册文件须含 Scripts CSE GUID 和启动项。
	if gpt := read("Windows/System32/GroupPolicy/gpt.ini"); !strings.Contains(gpt, "42B5FAAE-6536-11D2-AE5A-0000F87571E3") {
		t.Fatalf("gpt.ini missing Scripts CSE GUID: %q", gpt)
	}
	if ini := read("Windows/System32/GroupPolicy/Machine/Scripts/scripts.ini"); !strings.Contains(ini, "0CmdLine=ndadapt.cmd") {
		t.Fatalf("scripts.ini = %q", ini)
	}
	if cmd := read("Windows/System32/GroupPolicy/Machine/Scripts/Startup/ndadapt.cmd"); !strings.Contains(cmd, "adapt.ps1") {
		t.Fatalf("ndadapt.cmd = %q", cmd)
	}
}

func TestInjectAdaptationStagesPersistentMountScript(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "Windows", "System32"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := storage.SuperAdaptationReq{
		MAC:         "00505625442C",
		AdaptScript: []byte("adapt\n"),
		MountScript: []byte("mount D\nmount E\n"),
	}
	if err := injectAdaptation(mnt, req); err != nil {
		t.Fatal(err)
	}

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	if ps1 := read("Windows/System32/GroupPolicy/Machine/Scripts/Startup/mount-disks.ps1"); !strings.Contains(ps1, "mount D\r\nmount E\r\n") {
		t.Fatalf("mount-disks.ps1 not CRLF-normalized: %q", ps1)
	}
	// 挂载启动器必须脱离运行：组策略启动脚本是同步的，不能卡住登录界面。
	if cmd := read("Windows/System32/GroupPolicy/Machine/Scripts/Startup/ndmount.cmd"); !strings.Contains(cmd, "mount-disks.ps1") || !strings.Contains(cmd, "start \"\"") {
		t.Fatalf("ndmount.cmd = %q", cmd)
	}
	// 两个启动项都要注册：一次性适配在前，常驻挂载在后。
	ini := read("Windows/System32/GroupPolicy/Machine/Scripts/scripts.ini")
	if !strings.Contains(ini, "0CmdLine=ndadapt.cmd") || !strings.Contains(ini, "1CmdLine=ndmount.cmd") {
		t.Fatalf("scripts.ini = %q", ini)
	}
}

func TestMergeMountScriptFreshImage(t *testing.T) {
	mnt := t.TempDir()
	if err := mergeMountScript(mnt, []byte("mount\n")); err != nil {
		t.Fatal(err)
	}
	gp := filepath.Join(mnt, "Windows", "System32", "GroupPolicy")
	ini, err := os.ReadFile(filepath.Join(gp, "Machine", "Scripts", "scripts.ini"))
	if err != nil || !strings.Contains(string(ini), "0CmdLine=ndmount.cmd") {
		t.Fatalf("scripts.ini = %q err=%v", ini, err)
	}
	if gpt, err := os.ReadFile(filepath.Join(gp, "gpt.ini")); err != nil || !strings.Contains(string(gpt), "42B5FAAE") {
		t.Fatalf("gpt.ini = %q err=%v", gpt, err)
	}
	if ps1, err := os.ReadFile(filepath.Join(gp, "Machine", "Scripts", "Startup", "mount-disks.ps1")); err != nil || string(ps1) != "mount\r\n" {
		t.Fatalf("mount-disks.ps1 = %q err=%v", ps1, err)
	}
}

func TestMergeMountScriptPreservesPendingAdaptEntry(t *testing.T) {
	// 超管机已放入的一次性 adapt.ps1 项必须保留：合并只追加 ndmount，不重置组策略目录。
	mnt := t.TempDir()
	gp := filepath.Join(mnt, "Windows", "System32", "GroupPolicy")
	scripts := filepath.Join(gp, "Machine", "Scripts")
	if err := os.MkdirAll(filepath.Join(scripts, "Startup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "scripts.ini"), []byte(adaptScriptsINI), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gp, "gpt.ini"), []byte(storage.GroupPolicyINI), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mergeMountScript(mnt, []byte("mount\n")); err != nil {
		t.Fatal(err)
	}
	ini, err := os.ReadFile(filepath.Join(scripts, "scripts.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ini), "0CmdLine=ndadapt.cmd") || !strings.Contains(string(ini), "1CmdLine=ndmount.cmd") {
		t.Fatalf("scripts.ini = %q", ini)
	}
	// Version 应递增，让 gpsvc 重新处理 scripts.ini。
	if gpt, err := os.ReadFile(filepath.Join(gp, "gpt.ini")); err != nil || !strings.Contains(string(gpt), "Version=65540") {
		t.Fatalf("gpt.ini = %q err=%v", gpt, err)
	}

	// 再次合并时 ini 不变（ndmount 已注册）。
	if err := mergeMountScript(mnt, []byte("mount v2\n")); err != nil {
		t.Fatal(err)
	}
	ini2, err := os.ReadFile(filepath.Join(scripts, "scripts.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ini2) != string(ini) {
		t.Fatalf("scripts.ini changed on re-merge: %q", ini2)
	}
	// 脚本内容本身要刷新。
	if ps1, err := os.ReadFile(filepath.Join(scripts, "Startup", "mount-disks.ps1")); err != nil || string(ps1) != "mount v2\r\n" {
		t.Fatalf("mount-disks.ps1 = %q err=%v", ps1, err)
	}
}

// 上一次注入留下的适配注册被替换而不是叠加：ndadapt 只注册一次，脚本内容是这次的。
// 镜像自带的组策略不再整目录清掉（见 TestInjectAdaptationMergesIntoTheImagesOwnPolicy）。
func TestInjectAdaptationReplacesStalePolicy(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(storage.StartupScriptDir(mnt), 0o755); err != nil {
		t.Fatal(err)
	}
	ini := filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini")
	if err := os.WriteFile(ini, []byte("[Startup]\r\n0CmdLine=ndadapt.cmd\r\n0Parameters=\r\n1CmdLine=ndmount.cmd\r\n1Parameters=\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage.StartupScriptDir(mnt), "adapt.ps1"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectAdaptation(mnt, storage.SuperAdaptationReq{AdaptScript: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	got := readPolicy(t, ini)
	if got != "[Startup]\r\n0CmdLine=ndadapt.cmd\r\n0Parameters=\r\n1CmdLine=ndmount.cmd\r\n1Parameters=\r\n" {
		t.Fatalf("scripts.ini = %q", got)
	}
	if ps1 := readPolicy(t, filepath.Join(storage.StartupScriptDir(mnt), "adapt.ps1")); ps1 != "new" {
		t.Fatalf("adapt.ps1 = %q, want this run's script", ps1)
	}
}

// gpt.ini 只递增行首的 Version；gPCFunctionalityVersion 不是策略版本，改了它 gpsvc 可能拒读。
func TestBumpGptVersionLeavesFunctionalityVersionAlone(t *testing.T) {
	got := bumpGptVersion("[General]\r\ngPCFunctionalityVersion=2\r\nVersion=65538\r\n")
	if got != "[General]\r\ngPCFunctionalityVersion=2\r\nVersion=65539\r\n" {
		t.Fatalf("got %q", got)
	}
}

// 镜像版本哈希的是 storage 中对注入布局的描述；本用例确保实际写入与该描述一致，防止两者悄悄脱节。
func TestMergeMountScriptWritesTheLayoutStorageDescribes(t *testing.T) {
	mnt := t.TempDir()
	if err := mergeMountScript(mnt, []byte("mount\n")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(storage.StartupScriptDir(mnt))
	if err != nil {
		t.Fatalf("startup dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{storage.MountLauncherName, storage.MountScriptName}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("startup scripts = %v, want %v — the layout hashed into the version no longer matches what is written", names, want)
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	if got := read(filepath.Join(storage.StartupScriptDir(mnt), storage.MountLauncherName)); got != storage.MountLauncherBody {
		t.Fatalf("launcher = %q, want %q", got, storage.MountLauncherBody)
	}
	if got := read(filepath.Join(storage.GroupPolicyDir(mnt), "gpt.ini")); got != storage.GroupPolicyINI {
		t.Fatalf("gpt.ini = %q, want %q", got, storage.GroupPolicyINI)
	}
	if got := read(filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini")); !strings.Contains(got, storage.MountLauncherName) {
		t.Fatalf("scripts.ini = %q, want it to register %q", got, storage.MountLauncherName)
	}
}

// 适配写入和结果回读都要排在同一台机器的开机供给、超管保存、数据盘发布之后，
// 否则会往正被改名或销毁的 SCLIENT 克隆上打快照、挂载。
func TestSuperAdaptationEntriesHoldTheClientLock(t *testing.T) {
	for name, call := range map[string]func(*Agent) error{
		"PrepareSuperAdaptation": func(a *Agent) error {
			return a.PrepareSuperAdaptation(context.Background(), storage.SuperAdaptationReq{
				MAC: "aa:bb:cc:dd:ee:ff", Source: storage.ClientSource{ConfigID: "nope", SnapshotName: "0"}, AdaptScript: []byte("x"),
			})
		},
		"ReadSuperAdaptationResult": func(a *Agent) error {
			_, err := a.ReadSuperAdaptationResult(context.Background(), "aa:bb:cc:dd:ee:ff")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			z := newFakePool()
			agent := New("server-a", z)
			unlock := agent.lockClient("AA:BB:CC:DD:EE:FF")
			done := make(chan struct{})
			go func() {
				_ = call(agent)
				close(done)
			}()
			select {
			case <-done:
				t.Fatal("returned while another operation held the client lock")
			case <-time.After(200 * time.Millisecond):
			}
			if len(z.ops) != 0 {
				t.Fatalf("touched the pool while locked out: %v", z.ops)
			}
			unlock()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("did not finish after the lock was released")
			}
		})
	}
}

// 回读用的临时克隆是客户机克隆一类，必须落在 run/ 下：放在 nd/ 会被复制和备份带走。
func TestReadSuperAdaptationResultKeepsItsScratchCloneUnderRun(t *testing.T) {
	z := poolWith("tank/run/SCLIENT-AABBCCDDEEFF")
	agent := New("server-a", z)
	agent.runner = &fakeRunner{lsblkOutput: `{"blockdevices":[]}`}
	var cloned string
	// 没有分区可读，短超时让它不必等满分区轮询。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = agent.ReadSuperAdaptationResult(ctx, "aa:bb:cc:dd:ee:ff")
	for _, op := range z.ops {
		if strings.HasPrefix(op, "clone ") {
			cloned = strings.TrimPrefix(op, "clone ")
		}
	}
	if !strings.HasPrefix(cloned, "tank/run/") {
		t.Fatalf("scratch clone = %q, want it under tank/run/", cloned)
	}
	if c := storage.Classify(storage.DatasetID(cloned)); c.Kind != storage.CarrierInspect {
		t.Fatalf("scratch clone classified as %q, want a transient inspect carrier", c.Kind)
	}
}

// writeImagePolicy 在挂载点里放一份镜像自带的本地组策略：自己的启动/关机脚本、注册表策略和别的 CSE。
func writeImagePolicy(t *testing.T, mnt string, scriptsINI []byte) {
	t.Helper()
	gp := storage.GroupPolicyDir(mnt)
	if err := os.MkdirAll(storage.StartupScriptDir(mnt), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(gp, "gpt.ini"):                                 []byte("[General]\r\ngPCMachineExtensionNames=[{827D319E-6EAC-11D2-A4EA-00C04F79F83A}{803E14A0-B4FB-11D0-A0D0-00A0C90F574B}]\r\nVersion=7\r\n"),
		filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini"): scriptsINI,
		filepath.Join(gp, "Machine", "Registry.pol"):                 []byte("PReg"),
		filepath.Join(storage.StartupScriptDir(mnt), "corp.cmd"):     []byte("corp"),
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readPolicy(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// 适配是往镜像里追加一次性启动项，不能删掉镜像自带的本地组策略；gpt.ini 版本要在原值上递增，
// 否则 gpsvc 认为策略没变、不重新处理。
func TestInjectAdaptationMergesIntoTheImagesOwnPolicy(t *testing.T) {
	mnt := t.TempDir()
	writeImagePolicy(t, mnt, []byte("[Startup]\r\n0CmdLine=corp.cmd\r\n0Parameters=/q\r\n[Shutdown]\r\n0CmdLine=bye.cmd\r\n0Parameters=\r\n"))

	if err := injectAdaptation(mnt, storage.SuperAdaptationReq{AdaptScript: []byte("adapt"), MountScript: []byte("mount")}); err != nil {
		t.Fatal(err)
	}
	gp := storage.GroupPolicyDir(mnt)
	if readPolicy(t, filepath.Join(gp, "Machine", "Registry.pol")) != "PReg" || readPolicy(t, filepath.Join(storage.StartupScriptDir(mnt), "corp.cmd")) != "corp" {
		t.Fatal("the image's own policy files must survive")
	}
	ini := readPolicy(t, filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini"))
	for _, want := range []string{"0CmdLine=ndadapt.cmd", "1CmdLine=corp.cmd", "1Parameters=/q", "2CmdLine=ndmount.cmd", "[Shutdown]\r\n0CmdLine=bye.cmd"} {
		if !strings.Contains(ini, want) {
			t.Fatalf("scripts.ini = %q, want %q", ini, want)
		}
	}
	gpt := readPolicy(t, filepath.Join(gp, "gpt.ini"))
	if !strings.Contains(gpt, "Version=8") || !strings.Contains(gpt, "827D319E-6EAC-11D2-A4EA-00C04F79F83A") || !strings.Contains(gpt, storage.ScriptsCSEGUID) {
		t.Fatalf("gpt.ini = %q, want the image's CSEs kept, the scripts CSE added and the version bumped", gpt)
	}

	// 重跑注入不重复注册，版本照样递增。
	if err := injectAdaptation(mnt, storage.SuperAdaptationReq{AdaptScript: []byte("adapt"), MountScript: []byte("mount")}); err != nil {
		t.Fatal(err)
	}
	ini2 := readPolicy(t, filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini"))
	if strings.Count(ini2, "ndadapt.cmd") != 1 || strings.Count(ini2, "ndmount.cmd") != 1 || strings.Count(ini2, "corp.cmd") != 1 {
		t.Fatalf("re-inject duplicated entries: %q", ini2)
	}
	if gpt := readPolicy(t, filepath.Join(gp, "gpt.ini")); !strings.Contains(gpt, "Version=9") {
		t.Fatalf("gpt.ini = %q, want Version=9", gpt)
	}
}

// gpedit 写的 scripts.ini 是 UTF-16LE 带 BOM；按字节找 "[Startup]" 会落空，进而整份覆盖掉。
func TestInjectAdaptationReadsAUTF16ScriptsINI(t *testing.T) {
	mnt := t.TempDir()
	writeImagePolicy(t, mnt, utf16LE("[Startup]\r\n0CmdLine=corp.cmd\r\n0Parameters=\r\n"))

	if err := injectAdaptation(mnt, storage.SuperAdaptationReq{AdaptScript: []byte("adapt")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("scripts.ini must stay UTF-16LE: % x", raw[:min(len(raw), 8)])
	}
	text, _ := decodeINI(raw)
	if !strings.Contains(text, "0CmdLine=ndadapt.cmd") || !strings.Contains(text, "1CmdLine=corp.cmd") {
		t.Fatalf("scripts.ini = %q", text)
	}
}

func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
