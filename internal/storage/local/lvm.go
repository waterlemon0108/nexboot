package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 根在 LVM 上的镜像不激活卷组读取：所有克隆的 VG 同名，可能还与服务器自己的根（"ubuntu-vg"）同名，
// 激活会冲突，改名又会改掉客户机开机依赖的元数据。做法是用限定在这块盘上的 lvs/pvs 读布局，
// 再用 dmsetup 以自定名字手工映射每个逻辑卷。只支持 linear 段（默认安装即如此）。

const lvmMember = "LVM2_member"

func hasLVM(parts []inspectPartition) bool {
	for _, p := range parts {
		if p.FSType == lvmMember {
			return true
		}
	}
	return false
}

// lvmConfig 把 LVM 命令限定在给定的物理卷上。
func lvmConfig(pvs []string) string {
	accept := make([]string, 0, len(pvs)+1)
	for _, pv := range pvs {
		accept = append(accept, fmt.Sprintf(`"a|^%s$|"`, regexp.QuoteMeta(pv)))
	}
	list := "[ " + strings.Join(append(accept, `"r|.*|"`), ", ") + " ]"
	return "devices { filter = " + list + " global_filter = " + list + " }"
}

type lvSegment struct {
	vg, lv            string
	start, size       int64 // 扇区
	segType, pv       string
	firstPE, extentSz int64
}

// mapLVM 映射 parts 中 LVM 成员上的逻辑卷，作为待挂载分区返回。映射可写：脏 ext4 日志必须回放才能挂载。
// release 删除映射，须在其上的挂载全部卸掉后调用。
func mapLVM(ctx context.Context, r runner, parts []inspectPartition) ([]inspectPartition, func(), error) {
	var pvs []string
	for _, p := range parts {
		if p.FSType == lvmMember {
			pvs = append(pvs, p.Path)
		}
	}
	var created []string
	release := func() {
		cctx, cancel := cleanupContext()
		defer cancel()
		for _, name := range created {
			if err := removeMapping(cctx, r, name); err != nil {
				slog.Warn("dmsetup remove failed; mapping leaked", "name", name, "error", err)
			}
		}
	}
	if len(pvs) == 0 {
		return nil, release, nil
	}
	cfg := lvmConfig(pvs)
	peStart := map[string]int64{}
	out, err := r.Run(ctx, "pvs", "--readonly", "--config", cfg, "--noheadings", "--nosuffix", "--units", "s",
		"--separator", "|", "-o", "pv_name,pe_start")
	if err != nil {
		return nil, release, fmt.Errorf("pvs failed: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) == 2 {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				peStart[f[0]] = n
			}
		}
	}
	out, err = r.Run(ctx, "lvs", "--readonly", "--config", cfg, "--noheadings", "--nosuffix", "--units", "s",
		"--separator", "|", "-o", "vg_name,lv_name,seg_start,seg_size,segtype,seg_pe_ranges,vg_extent_size")
	if err != nil {
		return nil, release, fmt.Errorf("lvs failed: %w", err)
	}
	var order []string
	segs := map[string][]lvSegment{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		seg, err := parseLVSegment(line)
		if errors.Is(err, errNotLVSegment) {
			continue // 夹在输出行里的 lvm 告警
		}
		if err != nil {
			return nil, release, err
		}
		key := seg.vg + "/" + seg.lv
		if seg.segType != "linear" {
			return nil, release, fmt.Errorf("逻辑卷 %s 是 %s 类型，目前只支持普通（linear）逻辑卷", key, seg.segType)
		}
		if _, ok := peStart[seg.pv]; !ok {
			return nil, release, fmt.Errorf("逻辑卷 %s 用到了这块盘以外的物理卷 %s", key, seg.pv)
		}
		if _, seen := segs[key]; !seen {
			order = append(order, key)
		}
		segs[key] = append(segs[key], seg)
	}
	var mapped []inspectPartition
	for _, key := range order {
		var table strings.Builder
		for _, s := range segs[key] {
			fmt.Fprintf(&table, "%d %d linear %s %d\n", s.start, s.size, s.pv, peStart[s.pv]+s.firstPE*s.extentSz)
		}
		name := "nd-" + randomTag() + "-" + dmSafe(segs[key][0].lv)
		file, err := writeTempTable(table.String())
		if err != nil {
			return nil, release, err
		}
		out, err := r.Run(ctx, "dmsetup", "create", name, file)
		_ = os.Remove(file)
		if err != nil {
			return nil, release, fmt.Errorf("映射逻辑卷 %s 失败：%w：%s", key, err, strings.TrimSpace(string(out)))
		}
		created = append(created, name)
		_, _ = r.Run(ctx, "dmsetup", "mknodes", name)
		dev := "/dev/mapper/" + name
		fs, _ := r.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
		mapped = append(mapped, inspectPartition{Path: dev, FSType: strings.TrimSpace(string(fs))})
	}
	return mapped, release, nil
}

// removeMapping 删除一个 dm 映射。仍被占用（懒卸载的挂载）时改为延迟删除：最后一个使用者离开时
// 内核自动删，底下的克隆才能销毁。
func removeMapping(ctx context.Context, r runner, name string) error {
	if _, err := r.Run(ctx, "dmsetup", "remove", "--retry", name); err == nil {
		return nil
	}
	if out, err := r.Run(ctx, "dmsetup", "remove", "--deferred", name); err != nil {
		return fmt.Errorf("dmsetup remove %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parseLVSegment 解析一行 lvs 输出：
// vg|lv|seg_start|seg_size|segtype|pv:firstPE-lastPE|extent_size
func parseLVSegment(line string) (lvSegment, error) {
	f := strings.Split(line, "|")
	if len(f) != 7 {
		return lvSegment{}, errNotLVSegment
	}
	s := lvSegment{vg: f[0], lv: f[1], segType: f[4]}
	var err error
	if s.start, err = strconv.ParseInt(f[2], 10, 64); err != nil {
		return lvSegment{}, fmt.Errorf("lvs seg_start %q: %w", f[2], err)
	}
	if s.size, err = strconv.ParseInt(f[3], 10, 64); err != nil {
		return lvSegment{}, fmt.Errorf("lvs seg_size %q: %w", f[3], err)
	}
	if s.extentSz, err = strconv.ParseInt(f[6], 10, 64); err != nil {
		return lvSegment{}, fmt.Errorf("lvs extent size %q: %w", f[6], err)
	}
	if s.segType != "linear" {
		return s, nil
	}
	pv, rng, ok := strings.Cut(f[5], ":")
	first, _, _ := strings.Cut(rng, "-")
	if !ok || strings.Contains(f[5], " ") {
		return lvSegment{}, fmt.Errorf("unexpected seg_pe_ranges %q", f[5])
	}
	s.pv = pv
	if s.firstPE, err = strconv.ParseInt(first, 10, 64); err != nil {
		return lvSegment{}, fmt.Errorf("seg_pe_ranges %q: %w", f[5], err)
	}
	return s, nil
}

func writeTempTable(table string) (string, error) {
	f, err := os.CreateTemp("", "ndiskless-dm-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(table); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func randomTag() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var dmUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func dmSafe(s string) string { return dmUnsafe.ReplaceAllString(s, "_") }

var errNotLVSegment = errors.New("not an lvs segment row")

var errLVMFilterCustomized = errors.New("lvm.conf already sets its own global_filter")

const zvolLVMFilter = "\t# ndiskless: 不扫描 zvol，客户机盘里的卷组不该被服务器自己激活\n\tglobal_filter = [ \"r|^/dev/zd.*|\" ]\n"

// EnsureZvolLVMFilter 让服务器的 LVM 不扫描 zvol。每个克隆都带镜像的卷组，放任 udev 激活会占住克隆、
// 并撞上几十个同名卷组。已有人设置的 global_filter 只报告不改写。
func EnsureZvolLVMFilter(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	conf := string(b)
	if strings.Contains(conf, `r|^/dev/zd.*|`) {
		return false, nil
	}
	if regexp.MustCompile(`(?m)^[ \t]*global_filter[ \t]*=`).MatchString(conf) {
		return false, errLVMFilterCustomized
	}
	if loc := regexp.MustCompile(`(?m)^devices[ \t]*\{[ \t]*\n`).FindStringIndex(conf); loc != nil {
		conf = conf[:loc[1]] + zvolLVMFilter + conf[loc[1]:]
	} else {
		if conf != "" && !strings.HasSuffix(conf, "\n") {
			conf += "\n"
		}
		conf += "devices {\n" + zvolLVMFilter + "}\n"
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".ndiskless")
	if err := os.WriteFile(tmp, []byte(conf), info.Mode().Perm()); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
