package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// ScratchSweep 是 SweepScratch 清掉的崩溃残留，供启动日志使用。
type ScratchSweep struct {
	Unmounted []string // 挂载点
	Mappings  []string // dmsetup 映射名
	Clones    []string // 临时克隆数据集
	Snapshots []string // 临时快照
}

// Empty 表示没有任何残留。
func (s ScratchSweep) Empty() bool {
	return len(s.Unmounted)+len(s.Mappings)+len(s.Clones)+len(s.Snapshots) == 0
}

// scratchMappingRe 只认 mapLVM 起的名字（nd-<8 位随机>-<lv>），宿主机自己的映射不碰。
var scratchMappingRe = regexp.MustCompile(`^nd-[0-9a-f]{8}-`)

// SweepScratch 清掉进程崩溃留下的体检、导出、适配回读临时克隆及其挂载和逻辑卷映射。
// 只能在启动时、任何体检/导出/适配回读开始之前调用：此刻这些对象一定没有主人。
// 顺序是卸挂载 → 删映射 → 销毁克隆 → 删临时快照，前一步占着后一步。尽力而为，出错继续并合并返回。
func (a *Agent) SweepScratch(ctx context.Context) (ScratchSweep, error) {
	r := a.runner
	if r == nil {
		r = execRunner{}
	}
	var out ScratchSweep
	var errs []error

	for _, m := range parseProcMounts(a.procMounts()) {
		name, ok := strings.CutPrefix(m.device, "/dev/mapper/")
		if !ok || !scratchMappingRe.MatchString(name) {
			continue
		}
		if detach(ctx, r, m.mountpoint) {
			_ = os.Remove(m.mountpoint)
			out.Unmounted = append(out.Unmounted, m.mountpoint)
		} else {
			errs = append(errs, fmt.Errorf("unmount %s failed", m.mountpoint))
		}
	}

	list, err := r.Run(ctx, "dmsetup", "ls")
	if errors.Is(err, exec.ErrNotFound) {
		list, err = nil, nil // 没装 dmsetup 就不会有我们建的映射
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("dmsetup ls: %w: %s", err, strings.TrimSpace(string(list))))
	}
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !scratchMappingRe.MatchString(fields[0]) {
			continue
		}
		if err := removeMapping(ctx, r, fields[0]); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Mappings = append(out.Mappings, fields[0])
	}

	datasets, err := a.zfs.ListDatasets(ctx)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	var doomed []string
	for _, dataset := range datasets {
		if a.scratchClone(dataset) {
			doomed = append(doomed, dataset)
		}
	}
	for _, m := range a.zvolMounts() {
		for _, d := range doomed {
			if m.Dataset == d {
				out.Unmounted = append(out.Unmounted, m.Mountpoint)
			}
		}
	}
	a.releaseStaleMounts(ctx, doomed)
	for _, dataset := range doomed {
		if err := a.destroyClone(ctx, dataset); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Clones = append(out.Clones, dataset)
	}

	snaps, err := a.zfs.ListSnapshots(ctx)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	for _, snap := range snaps {
		dataset, name, ok := strings.Cut(snap, "@")
		if !ok || name != storage.AdaptCheckSnapshot {
			continue
		}
		if !a.inRun(dataset) || storage.Classify(datasetName(dataset)).Kind != storage.CarrierSuper {
			continue
		}
		if err := a.zfs.DestroySnapshot(ctx, dataset, name); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	return out, errors.Join(errs...)
}

// scratchClone 判断数据集是否为 run/ 下的体检、导出或适配回读临时克隆。
func (a *Agent) scratchClone(dataset string) bool {
	if !a.inRun(dataset) {
		return false
	}
	kind := storage.Classify(datasetName(dataset)).Kind
	return kind == storage.CarrierInspect || kind == storage.CarrierExport
}

// inRun 判断数据集是否直接位于本池的 run/ 下；目录容器 nd/ 会被复制，不在清扫范围内。
func (a *Agent) inRun(dataset string) bool {
	rest, ok := strings.CutPrefix(dataset, a.zfs.PoolName()+"/"+storage.RunRoot+"/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

func (a *Agent) procMounts() []byte {
	if a.procMountsFn != nil {
		return a.procMountsFn()
	}
	b, _ := os.ReadFile("/proc/self/mounts")
	return b
}

type procMount struct{ device, mountpoint string }

// parseProcMounts 只取设备和挂载点两列；我们的挂载点是 mkdtemp 路径，不含需要反转义的空白。
func parseProcMounts(data []byte) []procMount {
	var out []procMount
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			out = append(out, procMount{device: f[0], mountpoint: f[1]})
		}
	}
	return out
}
