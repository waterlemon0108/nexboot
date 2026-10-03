package local

import (
	"context"
	"fmt"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// SuperCloneForeign 表示超管机的持久克隆不是从要用（或要保存到）的配置克隆的：机器换了分组、
// 分组换了系统配置，或数据盘增删让 LUN 下标错位。复用会把别的配置的盘挂给机器，保存会把它
// 存进当前配置并删掉当前配置的历史。
type SuperCloneForeign struct {
	MAC    string // 规范化 MAC
	LUN    int    // 0 为系统盘
	Have   string // 克隆实际来自的配置；空为来源不明
	Want   string // 本次要用或要保存到的配置
	Saving bool
}

func (e SuperCloneForeign) Error() string {
	disk := "系统盘"
	if e.LUN > 0 {
		disk = fmt.Sprintf("数据盘（LUN %d）", e.LUN)
	}
	have := "来源不明的配置"
	back := "改回原来的配置"
	if e.Have != "" {
		have = "配置 " + e.Have
		back = "改回配置 " + e.Have
	}
	mac := displayMAC(e.MAC)
	if e.Saving {
		return fmt.Sprintf("请先把超管机 %s 所在分组%s再保存，或放弃这次超管修改：它的%s来自%s，"+
			"不能存进配置 %s，否则会用别的配置的盘替换 %s 并删掉它的还原点", mac, back, disk, have, e.Want, e.Want)
	}
	return fmt.Sprintf("请先把超管机 %s 所在分组%s，或在超管页放弃这台机器的修改后再开机：它的%s（含未保存的修改）来自%s，"+
		"分组现在要用配置 %s", mac, back, disk, have, e.Want)
}

// displayMAC 给规范化的 MAC 加冒号，与界面和设备标签上的写法一致。
func displayMAC(mac string) string {
	mac = storage.NormalizeMAC(mac)
	if len(mac) != 12 {
		return mac
	}
	parts := make([]string, 0, 6)
	for i := 0; i < len(mac); i += 2 {
		parts = append(parts, mac[i:i+2])
	}
	return strings.Join(parts, ":")
}

// superCloneOf 判断持久克隆是否属于 configDataset：它克隆自该配置的某个还原点（换了当前点也算），
// 或保存中断在 promote 之后、配置已挂到它下面。
func superCloneOf(origins map[string]string, clone, configDataset string) bool {
	if ds, _, ok := strings.Cut(origins[clone], "@"); ok && ds == configDataset {
		return true
	}
	ds, _, ok := strings.Cut(origins[configDataset], "@")
	return ok && ds == clone
}

// checkSuperSource 在复用或保存持久克隆前核对来源，不属于 configID 时返回 SuperCloneForeign。
func (a *Agent) checkSuperSource(origins map[string]string, mac string, lun int, clone, configID string, saving bool) error {
	if superCloneOf(origins, clone, a.zfs.Dataset(configID)) {
		return nil
	}
	have := ""
	if ds, _, ok := strings.Cut(origins[clone], "@"); ok {
		have = datasetName(ds)
	}
	return SuperCloneForeign{MAC: storage.NormalizeMAC(mac), LUN: lun, Have: have, Want: configID, Saving: saving}
}

// checkSuperStopSources 对 SuperStop 要保存的每块盘做 saveSuperClone 同样的来源核对，只读不改。
func (a *Agent) checkSuperStopSources(ctx context.Context, top *topology, mac string, req storage.SuperStopReq) error {
	type disk struct {
		lun          int
		clone, cfgID string
	}
	var disks []disk
	if strings.TrimSpace(req.ReductionName) != "" {
		disks = append(disks, disk{0, storage.SuperClientCloneName(mac), strings.TrimSpace(req.ConfigID)})
	}
	for _, d := range req.DataDisks {
		if strings.TrimSpace(d.ReductionName) != "" {
			disks = append(disks, disk{d.LUN, storage.SuperClientDataCloneName(mac, d.LUN), strings.TrimSpace(d.ConfigID)})
		}
	}
	var origins map[string]string
	for _, d := range disks {
		clone := a.zfs.Dataset(d.clone)
		// 与 saveSuperClone 一致：只有全新保存才核对，替换开始后（配置已挪开）只能继续。
		if d.cfgID == "" || !top.has(clone) || top.has(a.zfs.Dataset(storage.SupersededConfigName(d.cfgID))) {
			continue
		}
		if origins == nil {
			var err error
			if origins, err = a.zfs.ListOrigins(ctx); err != nil {
				return err
			}
		}
		if err := a.checkSuperSource(origins, mac, d.lun, clone, d.cfgID, true); err != nil {
			return err
		}
	}
	return nil
}
