package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

const (
	volWin11  = "iso_3e8aaa1eed40f3f0"
	volCn     = "cn-win11"
	volNodeA  = "node-a"
	volNodeB  = "node-b"
	gib       = int64(1) << 30
	rep1      = "rep-1793945325743146616"
	rep0      = "rep-1793945000000000000"
	backupRep = "ndbackup-1793900000000000000"
)

var volNow = time.Date(2026, 11, 6, 6, 10, 0, 0, time.UTC)

// seedVolumes 用集群真实的形状：一个在用的 win11、一个没人用的 cn-win11、一个数据盘，超管机 11 和 22。
// 客户机 22 的 ID 带 …650，MAC 却是 …65F：只能按 MAC 查。
func seedVolumes(t *testing.T) *store.SQLStore {
	t.Helper()
	ctx := context.Background()
	st := newImageTestStore(t)
	mustNil := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 0, 0, 0, time.UTC) }
	mustNil(st.Images().Create(ctx, domain.Image{ID: volWin11, Name: "win11_25h2", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeSystem, CreatedAt: day(22)}))
	mustNil(st.Images().Create(ctx, domain.Image{ID: volCn, Name: "cn-win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeSystem, CreatedAt: day(19)}))
	mustNil(st.Images().Create(ctx, domain.Image{ID: "data", Name: "data", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, Purpose: domain.ImagePurposeData, CreatedAt: day(21)}))
	for _, c := range []struct{ id, image string }{{volWin11 + "_default", volWin11}, {volCn + "_default", volCn}, {"data_default", "data"}} {
		mustNil(st.Configs().Create(ctx, domain.Config{ID: c.id, ImageID: c.image, Name: "default", CreatedAt: day(22)}))
	}
	for _, r := range []struct{ id, cfg, name string }{
		{volWin11 + "_default_2", volWin11 + "_default", "@2"},
		{volWin11 + "_default_1", volWin11 + "_default", "@1"},
		{volWin11 + "_default_0", volWin11 + "_default", "@0"},
		{volCn + "_default_0", volCn + "_default", "@0"},
		{"data_default_0", "data_default", "@0"},
	} {
		mustNil(st.Reductions().Create(ctx, domain.Reduction{ID: r.id, ConfigID: r.cfg, Name: r.name, DisplayName: r.name, CreatedAt: day(22), Status: domain.ReductionStatusReady}))
	}
	mustNil(st.Groups().Create(ctx, domain.Group{ID: "g1", Name: "test", StartIP: "10.0.0.100", ClientMax: 100, Netmask: "255.255.255.0", SystemImageID: volWin11, SystemConfigID: volWin11 + "_default", SystemReductionID: volWin11 + "_default_2"}))
	mustNil(st.GroupDisks().Create(ctx, domain.GroupDisk{ID: "gd1", GroupID: "g1", MountTarget: "E:", ImageID: "data", ConfigID: "data_default"}))
	offline := time.Date(2026, 10, 22, 8, 19, 0, 0, time.UTC)
	mustNil(st.Terminals().Create(ctx, domain.Terminal{ID: "terminal-0050562EF64E", Name: "11", MAC: "0050562EF64E", IP: "10.0.0.11", GroupID: "g1", IsSuper: true, State: domain.TerminalStateOffline}))
	mustNil(st.Terminals().Create(ctx, domain.Terminal{ID: "terminal-0050562EF650", Name: "22", MAC: "0050562EF65F", IP: "10.0.0.22", GroupID: "g1", State: domain.TerminalStateOffline, OfflineAt: &offline}))
	mustNil(st.Servers().Create(ctx, domain.Server{ID: volNodeA, Name: "a", IP: "192.168.10.3", APIURL: "http://192.168.10.3:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &volNow}))
	mustNil(st.Servers().Create(ctx, domain.Server{ID: volNodeB, Name: "b", IP: "192.168.10.4", APIURL: "http://192.168.10.4:8080", Role: domain.ServerRoleAll, Status: domain.ServerStatusUp, LastSeenAt: &volNow}))
	return st
}

func ds(name string, used int64, snaps ...storage.PoolSnapshotFact) storage.PoolDatasetFact {
	return storage.PoolDatasetFact{Name: name, Type: "volume", Used: used, VolSize: 64 * gib, Created: 1792424015, Snapshots: snaps}
}

func snapOf(name, guid string, used int64, clones ...string) storage.PoolSnapshotFact {
	return storage.PoolSnapshotFact{Name: name, GUID: guid, Used: used, Created: 1792424236, Clones: clones}
}

// writerFacts 是主机 .3 的 data 池。合计留 1 GiB 给系统类，用来核对总账。
func writerFacts() storage.PoolFacts {
	win11 := ds("nd/"+volWin11, 10*gib, snapOf("0", "g-win11-0", 0, "nd/"+volWin11+"_default"))
	cfg := ds("nd/"+volWin11+"_default", 1*gib, snapOf("0", "g-c0", 0), snapOf("1", "g-c1", 96296960), snapOf("2", "g-c2", 0, "run/IDENT-0050562EF65F"))
	cfg.Origin = "nd/" + volWin11 + "@0"
	cn := ds("nd/"+volCn, 8*gib, snapOf("0", "g-cn-0", 0, "nd/"+volCn+"_default"))
	cnCfg := ds("nd/"+volCn+"_default", 8192, snapOf("0", "g-cnc-0", 0))
	cnCfg.Origin = "nd/" + volCn + "@0"
	dataImg := ds("nd/data", 323584, snapOf("0", "g-d-0", 0, "nd/data_default"))
	dataImg.VolSize = 20 * gib
	dataCfg := ds("nd/data_default", 413696, snapOf("0", "g-dc-0", 0))
	dataCfg.Origin = "nd/data@0"
	ident := ds("run/IDENT-0050562EF65F", 9158656)
	ident.Origin = "nd/" + volWin11 + "_default@2"
	root := storage.PoolDatasetFact{Name: "nd", Type: "filesystem", Snapshots: []storage.PoolSnapshotFact{snapOf(rep1, "g-rep1", 0)}}
	files := storage.PoolDatasetFact{Name: "nd/files", Type: "filesystem", Used: 4 * gib, UsedBySnapshots: 57344}
	db := storage.PoolDatasetFact{Name: "nd/db", Type: "filesystem", Used: 1511424, UsedBySnapshots: 1163264,
		Snapshots: []storage.PoolSnapshotFact{snapOf(rep1, "g-rep1-db", 1163264)}}
	imports := []storage.PoolFileFact{{Name: "Win11.iso", Size: 3 * gib, Used: 2 * gib, Modified: 1792600000}, {Name: "CentOS.iso", Size: 2 * gib, Used: gib, Modified: 1792600000}}
	var sum int64
	for _, d := range []storage.PoolDatasetFact{win11, cfg, cn, cnCfg, dataImg, dataCfg, ident} {
		sum += d.Used
	}
	return storage.PoolFacts{
		Node: volNodeA, Pool: "data", DataPool: "data", Used: sum + 3*gib + 1*gib, Available: 48 * gib,
		Datasets: []storage.PoolDatasetFact{{Name: "", Type: "filesystem"}, root, {Name: "run", Type: "filesystem"}, win11, cfg, cn, cnCfg, dataImg, dataCfg, ident, files, db},
		Files:    &storage.PoolFilesFact{Readable: true, Imports: imports, Dirs: []storage.PoolFileFact{{Name: "winpe", Size: gib, Used: gib}}},
	}
}

func volumeService(st store.Store, local, remote map[string]storage.PoolFacts) PoolVolumeService {
	return PoolVolumeService{
		Store: st, NodeID: volNodeA, DataPool: "data", Writer: func() bool { return true }, Now: func() time.Time { return volNow },
		Facts: func(_ context.Context, pool string) (storage.PoolFacts, error) {
			facts, ok := local[pool]
			if !ok {
				return storage.PoolFacts{}, errors.New("这台服务器上没有存储池 " + pool)
			}
			return facts, nil
		},
		Fetch: func(_ context.Context, _ string, pool string) (storage.PoolFacts, error) {
			facts, ok := remote[pool]
			if !ok {
				return storage.PoolFacts{}, errors.New("connection refused")
			}
			return facts, nil
		},
	}
}

func imageRow(t *testing.T, res PoolVolumesResult, id string) VolumeImage {
	t.Helper()
	for _, img := range res.Images {
		if img.ID == id {
			return img
		}
	}
	t.Fatalf("没有镜像 %s：%+v", id, res.Images)
	return VolumeImage{}
}

func category(t *testing.T, res PoolVolumesResult, id string) VolumeCategory {
	t.Helper()
	for _, c := range res.Categories {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("没有类别 %s：%+v", id, res.Categories)
	return VolumeCategory{}
}

func TestPoolVolumesOnTheWriterListsImagesWithWhoUsesThem(t *testing.T) {
	st := seedVolumes(t)
	svc := volumeService(st, map[string]storage.PoolFacts{"data": writerFacts()}, nil)
	res, err := svc.List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reachable || !res.Authoritative || res.Role != PoolRoleData || res.IP != "192.168.10.3" || res.NodeRole != "writer" {
		t.Fatalf("池头不对：%+v", res)
	}
	win11 := imageRow(t, res, volWin11)
	if win11.Name != "win11_25h2" || win11.Dataset != "nd/"+volWin11 {
		t.Fatalf("名字要取库、数据集放次要位置：%+v", win11)
	}
	if win11.Used != 11*gib || win11.Size != 64*gib {
		t.Fatalf("实占要含配置，大小取 volsize：%+v", win11)
	}
	if win11.Configs != 1 || win11.Points != 3 {
		t.Fatalf("包含数不对：%+v", win11)
	}
	if strings.Join(win11.Users, "|") != "分组 test|超管机 11" {
		t.Fatalf("谁在用不对：%v", win11.Users)
	}
	if win11.CreatedAt.Day() != 22 {
		t.Fatalf("日期要取库：数据集的创建时间是本机收下的时间：%v", win11.CreatedAt)
	}
	if got := strings.Join(imageRow(t, res, "data").Users, "|"); got != "分组 test 的 E:|超管机 11" {
		t.Fatalf("数据盘的使用者不对：%s", got)
	}
	if cn := imageRow(t, res, volCn); len(cn.Users) != 0 || cn.State != "" {
		t.Fatalf("没人用的镜像不该有使用者：%+v", cn)
	}
	if res.Unused.Count != 1 || res.Unused.Bytes != 8*gib+8192 {
		t.Fatalf("未使用的合计不对：%+v", res.Unused)
	}
	if res.Images[0].ID != volWin11 {
		t.Fatalf("按占用从大到小排：%+v", res.Images)
	}
}

// 各类别合计必须等于池的已用，账对不上就没法回答「空间去哪了」。
func TestPoolVolumesCategoriesAddUpToThePoolUsage(t *testing.T) {
	st := seedVolumes(t)
	facts := writerFacts()
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, c := range res.Categories {
		sum += c.Bytes
	}
	if sum != facts.Used || res.Used != facts.Used || res.Available != facts.Available {
		t.Fatalf("合计 %d，池已用 %d：%+v", sum, facts.Used, res.Categories)
	}
	if got := category(t, res, "imports"); got.Bytes != 3*gib || got.Count != 2 {
		t.Fatalf("导入文件按实际占用算：%+v", got)
	}
	if got := category(t, res, "system"); got.Bytes != 1*gib {
		t.Fatalf("系统类是其余部分：%+v", got)
	}
	for _, c := range res.Categories {
		if c.ID == "residue" {
			t.Fatalf("没有残留时不显示这个类别：%+v", res.Categories)
		}
	}
}

func TestPoolVolumesGroupsClientDisksByMachineLookedUpByMAC(t *testing.T) {
	st := seedVolumes(t)
	facts := writerFacts()
	data := ds("run/CLIENT-0050562EF65F-DATA-1", 4096)
	data.Origin = "nd/data_default@0"
	stranger := ds("run/CLIENT-AABBCCDDEEFF", 8192)
	scratch := ds("run/INSPECT-WIN-1792", 16384)
	facts.Datasets = append(facts.Datasets, data, stranger, scratch)
	facts.Used += 4096 + 8192 + 16384
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	var known, unknown, temp *VolumeClient
	for i := range res.Clients {
		switch c := &res.Clients[i]; {
		case c.MAC == "0050562EF65F":
			known = c
		case c.MAC == "AABBCCDDEEFF":
			unknown = c
		case c.MAC == "":
			temp = c
		}
	}
	if known == nil || known.Name != "22" || !known.Known || known.State != string(domain.TerminalStateOffline) || known.OfflineAt == nil {
		t.Fatalf("客户机要按 MAC 查，不解析 ID：%+v", known)
	}
	if len(known.Disks) != 2 || known.Used != 9158656+4096 {
		t.Fatalf("同一台的盘要放一行：%+v", known)
	}
	var identity *VolumeClientDisk
	for i := range known.Disks {
		if known.Disks[i].Kind == string(storage.CarrierIdentity) {
			identity = &known.Disks[i]
		}
	}
	if identity == nil || identity.Source != "win11_25h2 @2" {
		t.Fatalf("身份层要说明来自哪个还原点：%+v", known.Disks)
	}
	if unknown == nil || unknown.Known {
		t.Fatalf("库里没有的机器要照实标：%+v", unknown)
	}
	if temp == nil || len(temp.Disks) != 1 || temp.Disks[0].Kind != string(storage.CarrierInspect) {
		t.Fatalf("没有机器的临时克隆也要列出来：%+v", temp)
	}
	if got := category(t, res, "client"); got.Bytes != 9158656+4096+8192+16384 || got.Count != 3 {
		t.Fatalf("客户机盘类别不对：%+v", got)
	}
}

func TestPoolVolumesOnAStandbyMarksWhatHasNotFollowedYet(t *testing.T) {
	st := seedVolumes(t)
	writer := writerFacts()
	// 库里和主机都有 data，这台还没收到：待复制。
	// 主机已删的 old：这台还有：待复制删除，配置跟着镜像算。
	// win11 主机有 @1，这台没有：落后。
	node := writerFacts()
	node.Node, node.Pool, node.DataPool = volNodeB, "tank", "tank"
	var kept []storage.PoolDatasetFact
	for _, d := range node.Datasets {
		switch d.Name {
		case "nd/data", "nd/data_default":
			continue
		case "nd/" + volWin11 + "_default":
			d.Snapshots = []storage.PoolSnapshotFact{snapOf("0", "g-c0", 0), snapOf("2", "g-c2", 0)}
		}
		kept = append(kept, d)
	}
	old := ds("nd/old", 2*gib, snapOf("0", "g-old-0", 0, "nd/old_default"))
	oldCfg := ds("nd/old_default", 4096, snapOf("0", "g-oldc-0", 0))
	oldCfg.Origin = "nd/old@0"
	node.Datasets = append(kept, old, oldCfg)

	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writer}, map[string]storage.PoolFacts{"tank": node}).List(context.Background(), volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeRole != "standby" || res.IP != "192.168.10.4" || !res.Authoritative {
		t.Fatalf("池头不对：%+v", res)
	}
	if got := imageRow(t, res, "data").State; got != VolumeStatePendingCopy {
		t.Fatalf("主机有、这台没有应为待复制：%q", got)
	}
	if got := imageRow(t, res, volWin11).State; got != VolumeStateBehind {
		t.Fatalf("少业务快照应为落后：%q", got)
	}
	if got := imageRow(t, res, volCn).State; got != "" {
		t.Fatalf("一致的镜像不该标记：%q", got)
	}
	pending := imageRow(t, res, "old")
	if pending.State != VolumeStatePendingDelete || pending.Used != 2*gib+4096 {
		t.Fatalf("主机已删、这台还有应为待复制删除，配置算在镜像里：%+v", pending)
	}
	for _, img := range res.Images {
		if img.ID == "old_default" {
			t.Fatalf("配置不该单独成行：%+v", res.Images)
		}
	}
	if res.Sync.Pending != 3 {
		t.Fatalf("待跟随项数不对：%+v", res.Sync)
	}
	for _, r := range res.Residues {
		if r.Dataset == "nd/old" {
			t.Fatalf("待复制删除不是残留：%+v", r)
		}
	}
}

// 只比业务快照：rep-* 每轮都变，比进去每一行都会闪。
func TestPoolVolumesIgnoresRoundMarkersWhenComparingWithTheWriter(t *testing.T) {
	st := seedVolumes(t)
	writer := writerFacts()
	node := writerFacts()
	node.Node, node.Pool, node.DataPool = volNodeB, "tank", "tank"
	for i := range writer.Datasets {
		if writer.Datasets[i].Name == "nd/"+volCn {
			writer.Datasets[i].Snapshots = append(writer.Datasets[i].Snapshots, snapOf(rep1, "g-new", 0), snapOf(backupRep, "g-new2", 0))
		}
	}
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writer}, map[string]storage.PoolFacts{"tank": node}).List(context.Background(), volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if got := imageRow(t, res, volCn).State; got != "" {
		t.Fatalf("复制标记不该算落后：%q", got)
	}
}

func TestPoolVolumesOnTheWriterReportsMissingAndUnregistered(t *testing.T) {
	st := seedVolumes(t)
	facts := writerFacts()
	var kept []storage.PoolDatasetFact
	for _, d := range facts.Datasets {
		if d.Name != "nd/data" && d.Name != "nd/data_default" {
			kept = append(kept, d)
		}
	}
	lost := ds("nd/lost", 3*gib, snapOf("0", "g-lost", 0))
	saved := ds("nd/win10_default_before_super", gib)
	temp := ds("nd/recv-12-3", 4096)
	facts.Datasets = append(kept, lost, saved, temp)
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if got := imageRow(t, res, "data").State; got != VolumeStateMissing {
		t.Fatalf("库有池无要标出来：%q", got)
	}
	kinds := map[string]VolumeResidue{}
	for _, r := range res.Residues {
		kinds[r.Dataset] = r
	}
	if r := kinds["nd/lost"]; r.Kind != "unregistered" || !r.HasBaseline {
		t.Fatalf("库里没有的目录卷是未登记，带 @0 的要提示会被收养：%+v", r)
	}
	if kinds["nd/win10_default_before_super"].Kind != "superseded" || kinds["nd/recv-12-3"].Kind != "recv_temp" {
		t.Fatalf("残留种类不对：%+v", res.Residues)
	}
	for _, img := range res.Images {
		if img.ID == "lost" {
			t.Fatalf("未登记卷不算镜像：%+v", img)
		}
	}
}

// 正在导入的卷库行还没写，不能报成未登记。
func TestPoolVolumesDoesNotCallAnImportInProgressUnregistered(t *testing.T) {
	st := seedVolumes(t)
	ctx := context.Background()
	if err := st.Tasks().Create(ctx, domain.Task{ID: "task-import", Type: domain.TaskTypeImportImage, Status: domain.TaskStatusRunning, TargetRef: "newimg", CreatedAt: volNow}); err != nil {
		t.Fatal(err)
	}
	facts := writerFacts()
	facts.Datasets = append(facts.Datasets, ds("nd/newimg", gib))
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(ctx, "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if got := imageRow(t, res, "newimg").State; got != VolumeStateImporting {
		t.Fatalf("导入中的卷要照实标：%q", got)
	}
	for _, r := range res.Residues {
		if r.Dataset == "nd/newimg" {
			t.Fatalf("导入中的卷被当成残留：%+v", r)
		}
	}
}

func TestPoolVolumesExplainsResiduesWithoutCountingChildrenTwice(t *testing.T) {
	st := seedVolumes(t)
	facts := writerFacts()
	diverged := "nd-diverged-1792304492415187876"
	child := ds(diverged+"/vmdk_default", 5*gib, snapOf("realtek", "g-unique", 0), snapOf("2", "g-c2", 0), snapOf(rep0, "g-mark", 0))
	root := storage.PoolDatasetFact{Name: diverged, Type: "filesystem", Used: 5 * gib}
	rebuilding := storage.PoolDatasetFact{Name: "nd-rebuilding-1792388566008542804", Type: "filesystem", Used: 2 * gib}
	held := ds("nd-held-1792480000000000000", gib, snapOf("0", "g-held", 0))
	layer := ds("run/IDENT-0050562EF65E", 4096)
	layer.Origin = "nd-held-1792480000000000000@0"
	facts.Datasets = append(facts.Datasets, root, child, rebuilding, held, layer)
	facts.Used += 8*gib + 4096
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]VolumeResidue{}
	for _, r := range res.Residues {
		kinds[r.Kind] = r
	}
	d := kinds["diverged"]
	if d.Dataset != diverged || d.Used != 5*gib || d.CreatedAt == nil || d.CreatedAt.Year() != 2026 {
		t.Fatalf("分叉副本不对：%+v", d)
	}
	if strings.Join(d.Unique, ",") != "vmdk_default@realtek" {
		t.Fatalf("只列当前目录里没有的还原点，复制标记不算：%v", d.Unique)
	}
	if h := kinds["held"]; strings.Join(h.Dependents, ",") != "0050562EF65E" {
		t.Fatalf("要点名是谁还在用：%+v", h)
	}
	if got := category(t, res, "residue"); got.Bytes != 8*gib || got.Count != 3 {
		t.Fatalf("残留合计不能把子数据集再算一遍：%+v", got)
	}
	for _, c := range res.Clients {
		if c.MAC == "0050562EF65E" && c.Used != 4096 {
			t.Fatalf("客户机盘不对：%+v", c)
		}
	}
}

func TestPoolVolumesReadsABackupPoolAgainstItsNodesDataPool(t *testing.T) {
	st := seedVolumes(t)
	data := writerFacts()
	// data 池删了 cn-win11，新增了 win11 的 @3，并打了更新的备份标记。
	var kept []storage.PoolDatasetFact
	for _, d := range data.Datasets {
		if d.Name == "nd/"+volCn || d.Name == "nd/"+volCn+"_default" {
			continue
		}
		if d.Name == "nd/"+volWin11+"_default" {
			d.Snapshots = append(append([]storage.PoolSnapshotFact{}, d.Snapshots...), snapOf("3", "g-c3", 0))
		}
		if d.Name == "nd" {
			d.Snapshots = append(append([]storage.PoolSnapshotFact{}, d.Snapshots...), snapOf("ndbackup-1794000000000000000", "g-b2", 0))
		}
		kept = append(kept, d)
	}
	data.Datasets = kept
	copyRoot := storage.PoolDatasetFact{Name: "ndiskless/nd", Type: "filesystem", Used: 19 * gib, Snapshots: []storage.PoolSnapshotFact{snapOf(backupRep, "g-b1", 0)}}
	backup := storage.PoolFacts{Node: volNodeA, Pool: "backup", DataPool: "data", Used: 19*gib + 3*gib + 4096, Available: 100 * gib, Datasets: []storage.PoolDatasetFact{
		{Name: "", Type: "filesystem"}, {Name: "ndiskless", Type: "filesystem"}, copyRoot,
		ds("ndiskless/nd/"+volWin11, 10*gib, snapOf("0", "g-win11-0", 0)),
		ds("ndiskless/nd/"+volWin11+"_default", gib, snapOf("0", "g-c0", 0), snapOf("1", "g-c1", 0), snapOf("2", "g-c2", 0)),
		ds("ndiskless/nd/"+volCn, 8*gib, snapOf("0", "g-cn-0", 0)),
		{Name: "ndiskless/nd-rebuilding-1793000000000000000", Type: "filesystem", Used: 3 * gib},
	}}
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": data, "backup": backup}, nil).List(context.Background(), "", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if res.Role != PoolRoleBackup || res.Backup == nil {
		t.Fatalf("备份池不对：%+v", res)
	}
	if !res.Backup.Present || res.Backup.Used != 19*gib || res.Backup.LastBackupAt == nil || !res.Backup.Pending {
		t.Fatalf("备份概况不对：%+v", res.Backup)
	}
	states := map[string]string{}
	for _, it := range res.Backup.Items {
		states[it.ID] = it.State
	}
	if states[volWin11] != BackupItemNewer || states[volCn] != BackupItemRemoved {
		t.Fatalf("和数据池比较不对：%+v", res.Backup.Items)
	}
	if len(res.Images) != 0 || len(res.Clients) != 0 {
		t.Fatalf("备份池不列数据池的类别：%+v", res)
	}
	if len(res.Residues) != 1 || res.Residues[0].Kind != "rebuilding" {
		t.Fatalf("备份重建留下的旧副本算残留：%+v", res.Residues)
	}
	if got := category(t, res, "backup"); got.Bytes != 19*gib {
		t.Fatalf("备份副本类别不对：%+v", got)
	}
}

func TestPoolVolumesSaysWhenANodeCannotBeRead(t *testing.T) {
	st := seedVolumes(t)
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writerFacts()}, nil).List(context.Background(), volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reachable || !strings.Contains(res.Error, "192.168.10.4") || len(res.Images) != 0 {
		t.Fatalf("读不到要说明，不能显示成空：%+v", res)
	}
}

func TestPoolVolumesWithoutTheWriterDatabaseIsMarkedNotAuthoritative(t *testing.T) {
	st := seedVolumes(t)
	svc := volumeService(st, map[string]storage.PoolFacts{"data": writerFacts()}, nil)
	svc.Writer = func() bool { return false }
	res, err := svc.List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if res.Authoritative || res.NodeRole != "standby" {
		t.Fatalf("备机的库不权威：%+v", res)
	}
}

func TestPoolVolumesReportsReplicationProgress(t *testing.T) {
	st := seedVolumes(t)
	ctx := context.Background()
	writer := writerFacts()
	// 主机：备机最后确认的是旧标记。
	ok := volNow
	if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{Target: "http://192.168.10.4:8080", Kind: "primary-serve", Root: "data/nd", LastSnapshot: rep0, LastOKAt: &ok, UpdatedAt: volNow}); err != nil {
		t.Fatal(err)
	}
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writer}, nil).List(ctx, "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sync.Level != "warn" || !strings.Contains(res.Sync.Message, "192.168.10.4") {
		t.Fatalf("没跟上的备机要点名：%+v", res.Sync)
	}
	if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{Target: "http://192.168.10.4:8080", Kind: "primary-serve", Root: "data/nd", LastSnapshot: rep1, LastOKAt: &ok, UpdatedAt: volNow}); err != nil {
		t.Fatal(err)
	}
	res, err = volumeService(st, map[string]storage.PoolFacts{"data": writer}, nil).List(ctx, "", "data")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sync.Level != "ok" {
		t.Fatalf("都跟上了不该告警：%+v", res.Sync)
	}

	// 备机：复制错误直接点出来。
	node := writerFacts()
	node.Node, node.Pool, node.DataPool, node.PullError = volNodeB, "tank", "tank", "对端的目录是空的"
	for i := range node.Datasets {
		if node.Datasets[i].Name == "nd" {
			node.Datasets[i].Snapshots = []storage.PoolSnapshotFact{snapOf(rep0, "g-old", 0)}
		}
	}
	res, err = volumeService(st, map[string]storage.PoolFacts{"data": writer}, map[string]storage.PoolFacts{"tank": node}).List(ctx, volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sync.Level != "error" || !strings.Contains(res.Sync.Message, "对端的目录是空的") {
		t.Fatalf("复制停滞要把原因点出来：%+v", res.Sync)
	}
}

func TestPoolVolumesRequiresAPoolName(t *testing.T) {
	st := seedVolumes(t)
	if _, err := volumeService(st, nil, nil).List(context.Background(), "", " "); err == nil {
		t.Fatal("没有池名应当拒绝")
	}
}

// 单机没有复制可言，页面不该提它。
func TestPoolVolumesOnASingleNodeHasNoReplicationStatus(t *testing.T) {
	st := newImageTestStore(t)
	facts := storage.PoolFacts{Node: volNodeA, Pool: "tank", DataPool: "tank", Used: gib, Available: gib, Datasets: []storage.PoolDatasetFact{{Name: "nd", Type: "filesystem"}}}
	svc := PoolVolumeService{Store: st, NodeID: volNodeA, DataPool: "tank", Now: func() time.Time { return volNow },
		Facts: func(context.Context, string) (storage.PoolFacts, error) { return facts, nil }}
	res, err := svc.List(context.Background(), "", "tank")
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeRole != "single" || res.Sync.Message != "" || !res.Authoritative {
		t.Fatalf("单机不对：%+v", res)
	}
}

func TestPoolFactsCacheServesRepeatedReadsAndNeverCachesFailures(t *testing.T) {
	now := volNow
	reads := 0
	var fail error
	cache := &PoolFactsCache{TTL: 10 * time.Second, Now: func() time.Time { return now },
		Read: func(_ context.Context, pool string) (storage.PoolFacts, error) {
			reads++
			if fail != nil {
				return storage.PoolFacts{}, fail
			}
			return storage.PoolFacts{Pool: pool}, nil
		}}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := cache.Get(ctx, "tank"); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("10 秒内不该重复读池：%d 次", reads)
	}
	if _, err := cache.Get(ctx, "backup"); err != nil || reads != 2 {
		t.Fatalf("不同的池各读各的：%d %v", reads, err)
	}
	now = now.Add(11 * time.Second)
	if _, err := cache.Get(ctx, "tank"); err != nil || reads != 3 {
		t.Fatalf("过期后要重读：%d %v", reads, err)
	}
	fail = errors.New("busy")
	now = now.Add(11 * time.Second)
	if _, err := cache.Get(ctx, "tank"); err == nil {
		t.Fatal("读失败要报错")
	}
	fail = nil
	if _, err := cache.Get(ctx, "tank"); err != nil || reads != 5 {
		t.Fatalf("失败不缓存：%d %v", reads, err)
	}
}

// 花名册地址可能已指向另一台机器：问错了要报出来，不能把别人的池当成这台的。
func TestPoolVolumesRefusesAnAnswerFromTheWrongMachine(t *testing.T) {
	st := seedVolumes(t)
	other := writerFacts()
	other.Node, other.Pool, other.DataPool = "someone-else", "tank", "tank"
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writerFacts()}, map[string]storage.PoolFacts{"tank": other}).List(context.Background(), volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reachable || len(res.Images) != 0 {
		t.Fatalf("把别的机器的池当成了这台：%+v", res)
	}
}

func TestReplicatorReportsItsOwnPullError(t *testing.T) {
	st := newImageTestStore(t)
	ctx := context.Background()
	rep := &Replicator{Store: st, Now: func() time.Time { return volNow }}
	if got := rep.PullError(ctx); got != "" {
		t.Fatalf("没有记录时不该有错误：%q", got)
	}
	if err := st.ReplicationStates().Upsert(ctx, domain.ReplicationState{Target: "http://vip:8080", Kind: "standby-pull", Root: "tank/nd", LastError: "对端的目录是空的", UpdatedAt: volNow}); err != nil {
		t.Fatal(err)
	}
	if got := rep.PullError(ctx); got != "对端的目录是空的" {
		t.Fatalf("备机的拉取错误没有带出来：%q", got)
	}
}

// 池名来自请求参数：读失败的不能留在缓存里，否则查不存在的名字会让它一直变大。
// 删除导入文件后页面要马上看到变化，不能等缓存过期。
func TestPoolFactsCacheForgetDropsWhatItRemembers(t *testing.T) {
	reads := 0
	cache := &PoolFactsCache{TTL: time.Hour, Read: func(_ context.Context, pool string) (storage.PoolFacts, error) {
		reads++
		return storage.PoolFacts{Pool: pool}, nil
	}}
	ctx := context.Background()
	if _, err := cache.Get(ctx, "tank"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(ctx, "data"); err != nil {
		t.Fatal(err)
	}
	cache.Forget()
	for _, pool := range []string{"tank", "data"} {
		if _, err := cache.Get(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 4 {
		t.Fatalf("读了 %d 次，清掉之后每个池都该重读", reads)
	}
}

func TestPoolFactsCacheForgetsPoolsItCouldNotRead(t *testing.T) {
	cache := &PoolFactsCache{TTL: time.Minute, Read: func(context.Context, string) (storage.PoolFacts, error) {
		return storage.PoolFacts{}, errors.New("这台服务器上没有存储池")
	}}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := cache.Get(context.Background(), name); err == nil {
			t.Fatal("读失败要报错")
		}
	}
	if len(cache.entries) != 0 {
		t.Fatalf("读失败的池留在了缓存里：%d 个", len(cache.entries))
	}
}

// 删掉的文件还被复制标记占着：导入文件那一类已经减了，已用还没降，要说明这部分在哪。
func TestSystemListsDeletedFilesStillHeldByMarkers(t *testing.T) {
	const held = 7 * gib
	cases := []struct {
		name  string
		bytes int64
		want  bool
	}{{"平时只有元数据", 393216, false}, {"刚删了大文件", held, true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st := seedVolumes(t)
			facts := writerFacts()
			for i := range facts.Datasets {
				if facts.Datasets[i].Name == "nd/files" {
					facts.Datasets[i].UsedBySnapshots = c.bytes
				}
			}
			res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(ctx, "", "data")
			if err != nil {
				t.Fatal(err)
			}
			var got *VolumeSystemItem
			for i := range res.System {
				if res.System[i].Kind == "deleted_files" {
					got = &res.System[i]
				}
			}
			if (got != nil) != c.want {
				t.Fatalf("有没有这一行不对：%+v", got)
			}
			if got != nil && (got.Used != held || got.Path != "nd/files") {
				t.Fatalf("内容不对：%+v", got)
			}
		})
	}
}

// 主机删了导入文件，这台下一轮同步才跟着删。这期间列表还有它，再点删除什么也不会发生，
// 2026-11-08 用户在备机的池里连删了两次，第二次也提示成功。
func TestPoolVolumesOnAStandbyMarksImportsTheWriterAlreadyDeleted(t *testing.T) {
	st := seedVolumes(t)
	writer := writerFacts()
	node := writerFacts()
	node.Node, node.Pool, node.DataPool = volNodeB, "tank", "tank"
	files := *node.Files
	files.Imports = append(append([]storage.PoolFileFact{}, files.Imports...), storage.PoolFileFact{Name: "CentOS-old.iso", Size: gib, Used: gib})
	node.Files = &files

	res, err := volumeService(st, map[string]storage.PoolFacts{"data": writer}, map[string]storage.PoolFacts{"tank": node}).List(context.Background(), volNodeB, "tank")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, f := range res.Imports {
		states[f.Name] = f.State
	}
	if states["CentOS-old.iso"] != VolumeStatePendingDelete {
		t.Fatalf("主机已删、这台还有的文件应为待复制删除：%v", states)
	}
	if states["Win11.iso"] != "" {
		t.Fatalf("两边都有的文件不该标：%v", states)
	}
	if res.Sync.Pending == 0 {
		t.Fatal("同步状态要算上它")
	}

	// 写入者自己的池不和别人比。
	own, err := volumeService(st, map[string]storage.PoolFacts{"data": writer}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range own.Imports {
		if f.State != "" {
			t.Fatalf("写入者的文件不该标：%+v", f)
		}
	}
}

// 开机时系统盘从身份层克隆。身份层是内部的中间层，操作者认的是还原点，要再往上找一级。
func TestPoolVolumesSaysWhichRestorePointABootedClientCameFrom(t *testing.T) {
	st := seedVolumes(t)
	facts := writerFacts()
	sys := ds("run/CLIENT-0050562EF65F", 300<<20)
	sys.Origin = "run/IDENT-0050562EF65F@ready"
	data := ds("run/CLIENT-0050562EF65F-DATA-1", 4<<20)
	data.Origin = "nd/data_default@0"
	orphan := ds("run/CLIENT-AABBCCDDEEFF", 8192)
	orphan.Origin = "run/IDENT-AABBCCDDEEFF@ready" // 身份层已经不在了
	facts.Datasets = append(facts.Datasets, sys, data, orphan)
	res, err := volumeService(st, map[string]storage.PoolFacts{"data": facts}, nil).List(context.Background(), "", "data")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, c := range res.Clients {
		for _, d := range c.Disks {
			sources[d.Dataset] = d.Source
		}
	}
	if got := sources["run/CLIENT-0050562EF65F"]; got != "win11_25h2 @2" {
		t.Fatalf("系统盘要说出还原点，不是身份层的名字：%q", got)
	}
	if got := sources["run/CLIENT-0050562EF65F-DATA-1"]; got != "data @0" {
		t.Fatalf("数据盘来源不对：%q", got)
	}
	if got := sources["run/CLIENT-AABBCCDDEEFF"]; got != "网卡层" {
		t.Fatalf("身份层不在时也不能露出内部名字：%q", got)
	}
}
