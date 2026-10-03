package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/iscsi"
)

// 本地组策略的计算机启动脚本注册文件。gpsvc 开机即读取（无需登录）并以 SYSTEM 运行；
// 纯文件写入，避免离线改注册表 hive。
const (
	adaptScriptsINI   = "[Startup]\r\n0CmdLine=ndadapt.cmd\r\n0Parameters=\r\n"
	adaptLauncherName = "ndadapt.cmd"
	// scriptsCSEPair 是 gPCMachineExtensionNames 里 Scripts 扩展及其管理单元的那一组。
	scriptsCSEPair = "[{" + storage.ScriptsCSEGUID + "}{40B6664F-4972-11D1-A7CA-0000F87571E3}]"
	// adapt.ps1 故意同步执行：驱动安装必须在有人登录前完成；挂载启动器则是脱离运行。
	adaptCmd = "@echo off\r\npowershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0adapt.ps1\"\r\n"
)

// PrepareSuperAdaptation 向超管机持久克隆写入一次性自动适配：确保 SCLIENT 克隆存在，
// 放入驱动包和适配脚本，并注册为本地组策略启动脚本，下次开机由 Windows 以 SYSTEM 运行一次。
func (a *Agent) PrepareSuperAdaptation(ctx context.Context, req storage.SuperAdaptationReq) error {
	if req.MAC == "" || req.Source.ConfigID == "" || req.Source.SnapshotName == "" {
		return fmt.Errorf("mac, config and snapshot are required")
	}
	if len(req.AdaptScript) == 0 {
		return fmt.Errorf("adapt script is required")
	}
	// 与开机供给、超管保存、数据盘发布共用一把锁，内部只调无锁实现。
	defer a.lockClient(req.MAC)()
	r := a.runner
	if r == nil {
		r = execRunner{}
	}

	// 已有克隆则复用，开机前重跑会注入到同一块盘。
	dataset := a.zfs.Dataset(storage.SuperClientCloneName(req.MAC))
	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(datasets))
	for _, dataset := range datasets {
		existing[dataset] = true
	}
	if existing[dataset] && req.ResetClone {
		if err := a.resetSuperSystemClone(ctx, req.MAC, datasets); err != nil {
			return err
		}
		existing[dataset] = false
	}
	if existing[dataset] {
		origins, err := a.zfs.ListOrigins(ctx)
		if err != nil {
			return err
		}
		if err := a.checkSuperSource(origins, req.MAC, 0, dataset, req.Source.ConfigID, false); err != nil {
			return err
		}
	} else {
		if err := a.zfs.Clone(ctx, a.zfs.Snapshot(req.Source.ConfigID, req.Source.SnapshotName), dataset); err != nil {
			return err
		}
	}
	return withWindowsPartition(ctx, r, a.zfs.VolumePath(dataset), volRW, clonePartitionWait, func(mnt string) error {
		return injectAdaptation(mnt, req)
	})
}

func (a *Agent) resetSuperSystemClone(ctx context.Context, mac string, datasets []string) error {
	system := a.zfs.Dataset(storage.SuperClientCloneName(mac))
	if a.exporter != nil {
		luns := []int{}
		for _, dataset := range superClientCloneDatasets(a.zfs, mac, datasets) {
			if lun, ok := superClientCloneLUN(a.zfs, mac, dataset); ok {
				luns = append(luns, lun)
			}
		}
		if err := a.exporter.TeardownTarget(ctx, iscsi.TargetForMAC(mac), luns); err != nil {
			return err
		}
	}
	// 用 destroyClone 而非直接 Destroy：刚拆完 target，udev 重新探测期间删除会短暂 busy。
	return a.destroyClone(ctx, system)
}

// injectAdaptation 把驱动包、适配脚本和组策略启动项写入已挂载的 Windows 分区。
func injectAdaptation(mnt string, req storage.SuperAdaptationReq) error {
	ndadapt := filepath.Join(mnt, "ndadapt")
	if err := os.RemoveAll(ndadapt); err != nil {
		return err
	}
	if err := os.MkdirAll(ndadapt, 0o755); err != nil {
		return err
	}
	if len(req.BundleZip) > 0 {
		if err := os.WriteFile(filepath.Join(ndadapt, "bundle.zip"), req.BundleZip, 0o644); err != nil {
			return err
		}
	}

	// 只合并不重置：镜像自带的本地组策略（别的启动脚本、Registry.pol、其它 CSE）都要保留。
	startup := storage.StartupScriptDir(mnt)
	if err := os.MkdirAll(startup, 0o755); err != nil {
		return err
	}
	writes := map[string][]byte{
		filepath.Join(startup, "adapt.ps1"):       toCRLF(req.AdaptScript),
		filepath.Join(startup, adaptLauncherName): []byte(adaptCmd),
	}
	if len(req.MountScript) > 0 {
		writes[filepath.Join(startup, storage.MountScriptName)] = toCRLF(req.MountScript)
		writes[filepath.Join(startup, storage.MountLauncherName)] = []byte(storage.MountLauncherBody)
	}
	for path, content := range writes {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return err
		}
	}
	if err := editINI(filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini"), func(text string) string {
		return registerAdaptStartup(text, len(req.MountScript) > 0)
	}); err != nil {
		return err
	}
	return editINI(filepath.Join(storage.GroupPolicyDir(mnt), "gpt.ini"), registerScriptsCSE)
}

// editINI 按原编码读改写 ini；gpedit 写的是带 BOM 的 UTF-16LE，按字节处理会找不到段名而整份覆盖。
func editINI(path string, edit func(string) string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text, wide := decodeINI(b)
	return os.WriteFile(path, encodeINI(edit(text), wide), 0o644)
}

func decodeINI(b []byte) (string, bool) {
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		return string(b), false
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units)), true
}

func encodeINI(s string, wide bool) []byte {
	if !wide {
		return []byte(s)
	}
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

var (
	startupEntryRe = regexp.MustCompile(`(?i)^\s*(\d+)(CmdLine|Parameters)=(.*)$`)
	iniSectionRe   = regexp.MustCompile(`^\s*\[`)
)

// registerAdaptStartup 把 ndadapt.cmd 注册为 [Startup] 第 0 项（驱动要在其余启动脚本之前装好），
// 镜像原有项顺延；带挂载脚本且未注册时 ndmount.cmd 排最后。重跑不重复注册，其它段原样保留。
func registerAdaptStartup(content string, withMount bool) string {
	type entry struct{ cmd, params string }
	lines := strings.Split(strings.ReplaceAll(string(toCRLF([]byte(content))), "\r\n", "\n"), "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if start < 0 && strings.EqualFold(strings.TrimSpace(line), "[Startup]") {
			start = i
			continue
		}
		if start >= 0 && iniSectionRe.MatchString(line) {
			end = i
			break
		}
	}
	var existing []entry
	if start >= 0 {
		byNum := map[int]*entry{}
		var nums []int
		for _, line := range lines[start+1 : end] {
			m := startupEntryRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
			if m == nil {
				continue
			}
			n, _ := strconv.Atoi(m[1])
			if byNum[n] == nil {
				byNum[n] = &entry{}
				nums = append(nums, n)
			}
			if strings.EqualFold(m[2], "CmdLine") {
				byNum[n].cmd = m[3]
			} else {
				byNum[n].params = m[3]
			}
		}
		slices.Sort(nums)
		for _, n := range nums {
			if e := byNum[n]; e.cmd != "" && !strings.EqualFold(strings.TrimSpace(e.cmd), adaptLauncherName) {
				existing = append(existing, *e)
			}
		}
	}
	entries := append([]entry{{cmd: adaptLauncherName}}, existing...)
	if withMount && !slices.ContainsFunc(existing, func(e entry) bool {
		return strings.EqualFold(strings.TrimSpace(e.cmd), storage.MountLauncherName)
	}) {
		entries = append(entries, entry{cmd: storage.MountLauncherName})
	}
	section := []string{"[Startup]"}
	for i, e := range entries {
		section = append(section, fmt.Sprintf("%dCmdLine=%s", i, e.cmd), fmt.Sprintf("%dParameters=%s", i, e.params))
	}
	var out []string
	if start < 0 {
		out = append(section, lines...)
	} else {
		out = append(append(append([]string{}, lines[:start]...), section...), lines[end:]...)
	}
	text := strings.Join(out, "\r\n")
	text = strings.TrimRight(text, "\r\n") + "\r\n"
	return text
}

var (
	gptExtensionsRe = regexp.MustCompile(`(?mi)^(\s*gPCMachineExtensionNames=)([^\r\n]*)`)
	gptGroupRe      = regexp.MustCompile(`\[[^\]]*\]`)
	gptGeneralRe    = regexp.MustCompile(`(?mi)^\s*\[General\][^\r\n]*(\r?\n)?`)
)

// registerScriptsCSE 确保 gpt.ini 注册了 Scripts 扩展（保留镜像已有的其它扩展）并递增版本。
// 扩展组按 GUID 排序：顺序不对时 gpsvc 可能跳过其中的扩展。
func registerScriptsCSE(content string) string {
	if strings.TrimSpace(content) == "" {
		return storage.GroupPolicyINI
	}
	if !strings.Contains(strings.ToUpper(content), storage.ScriptsCSEGUID) {
		if loc := gptExtensionsRe.FindStringSubmatchIndex(content); loc != nil {
			groups := append(gptGroupRe.FindAllString(content[loc[4]:loc[5]], -1), scriptsCSEPair)
			slices.SortFunc(groups, func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) })
			content = content[:loc[4]] + strings.Join(groups, "") + content[loc[5]:]
		} else {
			content = insertUnderGeneral(content, "gPCMachineExtensionNames="+scriptsCSEPair)
		}
	}
	if gptVersionRe.MatchString(content) {
		return bumpGptVersion(content)
	}
	return insertUnderGeneral(content, "Version=65539")
}

// insertUnderGeneral 把一行加到 [General] 段首；没有该段就新建在文件开头。
func insertUnderGeneral(content, line string) string {
	if loc := gptGeneralRe.FindStringIndex(content); loc != nil {
		head := content[:loc[1]]
		if !strings.HasSuffix(head, "\n") {
			head += "\r\n"
		}
		return head + line + "\r\n" + content[loc[1]:]
	}
	return "[General]\r\n" + line + "\r\n" + content
}

// toCRLF 统一为 Windows 换行，否则注入的脚本运行会出错。
func toCRLF(b []byte) []byte {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return []byte(strings.ReplaceAll(s, "\n", "\r\n"))
}

// injectMountScript 把盘符挂载启动脚本合并进系统克隆的本地组策略启动项，每次开机执行。
// 只合并不重置组策略目录：已放入的 adapt.ps1 和镜像自带项都要保留。
func (a *Agent) injectMountScript(ctx context.Context, dev string, script []byte) error {
	return a.mergeMountScriptInto(ctx, dev, script, clonePartitionWait)
}

// bakeMountScript 在导入时对新镜像做同样的合并，只是等待更久：没人在等，
// 而这里放弃意味着之后每次开机都要再注入。
func (a *Agent) bakeMountScript(ctx context.Context, dev string, script []byte) error {
	return a.mergeMountScriptInto(ctx, dev, script, importPartitionWait)
}

func (a *Agent) mergeMountScriptInto(ctx context.Context, dev string, script []byte, wait time.Duration) error {
	if a.injectMountScriptFn != nil {
		return a.injectMountScriptFn(ctx, dev, script)
	}
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	return withWindowsPartition(ctx, r, dev, volRW, wait, func(mnt string) error {
		return mergeMountScript(mnt, script)
	})
}

func mergeMountScript(mnt string, script []byte) error {
	startup := storage.StartupScriptDir(mnt)
	if err := os.MkdirAll(startup, 0o755); err != nil {
		return err
	}
	// 脚本内容总是覆盖（服务端升级后重启即生效）；ini 注册幂等合并。
	if err := os.WriteFile(filepath.Join(startup, storage.MountScriptName), toCRLF(script), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(startup, storage.MountLauncherName), []byte(storage.MountLauncherBody), 0o644); err != nil {
		return err
	}
	if err := mergeScriptsINI(filepath.Join(storage.MachineScriptsDir(mnt), "scripts.ini")); err != nil {
		return err
	}
	// gpt.ini 必须含 Scripts CSE，gpsvc 才处理本地脚本；改了 scripts.ini 要递增 Version 强制重新处理。
	return editINI(filepath.Join(storage.GroupPolicyDir(mnt), "gpt.ini"), registerScriptsCSE)
}

var (
	scriptsCmdLineRe = regexp.MustCompile(`(?m)^\s*(\d+)CmdLine=`)
	gptVersionRe     = regexp.MustCompile(`(?mi)^(\s*Version=)(\d+)`)
)

// mergeScriptsINI 在保留已有项的前提下注册 ndmount.cmd 启动项。新项紧跟 [Startup] 标题插入
// （执行顺序看序号不看位置），避免被后面的 [Shutdown] 段吞掉。
func mergeScriptsINI(path string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(b)
	if strings.Contains(content, storage.MountLauncherName) {
		return nil
	}
	if !strings.Contains(content, "[Startup]") {
		return os.WriteFile(path, []byte("[Startup]\r\n0CmdLine="+storage.MountLauncherName+"\r\n0Parameters=\r\n"), 0o644)
	}
	next := 0
	for _, m := range scriptsCmdLineRe.FindAllStringSubmatch(content, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= next {
			next = n + 1
		}
	}
	entry := fmt.Sprintf("[Startup]\r\n%dCmdLine=%s\r\n%dParameters=", next, storage.MountLauncherName, next)
	content = strings.Replace(content, "[Startup]", entry, 1)
	return os.WriteFile(path, []byte(content), 0o644)
}

// bumpGptVersion 递增 gpt.ini 的 Version（低位字是计算机策略版本），让 gpsvc 重新处理。
// 按行首匹配：gPCFunctionalityVersion 不能跟着变。
func bumpGptVersion(content string) string {
	return gptVersionRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := gptVersionRe.FindStringSubmatch(m)
		n, err := strconv.Atoi(sub[2])
		if err != nil {
			return m
		}
		return sub[1] + strconv.Itoa(n+1)
	})
}

// ReadSuperAdaptationResult 在超管机自行关机后读取 C:\ndadapt\done.txt。
// SCLIENT 盘仍被 iSCSI 导出占用，所以读临时快照的克隆，既一致又不打扰在线盘。
func (a *Agent) ReadSuperAdaptationResult(ctx context.Context, mac string) (storage.SuperAdaptationResult, error) {
	defer a.lockClient(mac)()
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	dataset := a.zfs.Dataset(storage.SuperClientCloneName(mac))
	snap := dataset + "@" + storage.AdaptCheckSnapshot
	tmp := a.zfs.Dataset(storage.AdaptCheckCloneName(mac))
	// 清理残留的检查对象（先删克隆再删快照）；旧版本把克隆放在目录容器下，一并清掉。
	a.destroyDetached(tmp)
	a.destroyDetached(path.Join(a.zfs.PoolName(), storage.CatalogueRoot, storage.AdaptCheckCloneName(mac)))
	a.destroyDetached(snap)
	if err := a.zfs.SnapshotVolume(ctx, dataset, storage.AdaptCheckSnapshot); err != nil {
		return storage.SuperAdaptationResult{}, err
	}
	defer a.destroyDetached(snap) // 后进先出：在删 tmp 之后执行
	if err := a.zfs.Clone(ctx, snap, tmp); err != nil {
		return storage.SuperAdaptationResult{}, err
	}
	defer a.destroyDetached(tmp)

	var res storage.SuperAdaptationResult
	err := withWindowsPartition(ctx, r, a.zfs.VolumePath(tmp), volRO, clonePartitionWait, func(mnt string) error {
		res = readDoneMarker(mnt)
		return nil
	})
	// 找不到 Windows 分区对轮询方等同于「尚未完成」，与缺少 done 标记一样。
	if errors.Is(err, errNoWindowsPartition) {
		return storage.SuperAdaptationResult{}, nil
	}
	return res, err
}

func readDoneMarker(mnt string) storage.SuperAdaptationResult {
	b, err := os.ReadFile(filepath.Join(mnt, "ndadapt", "done.txt"))
	if err != nil {
		return storage.SuperAdaptationResult{} // 尚未完成
	}
	text := strings.TrimPrefix(string(b), "\ufeff") // 去掉 UTF-8 BOM
	text = strings.TrimSpace(text)
	return storage.SuperAdaptationResult{Done: true, OK: strings.Contains(text, "OK=True"), Log: text}
}
