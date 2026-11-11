package ops

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// 镜像行的状态：空表示和主机一致。
const (
	VolumeStatePendingDelete = "pending_delete" // 主机已删，这台等下一轮复制跟随
	VolumeStatePendingCopy   = "pending_copy"   // 主机有，这台还没收到
	VolumeStateBehind        = "behind"         // 业务快照和主机不一致
	VolumeStateMissing       = "missing"        // 库里有，这台的池里没有
	VolumeStateImporting     = "importing"      // 正在导入，库行还没写
)

// 备份副本里的成员和数据池比较的结果。
const (
	BackupItemSame    = "same"
	BackupItemRemoved = "removed" // 数据池已删，下一轮备份时移除
	BackupItemNewer   = "newer"   // 数据池有变化，等下一轮备份
)

// replicationAliveWindow 内刷新过的备机才算在跟随，与计划切换的口径一致。
const replicationAliveWindow = 3 * time.Minute

// VolumeCategory 是页面上的一个类别，合计等于池的已用。
type VolumeCategory struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
	Count int    `json:"count"`
}

type VolumeImage struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Dataset   string     `json:"dataset"`
	Purpose   string     `json:"purpose,omitempty"`
	Used      int64      `json:"used"`
	Size      int64      `json:"size"`
	Configs   int        `json:"configs"`
	Points    int        `json:"points"`
	Users     []string   `json:"users"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	State     string     `json:"state,omitempty"`
}

type VolumeFile struct {
	Name       string     `json:"name"`
	Size       int64      `json:"size"`
	Used       int64      `json:"used"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	// State 只有待复制删除：主机已删，这台下一轮同步才跟着删。
	State string `json:"state,omitempty"`
}

type VolumeClientDisk struct {
	Dataset string `json:"dataset"`
	Kind    string `json:"kind"`
	LUN     int    `json:"lun"`
	Used    int64  `json:"used"`
	Source  string `json:"source,omitempty"`
}

// VolumeClient 把一台机器的盘放一行；MAC 为空的一行是没有机器的临时克隆。
type VolumeClient struct {
	MAC       string             `json:"mac"`
	Name      string             `json:"name"`
	Known     bool               `json:"known"`
	State     string             `json:"state,omitempty"`
	IsSuper   bool               `json:"is_super"`
	OfflineAt *time.Time         `json:"offline_at,omitempty"`
	Used      int64              `json:"used"`
	Disks     []VolumeClientDisk `json:"disks"`
}

// VolumeResidue 是残留。Unique 列出当前目录里没有的还原点，但分不清是别处没有的内容还是早已删掉的。
type VolumeResidue struct {
	Dataset     string     `json:"dataset"`
	Kind        string     `json:"kind"`
	Used        int64      `json:"used"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	Points      int        `json:"points"`
	Unique      []string   `json:"unique,omitempty"`
	Dependents  []string   `json:"dependents,omitempty"`
	HasBaseline bool       `json:"has_baseline,omitempty"`
}

type VolumeSystemItem struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Path  string `json:"path"`
	Used  int64  `json:"used"`
	Count int    `json:"count,omitempty"`
}

type VolumeBackupItem struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Dataset   string     `json:"dataset"`
	Purpose   string     `json:"purpose,omitempty"`
	Used      int64      `json:"used"`
	Size      int64      `json:"size"`
	Configs   int        `json:"configs"`
	Points    int        `json:"points"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	State     string     `json:"state"`
}

// VolumeBackup 是备份池里的整份副本。
type VolumeBackup struct {
	Present      bool               `json:"present"`
	Used         int64              `json:"used"`
	SystemUsed   int64              `json:"system_used"`
	LastBackupAt *time.Time         `json:"last_backup_at,omitempty"`
	Pending      bool               `json:"pending"`
	Items        []VolumeBackupItem `json:"items"`
}

// VolumeSync 是这个池的复制状态。Level 为 ok、warn 或 error，正常时页面只显示一行小字。
type VolumeSync struct {
	Level   string `json:"level"`
	Message string `json:"message,omitempty"`
	Pending int    `json:"pending"`
}

type VolumeUnused struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// PoolVolumesResult 回答一个池里有什么、属于谁。Reachable 为 false 时只有 Error 有意义：读不到不等于没有。
type PoolVolumesResult struct {
	Node          string             `json:"node"`
	IP            string             `json:"ip"`
	NodeRole      string             `json:"node_role"`
	Pool          string             `json:"pool"`
	Role          string             `json:"role"`
	Reachable     bool               `json:"reachable"`
	Error         string             `json:"error,omitempty"`
	Authoritative bool               `json:"authoritative"`
	Used          int64              `json:"used"`
	Available     int64              `json:"available"`
	Freeing       int64              `json:"freeing"`
	Sync          VolumeSync         `json:"sync"`
	Categories    []VolumeCategory   `json:"categories"`
	Unused        VolumeUnused       `json:"unused"`
	Images        []VolumeImage      `json:"images"`
	Imports       []VolumeFile       `json:"imports"`
	FilesReadable bool               `json:"files_readable"`
	Clients       []VolumeClient     `json:"clients"`
	Residues      []VolumeResidue    `json:"residues"`
	System        []VolumeSystemItem `json:"system"`
	Backup        *VolumeBackup      `json:"backup,omitempty"`
}

// PoolVolumeService 把节点上报的事实和库对上。节点只报事实，归类都在这里做：备机的库不权威。
type PoolVolumeService struct {
	Store    store.Store
	NodeID   string
	DataPool string
	SelfURL  string
	// Facts 读本节点；Fetch 读别的节点，单机时为 nil。
	Facts func(ctx context.Context, pool string) (storage.PoolFacts, error)
	Fetch func(ctx context.Context, apiURL, pool string) (storage.PoolFacts, error)
	// Writer 回答本节点现在是否写入者；nil 表示单机。
	Writer  func() bool
	Now     func() time.Time
	Timeout time.Duration
}

// volumeCatalogue 是一次读出的库：配置归属、镜像使用者和客户机。
type volumeCatalogue struct {
	images    []domain.Image
	configs   map[string][]domain.Config
	imageOf   map[string]domain.Image
	points    map[string]int
	users     map[string][]string
	terminals map[string]domain.Terminal
}

func (s PoolVolumeService) List(ctx context.Context, node, pool string) (PoolVolumesResult, error) {
	pool = strings.TrimSpace(pool)
	if pool == "" {
		return PoolVolumesResult{}, errs.Invalid("要看哪个存储池？")
	}
	if node == "" {
		node = s.NodeID
	}
	servers, err := s.Store.Servers().List(ctx)
	if err != nil {
		return PoolVolumesResult{}, err
	}
	res := PoolVolumesResult{Node: node, Pool: pool, Sync: VolumeSync{Level: "ok"}, Images: []VolumeImage{}, Imports: []VolumeFile{},
		Clients: []VolumeClient{}, Residues: []VolumeResidue{}, System: []VolumeSystemItem{}, Categories: []VolumeCategory{}}
	var target *domain.Server
	clustered := false
	for i := range servers {
		if servers[i].ID == node {
			target = &servers[i]
			res.IP = servers[i].IP
		}
		if servers[i].ID != s.NodeID {
			clustered = true
		}
	}
	if target == nil && node != s.NodeID {
		return PoolVolumesResult{}, errs.NotFound("节点不存在：" + node)
	}
	writer := s.Writer == nil || s.Writer()
	res.Authoritative = writer
	res.NodeRole = nodeRoleOf(clustered, node == s.NodeID, writer, target)

	facts, err := s.read(ctx, node, target, pool)
	if err != nil {
		who := res.IP
		if who == "" {
			who = "这台服务器"
		}
		res.Error = fmt.Sprintf("%s 没有应答，看不到这个池里有什么（这不代表池里没有卷）：%v", who, err)
		return res, nil
	}
	res.Reachable = true
	res.Used, res.Available, res.Freeing = facts.Used, facts.Available, facts.Freeing

	catalogue, err := s.loadCatalogue(ctx)
	if err != nil {
		return PoolVolumesResult{}, err
	}
	if facts.Pool == facts.DataPool {
		res.Role = PoolRoleData
		err = s.buildDataPool(ctx, &res, facts, catalogue, servers, clustered, node, writer)
	} else {
		res.Role = PoolRoleBackup
		s.buildBackupPool(ctx, &res, facts, catalogue, node, target)
	}
	return res, err
}

func nodeRoleOf(clustered, self, writer bool, target *domain.Server) string {
	switch {
	case !clustered:
		return "single"
	case self && writer:
		return "writer"
	case self:
		return "standby"
	case !writer && target != nil && target.HAState == "active":
		return "writer"
	}
	return "standby"
}

func (s PoolVolumeService) read(ctx context.Context, node string, target *domain.Server, pool string) (storage.PoolFacts, error) {
	if node == s.NodeID {
		return s.Facts(ctx, pool)
	}
	if s.Fetch == nil || target == nil || target.APIURL == "" {
		return storage.PoolFacts{}, fmt.Errorf("节点地址未知")
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	nctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	facts, err := s.Fetch(nctx, target.APIURL, pool)
	if err != nil {
		return storage.PoolFacts{}, err
	}
	// 花名册地址可能已指向另一台机器，不能把别人的池当成这台的。
	if facts.Node != "" && facts.Node != node {
		return storage.PoolFacts{}, fmt.Errorf("这个地址上现在是另一台机器（%s）", facts.Node)
	}
	return facts, nil
}

func (s PoolVolumeService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s PoolVolumeService) loadCatalogue(ctx context.Context) (volumeCatalogue, error) {
	c := volumeCatalogue{configs: map[string][]domain.Config{}, imageOf: map[string]domain.Image{}, points: map[string]int{},
		users: map[string][]string{}, terminals: map[string]domain.Terminal{}}
	var err error
	if c.images, err = s.Store.Images().List(ctx); err != nil {
		return c, err
	}
	for _, img := range c.images {
		configs, err := s.Store.Configs().ListByImage(ctx, img.ID)
		if err != nil {
			return c, err
		}
		c.configs[img.ID] = configs
		for _, cfg := range configs {
			c.imageOf[cfg.ID] = img
			reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
			if err != nil {
				return c, err
			}
			c.points[img.ID] += len(reductions)
		}
	}
	terminals, err := s.Store.Terminals().List(ctx)
	if err != nil {
		return c, err
	}
	for _, t := range terminals {
		c.terminals[storage.NormalizeMAC(t.MAC)] = t
	}
	return c, s.loadUsers(ctx, &c, terminals)
}

// loadUsers 算出每个镜像被谁用着。写法和删除守卫一致，但这里只用来显示，真正的判定仍在删除时。
func (s PoolVolumeService) loadUsers(ctx context.Context, c *volumeCatalogue, terminals []domain.Terminal) error {
	groups, err := s.Store.Groups().List(ctx)
	if err != nil {
		return err
	}
	disks, err := s.Store.GroupDisks().List(ctx)
	if err != nil {
		return err
	}
	clones, err := s.Store.ClientClones().List(ctx)
	if err != nil {
		return err
	}
	groupImages := map[string]map[string]bool{}
	groupNames := map[string]string{}
	for _, g := range groups {
		groupNames[g.ID] = g.Name
		groupImages[g.ID] = map[string]bool{}
		if g.SystemImageID != "" {
			groupImages[g.ID][g.SystemImageID] = true
			c.users[g.SystemImageID] = append(c.users[g.SystemImageID], "分组 "+g.Name)
		}
	}
	for _, d := range disks {
		if groupImages[d.GroupID] != nil {
			groupImages[d.GroupID][d.ImageID] = true
		}
		c.users[d.ImageID] = append(c.users[d.ImageID], "分组 "+groupNames[d.GroupID]+" 的 "+d.MountTarget)
	}
	for _, t := range terminals {
		if !t.IsSuper {
			continue
		}
		for imageID := range groupImages[t.GroupID] {
			c.users[imageID] = append(c.users[imageID], "超管机 "+terminalLabel(t))
		}
	}
	seen := map[string]bool{}
	for _, clone := range clones {
		img, ok := c.imageOf[clone.ConfigID]
		if !ok {
			continue
		}
		t, known := c.terminals[storage.NormalizeMAC(clone.TerminalMAC)]
		if known && t.IsSuper {
			continue // 已经写过超管机
		}
		label := "客户机 " + clone.TerminalMAC
		if known {
			label = "客户机 " + terminalLabel(t)
		}
		if key := img.ID + "|" + label; !seen[key] {
			seen[key] = true
			c.users[img.ID] = append(c.users[img.ID], label)
		}
	}
	return nil
}

func terminalLabel(t domain.Terminal) string {
	if strings.TrimSpace(t.Name) != "" {
		return t.Name
	}
	return t.MAC
}

func datasetsByName(facts storage.PoolFacts) map[string]storage.PoolDatasetFact {
	out := make(map[string]storage.PoolDatasetFact, len(facts.Datasets))
	for _, d := range facts.Datasets {
		out[d.Name] = d
	}
	return out
}

// businessGUIDs 取业务快照的 GUID。rep-* 每轮都变，比进去每一行都会闪。
func businessGUIDs(d storage.PoolDatasetFact) map[string]bool {
	out := map[string]bool{}
	for _, snap := range d.Snapshots {
		if !storage.SystemSnapshot(snap.Name) {
			out[snap.GUID] = true
		}
	}
	return out
}

func sameGUIDs(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func hasSnapshot(d storage.PoolDatasetFact, name string) bool {
	for _, snap := range d.Snapshots {
		if snap.Name == name {
			return true
		}
	}
	return false
}

func (s PoolVolumeService) buildDataPool(ctx context.Context, res *PoolVolumesResult, facts storage.PoolFacts, c volumeCatalogue,
	servers []domain.Server, clustered bool, node string, writer bool) error {
	current := datasetsByName(facts)
	// 只有写入者能和别的节点比：备机的库不权威。读不到写入者自己的池时不标记，不能瞎报。
	var baseline map[string]storage.PoolDatasetFact
	var writerFacts storage.PoolFacts
	if writer && node != s.NodeID {
		if own, err := s.Facts(ctx, s.DataPool); err == nil {
			baseline, writerFacts = datasetsByName(own), own
		}
	}
	busy := func(string) bool { return false }
	if writer {
		var err error
		if busy, err = (ReconcileService{Store: s.Store}).importing(ctx); err != nil {
			return err
		}
	}
	claimed := map[string]bool{}
	for _, img := range c.images {
		res.Images = append(res.Images, buildImageRow(img, c, current, baseline, claimed))
	}
	s.addUnclaimedCatalogue(res, facts, current, baseline, claimed, busy)
	sort.SliceStable(res.Images, func(i, j int) bool {
		if res.Images[i].Used != res.Images[j].Used {
			return res.Images[i].Used > res.Images[j].Used
		}
		return res.Images[i].Name < res.Images[j].Name
	})
	for _, img := range res.Images {
		switch img.State {
		case "":
			if len(img.Users) == 0 {
				res.Unused.Count++
				res.Unused.Bytes += img.Used
			}
		case VolumeStatePendingDelete, VolumeStatePendingCopy, VolumeStateBehind:
			res.Sync.Pending++
		}
	}
	if facts.Files != nil && facts.Files.Readable {
		res.FilesReadable = true
		// 主机已删、这台还没跟上的文件，再点删除什么也不会发生，要标出来。读不到主机的文件库时不标。
		var onWriter map[string]bool
		if writerFacts.Files != nil && writerFacts.Files.Readable {
			onWriter = map[string]bool{}
			for _, f := range writerFacts.Files.Imports {
				onWriter[f.Name] = true
			}
		}
		for _, f := range facts.Files.Imports {
			row := VolumeFile{Name: f.Name, Size: f.Size, Used: f.Used, ModifiedAt: unixTime(f.Modified)}
			if onWriter != nil && !onWriter[f.Name] {
				row.State = VolumeStatePendingDelete
				res.Sync.Pending++
			}
			res.Imports = append(res.Imports, row)
		}
	}
	res.Clients = buildClients(facts, c)
	residueUsed := addAsideResidues(res, facts, storage.CatalogueRoot, c)
	buildSystem(res, facts, current)
	res.Sync = s.syncStatus(ctx, res, facts, writerFacts, baseline, servers, clustered, node, writer)
	categorize(res, facts.Used, residueUsed)
	return nil
}

// buildImageRow 把镜像和它的配置算成一行。
func buildImageRow(img domain.Image, c volumeCatalogue, current, baseline map[string]storage.PoolDatasetFact, claimed map[string]bool) VolumeImage {
	row := VolumeImage{ID: img.ID, Name: img.Name, Dataset: storage.CatalogueRoot + "/" + img.ID, Purpose: string(img.Purpose),
		Size: img.Size, Configs: len(c.configs[img.ID]), Points: c.points[img.ID], Users: c.users[img.ID]}
	if row.Users == nil {
		row.Users = []string{}
	}
	if !img.CreatedAt.IsZero() {
		created := img.CreatedAt
		row.CreatedAt = &created
	}
	names := []string{img.ID}
	for _, cfg := range c.configs[img.ID] {
		names = append(names, cfg.ID)
	}
	found, behind := false, false
	for _, name := range names {
		rel := storage.CatalogueRoot + "/" + name
		claimed[rel] = true
		d, ok := current[rel]
		if !ok {
			continue
		}
		found = true
		row.Used += d.Used
		if name == img.ID && d.VolSize > 0 {
			row.Size = d.VolSize // 库里的大小可能是 0
		}
		if w, ok := baseline[rel]; baseline != nil && ok && !sameGUIDs(businessGUIDs(w), businessGUIDs(d)) {
			behind = true
		}
	}
	switch {
	case !found && baseline != nil:
		if _, ok := baseline[row.Dataset]; ok {
			row.State = VolumeStatePendingCopy
		} else {
			row.State = VolumeStateMissing
		}
	case !found:
		row.State = VolumeStateMissing
	case behind:
		row.State = VolumeStateBehind
	}
	return row
}

// addUnclaimedCatalogue 处理 nd 下库里没有的数据集：正在导入的、主机已删等复制的，其余都是未登记。
func (s PoolVolumeService) addUnclaimedCatalogue(res *PoolVolumesResult, facts storage.PoolFacts, current, baseline map[string]storage.PoolDatasetFact,
	claimed map[string]bool, busy func(string) bool) {
	pending := map[string]string{}
	for _, d := range facts.Datasets {
		obj := storage.ClassifyPoolPath(d.Name)
		if claimed[d.Name] {
			continue
		}
		switch obj.Kind {
		case storage.PoolObjectCatalogue:
			if busy(obj.Name) {
				pending[d.Name] = VolumeStateImporting
				continue
			}
			if baseline != nil {
				if _, onWriter := baseline[d.Name]; !onWriter {
					pending[d.Name] = VolumeStatePendingDelete
					continue
				}
			}
			res.Residues = append(res.Residues, VolumeResidue{Dataset: d.Name, Kind: "unregistered", Used: d.Used,
				CreatedAt: unixTime(d.Created), HasBaseline: hasSnapshot(d, storage.BaselineReduction)})
		case storage.PoolObjectSuperseded:
			res.Residues = append(res.Residues, VolumeResidue{Dataset: d.Name, Kind: "superseded", Used: d.Used, CreatedAt: unixTime(d.Created)})
		case storage.PoolObjectRecvTemp:
			res.Residues = append(res.Residues, VolumeResidue{Dataset: d.Name, Kind: "recv_temp", Used: d.Used, CreatedAt: unixTime(d.Created)})
		}
	}
	// 配置跟着镜像算：顺着克隆来源找到根。
	rootOf := func(name string) string {
		for i := 0; i < 16; i++ {
			parent, _, _ := strings.Cut(current[name].Origin, "@")
			if _, ok := pending[parent]; !ok {
				return name
			}
			name = parent
		}
		return name
	}
	rows := map[string]*VolumeImage{}
	var order []string
	for _, d := range facts.Datasets {
		state, ok := pending[d.Name]
		if !ok {
			continue
		}
		root := rootOf(d.Name)
		row := rows[root]
		if row == nil {
			id := storage.DatasetID(root)
			row = &VolumeImage{ID: id, Name: id, Dataset: root, Users: []string{}, State: state}
			rows[root] = row
			order = append(order, root)
		}
		row.Used += d.Used
		if root == d.Name {
			row.Size = d.VolSize
		} else {
			row.Configs++
			row.Points += len(businessGUIDs(d))
		}
	}
	for _, root := range order {
		res.Images = append(res.Images, *rows[root])
	}
}

func unixTime(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

// buildClients 按机器聚合 run 下的盘，机器按 MAC 查：客户机 ID 里的数字可能和 MAC 对不上。
func buildClients(facts storage.PoolFacts, c volumeCatalogue) []VolumeClient {
	rows := map[string]*VolumeClient{}
	byName := make(map[string]storage.PoolDatasetFact, len(facts.Datasets))
	for _, d := range facts.Datasets {
		byName[d.Name] = d
	}
	for _, d := range facts.Datasets {
		obj := storage.ClassifyPoolPath(d.Name)
		if obj.Kind != storage.PoolObjectCarrier {
			continue
		}
		mac := storage.NormalizeMAC(obj.Carrier.MAC)
		row := rows[mac]
		if row == nil {
			row = &VolumeClient{MAC: mac, Disks: []VolumeClientDisk{}}
			if t, ok := c.terminals[mac]; ok && mac != "" {
				row.Known, row.Name, row.State, row.IsSuper, row.OfflineAt = true, t.Name, string(t.State), t.IsSuper, t.OfflineAt
			}
			rows[mac] = row
		}
		row.Used += d.Used
		row.Disks = append(row.Disks, VolumeClientDisk{Dataset: d.Name, Kind: string(obj.Carrier.Kind), LUN: obj.Carrier.LUN, Used: d.Used, Source: cloneSource(d.Origin, byName, c)})
	}
	out := make([]VolumeClient, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Used != out[j].Used {
			return out[i].Used > out[j].Used
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}

// originLabel 把 "nd/<配置>@<快照>" 翻成操作者认识的「镜像名 @快照」。
// cloneSource 说明一块盘来自哪个还原点。开机时系统盘从身份层克隆，身份层是内部的中间层，
// 操作者认的是还原点，所以再往上找一级。
func cloneSource(origin string, byName map[string]storage.PoolDatasetFact, c volumeCatalogue) string {
	dataset, _, ok := strings.Cut(origin, "@")
	if !ok {
		return originLabel(origin, c)
	}
	obj := storage.ClassifyPoolPath(dataset)
	if obj.Kind != storage.PoolObjectCarrier || obj.Carrier.Kind != storage.CarrierIdentity {
		return originLabel(origin, c)
	}
	if layer, found := byName[dataset]; found && layer.Origin != "" {
		return originLabel(layer.Origin, c)
	}
	return "网卡层"
}

func originLabel(origin string, c volumeCatalogue) string {
	dataset, snap, ok := strings.Cut(origin, "@")
	if !ok {
		return origin
	}
	if img, known := c.imageOf[storage.DatasetID(dataset)]; known {
		return img.Name + " @" + snap
	}
	return origin
}

// addAsideResidues 列挪开的副本。根的 used 已含子数据集，不能再加一遍。root 是用来比较的现行目录。
func addAsideResidues(res *PoolVolumesResult, facts storage.PoolFacts, root string, c volumeCatalogue) int64 {
	live := map[string]bool{}
	for _, d := range facts.Datasets {
		if d.Name == root || strings.HasPrefix(d.Name, root+"/") {
			for guid := range businessGUIDs(d) {
				live[guid] = true
			}
		}
	}
	for _, d := range facts.Datasets {
		obj := storage.ClassifyPoolPath(d.Name)
		var kind string
		switch obj.Kind {
		case storage.PoolObjectRebuilding, storage.PoolObjectBackupAside:
			kind = storage.PreservedRebuilding
		case storage.PoolObjectDiverged:
			kind = storage.PreservedDiverged
		case storage.PoolObjectHeld:
			kind = storage.PreservedHeld
		default:
			continue
		}
		row := VolumeResidue{Dataset: d.Name, Kind: kind, Used: d.Used, CreatedAt: asideTime(d.Name, d.Created)}
		for _, inner := range facts.Datasets {
			if inner.Name != d.Name && !strings.HasPrefix(inner.Name, d.Name+"/") {
				continue
			}
			for _, snap := range inner.Snapshots {
				if storage.SystemSnapshot(snap.Name) {
					continue
				}
				row.Points++
				if !live[snap.GUID] {
					row.Unique = append(row.Unique, strings.TrimPrefix(strings.TrimPrefix(inner.Name, d.Name), "/")+"@"+snap.Name)
				}
			}
		}
		row.Dependents = asideDependents(facts, d.Name, c)
		res.Residues = append(res.Residues, row)
	}
	var used int64
	for _, r := range res.Residues {
		used += r.Used
	}
	sort.SliceStable(res.Residues, func(i, j int) bool { return res.Residues[i].Used > res.Residues[j].Used })
	return used
}

// asideTime 从名字里的纳秒时间戳取时间；数据集的 creation 是改名前建的，不是挪开的时候。
func asideTime(name string, created int64) *time.Time {
	stamp := name[strings.LastIndex(name, "-")+1:]
	if ns, err := strconv.ParseInt(stamp, 10, 64); err == nil && ns > 0 {
		t := time.Unix(0, ns).UTC()
		return &t
	}
	return unixTime(created)
}

// asideDependents 点名还克隆着这份副本的对象：克隆自它的客户机盘挡着删除。
func asideDependents(facts storage.PoolFacts, root string, c volumeCatalogue) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range facts.Datasets {
		if d.Origin == "" || d.Name == root || strings.HasPrefix(d.Name, root+"/") {
			continue
		}
		parent, _, _ := strings.Cut(d.Origin, "@")
		if parent != root && !strings.HasPrefix(parent, root+"/") {
			continue
		}
		label := d.Name
		if obj := storage.ClassifyPoolPath(d.Name); obj.Kind == storage.PoolObjectCarrier && obj.Carrier.MAC != "" {
			mac := storage.NormalizeMAC(obj.Carrier.MAC)
			label = mac
			if t, ok := c.terminals[mac]; ok {
				label = terminalLabel(t)
			}
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// buildSystem 列由系统维护的内容，以及认不出的数据集。认不出的只读，也要让操作者看见。
// deletedFilesFloor 以下算平时的元数据，不单列。
const deletedFilesFloor = 16 << 20

func buildSystem(res *PoolVolumesResult, facts storage.PoolFacts, current map[string]storage.PoolDatasetFact) {
	if facts.Files != nil && facts.Files.Readable {
		for _, f := range facts.Files.Dirs {
			res.System = append(res.System, VolumeSystemItem{Kind: "files_dir", Name: f.Name, Path: "nd/files/" + f.Name, Used: f.Used})
		}
		// 删掉的文件还被复制标记占着时已用不降，要说明这部分在哪。
		if d, ok := current["nd/files"]; ok && d.UsedBySnapshots >= deletedFilesFloor {
			res.System = append(res.System, VolumeSystemItem{Kind: "deleted_files", Path: d.Name, Used: d.UsedBySnapshots})
		}
	} else if d, ok := current["nd/files"]; ok {
		res.System = append(res.System, VolumeSystemItem{Kind: "files", Path: d.Name, Used: d.Used})
	}
	if d, ok := current["nd/db"]; ok {
		res.System = append(res.System, VolumeSystemItem{Kind: "db", Path: d.Name, Used: d.Used})
	}
	addMarkerItems(res, facts)
	for _, d := range facts.Datasets {
		switch storage.ClassifyPoolPath(d.Name).Kind {
		case storage.PoolObjectLegacyImports:
			res.System = append(res.System, VolumeSystemItem{Kind: "legacy_imports", Path: d.Name, Used: d.Used})
		case storage.PoolObjectUnknown:
			res.System = append(res.System, VolumeSystemItem{Kind: "unknown", Path: d.Name, Used: d.Used})
		}
	}
}

func addMarkerItems(res *PoolVolumesResult, facts storage.PoolFacts) {
	var rep, backup VolumeSystemItem
	rep.Kind, backup.Kind = "rep_markers", "backup_markers"
	rep.Path, backup.Path = "rep-*", "ndbackup-*"
	for _, d := range facts.Datasets {
		for _, snap := range d.Snapshots {
			switch {
			case strings.HasPrefix(snap.Name, storage.ReplicationSnapshotPrefix):
				rep.Count++
				rep.Used += snap.Used
			case strings.HasPrefix(snap.Name, storage.BackupSnapshotPrefix):
				backup.Count++
				backup.Used += snap.Used
			}
		}
	}
	if rep.Count > 0 {
		res.System = append(res.System, rep)
	}
	if backup.Count > 0 {
		res.System = append(res.System, backup)
	}
}

// categorize 算类别合计；系统类是其余部分，保证合计等于池的已用。
func categorize(res *PoolVolumesResult, used, residueUsed int64) {
	var images, imports, clients int64
	for _, img := range res.Images {
		images += img.Used
	}
	for _, f := range res.Imports {
		imports += f.Used
	}
	for _, c := range res.Clients {
		clients += c.Used
	}
	res.Categories = []VolumeCategory{
		{ID: "images", Bytes: images, Count: len(res.Images)},
		{ID: "imports", Bytes: imports, Count: len(res.Imports)},
		{ID: "client", Bytes: clients, Count: len(res.Clients)},
	}
	if len(res.Residues) > 0 {
		res.Categories = append(res.Categories, VolumeCategory{ID: "residue", Bytes: residueUsed, Count: len(res.Residues)})
	}
	system := used - images - imports - clients - residueUsed
	if system < 0 {
		system = 0
	}
	res.Categories = append(res.Categories, VolumeCategory{ID: "system", Bytes: system, Count: len(res.System)})
}

func latestMarkerOf(d storage.PoolDatasetFact, prefix string) string {
	latest := ""
	var latestSeq int64
	for _, snap := range d.Snapshots {
		if !strings.HasPrefix(snap.Name, prefix) {
			continue
		}
		seq, err := strconv.ParseInt(strings.TrimPrefix(snap.Name, prefix), 10, 64)
		if err == nil && (latest == "" || seq > latestSeq) {
			latest, latestSeq = snap.Name, seq
		}
	}
	return latest
}

func (s PoolVolumeService) syncStatus(ctx context.Context, res *PoolVolumesResult, facts, writerFacts storage.PoolFacts, baseline map[string]storage.PoolDatasetFact,
	servers []domain.Server, clustered bool, node string, writer bool) VolumeSync {
	sync := VolumeSync{Level: "ok", Pending: res.Sync.Pending}
	if !clustered {
		return sync
	}
	if !writer {
		sync.Level, sync.Message = "warn", "这台是备机，名称和使用情况可能滞后，请到主机查看"
		return sync
	}
	if node == s.NodeID {
		return s.writerSync(ctx, sync, facts, servers)
	}
	if facts.PullError != "" {
		sync.Level, sync.Message = "error", "复制停滞："+facts.PullError
		return sync
	}
	if baseline == nil {
		sync.Level, sync.Message = "warn", "读不到主机的池，没法和主机比较"
		return sync
	}
	writerIP := s.NodeID
	for _, srv := range servers {
		if srv.ID == s.NodeID && srv.IP != "" {
			writerIP = srv.IP
		}
	}
	mine := latestMarkerOf(datasetsByName(facts)[storage.CatalogueRoot], storage.ReplicationSnapshotPrefix)
	latest := latestMarkerOf(datasetsByName(writerFacts)[storage.CatalogueRoot], storage.ReplicationSnapshotPrefix)
	if CaughtUpTo(mine, latest) {
		sync.Message = "已跟上主机 " + writerIP
		return sync
	}
	gotSeq, gotOK := markerSeq(mine)
	wantSeq, wantOK := markerSeq(latest)
	if gotOK && wantOK && time.Duration(wantSeq-gotSeq) <= replicationAliveWindow {
		sync.Message = "正在跟上主机 " + writerIP
		return sync
	}
	sync.Level = "warn"
	if gotOK && wantOK {
		sync.Message = fmt.Sprintf("落后主机 %s 约 %d 分钟", writerIP, int(time.Duration(wantSeq-gotSeq).Minutes()))
	} else {
		sync.Message = "还没有跟上主机 " + writerIP
	}
	return sync
}

func (s PoolVolumeService) writerSync(ctx context.Context, sync VolumeSync, facts storage.PoolFacts, servers []domain.Server) VolumeSync {
	now := s.now()
	ipOf := map[string]string{}
	var dark []string
	others := 0
	for _, srv := range servers {
		if srv.ID == s.NodeID {
			continue
		}
		others++
		if host := hostOfURL(srv.APIURL); host != "" {
			ipOf[host] = srv.IP
		}
		if srv.LastSeenAt == nil || now.Sub(*srv.LastSeenAt) > place.Freshness {
			dark = append(dark, srv.IP)
		}
	}
	rows, err := s.Store.ReplicationStates().List(ctx)
	if err != nil {
		sync.Level, sync.Message = "warn", "读不到复制状态"
		return sync
	}
	latest := latestMarkerOf(datasetsByName(facts)[storage.CatalogueRoot], storage.ReplicationSnapshotPrefix)
	var parts []string
	lagging := LaggingPullers(rows, latest, now, replicationAliveWindow, s.SelfURL)
	if len(lagging) == 1 && lagging[0] == "" {
		lagging = nil
		if len(dark) < others {
			parts = append(parts, "备机还没有开始跟随")
		}
	}
	var behind []string
	for _, target := range lagging {
		host := hostOfURL(target)
		if ip := ipOf[host]; ip != "" {
			host = ip
		}
		behind = append(behind, host)
	}
	if len(behind) > 0 {
		parts = append(parts, strings.Join(behind, "、")+" 还没跟上")
	}
	if len(dark) > 0 {
		parts = append(parts, strings.Join(dark, "、")+" 没有应答")
	}
	if len(parts) == 0 {
		sync.Message = "备机都已跟上"
		return sync
	}
	sync.Level, sync.Message = "warn", strings.Join(parts, "；")
	return sync
}

func hostOfURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return raw
}

// buildBackupPool 列备份池：整份副本里有什么，和本节点数据池比较。读不到数据池时不比较，不能瞎报。
func (s PoolVolumeService) buildBackupPool(ctx context.Context, res *PoolVolumesResult, facts storage.PoolFacts, c volumeCatalogue, node string, target *domain.Server) {
	current := datasetsByName(facts)
	copyRoot := storage.BackupRoot + "/" + storage.CatalogueRoot
	backup := &VolumeBackup{Items: []VolumeBackupItem{}}
	root, present := current[copyRoot]
	var data map[string]storage.PoolDatasetFact
	if own, err := s.read(ctx, node, target, facts.DataPool); err == nil {
		data = datasetsByName(own)
	}
	if present {
		backup.Present, backup.Used = true, root.Used
		copied := latestMarkerOf(root, storage.BackupSnapshotPrefix)
		if at, ok := markerTime(copied); ok {
			backup.LastBackupAt = &at
		}
		if data != nil {
			latest := latestMarkerOf(data[storage.CatalogueRoot], storage.BackupSnapshotPrefix)
			backup.Pending = latest != "" && latest != copied
		}
		backup.Items, backup.SystemUsed = buildBackupItems(facts, current, data, c)
	}
	res.Backup = backup
	residueUsed := addAsideResidues(res, facts, copyRoot, c)
	for _, d := range facts.Datasets {
		if storage.ClassifyPoolPath(d.Name).Kind == storage.PoolObjectUnknown {
			res.System = append(res.System, VolumeSystemItem{Kind: "unknown", Path: d.Name, Used: d.Used})
		}
	}
	res.Categories = []VolumeCategory{{ID: "backup", Bytes: backup.Used, Count: len(backup.Items)}}
	if len(res.Residues) > 0 {
		res.Categories = append(res.Categories, VolumeCategory{ID: "residue", Bytes: residueUsed, Count: len(res.Residues)})
	}
	if system := facts.Used - backup.Used - residueUsed; system > 0 || len(res.System) > 0 {
		if system < 0 {
			system = 0
		}
		res.Categories = append(res.Categories, VolumeCategory{ID: "system", Bytes: system, Count: len(res.System)})
	}
}

func buildBackupItems(facts storage.PoolFacts, current, data map[string]storage.PoolDatasetFact, c volumeCatalogue) ([]VolumeBackupItem, int64) {
	prefix := storage.BackupRoot + "/" + storage.CatalogueRoot + "/"
	claimed := map[string]bool{}
	var items []VolumeBackupItem
	for _, img := range c.images {
		names := []string{img.ID}
		for _, cfg := range c.configs[img.ID] {
			names = append(names, cfg.ID)
		}
		item := VolumeBackupItem{ID: img.ID, Name: img.Name, Dataset: prefix + img.ID, Purpose: string(img.Purpose), Size: img.Size, State: BackupItemSame}
		if !img.CreatedAt.IsZero() {
			created := img.CreatedAt
			item.CreatedAt = &created
		}
		found := false
		for _, name := range names {
			d, ok := current[prefix+name]
			claimed[prefix+name] = true
			if !ok {
				continue
			}
			found = true
			item.Used += d.Used
			if name == img.ID && d.VolSize > 0 {
				item.Size = d.VolSize
			} else if name != img.ID {
				item.Configs++
				item.Points += len(businessGUIDs(d))
			}
			compareBackupDataset(&item, name, d, data)
		}
		if found {
			items = append(items, item)
		}
	}
	var systemUsed int64
	for _, d := range facts.Datasets {
		obj := storage.ClassifyPoolPath(d.Name)
		if obj.Kind != storage.PoolObjectBackupItem || claimed[d.Name] {
			continue
		}
		if storage.ReservedCatalogueChild(obj.Name) {
			systemUsed += d.Used
			continue
		}
		item := VolumeBackupItem{ID: obj.Name, Name: obj.Name, Dataset: d.Name, Used: d.Used, Size: d.VolSize, State: BackupItemSame, CreatedAt: unixTime(d.Created)}
		compareBackupDataset(&item, obj.Name, d, data)
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Used > items[j].Used })
	return items, systemUsed
}

func compareBackupDataset(item *VolumeBackupItem, name string, d storage.PoolDatasetFact, data map[string]storage.PoolDatasetFact) {
	if data == nil {
		return
	}
	live, ok := data[storage.CatalogueRoot+"/"+name]
	if !ok {
		item.State = BackupItemRemoved
		return
	}
	if item.State == BackupItemSame && !sameGUIDs(businessGUIDs(live), businessGUIDs(d)) {
		item.State = BackupItemNewer
	}
}

// PoolFactsCache 让重复打开页面不重复跑 zfs list：同一个池 TTL 内只读一次，同时来的请求排队等同一次读。失败不缓存。
type PoolFactsCache struct {
	TTL  time.Duration
	Now  func() time.Time
	Read func(ctx context.Context, pool string) (storage.PoolFacts, error)

	mu      sync.Mutex
	entries map[string]*poolFactsEntry
}

type poolFactsEntry struct {
	mu    sync.Mutex
	at    time.Time
	facts storage.PoolFacts
	valid bool
}

// Forget 丢掉记住的内容：删除之后页面要马上看到变化。
func (c *PoolFactsCache) Forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

func (c *PoolFactsCache) Get(ctx context.Context, pool string) (storage.PoolFacts, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*poolFactsEntry{}
	}
	entry := c.entries[pool]
	if entry == nil {
		entry = &poolFactsEntry{}
		c.entries[pool] = entry
	}
	c.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if entry.valid && now().Sub(entry.at) < c.TTL {
		return entry.facts, nil
	}
	facts, err := c.Read(ctx, pool)
	if err != nil {
		entry.valid = false
		// 池名来自请求参数，读失败的不留着。
		c.mu.Lock()
		delete(c.entries, pool)
		c.mu.Unlock()
		return storage.PoolFacts{}, err
	}
	entry.facts, entry.at, entry.valid = facts, now(), true
	return facts, nil
}
