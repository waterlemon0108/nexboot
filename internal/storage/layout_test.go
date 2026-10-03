package storage

import "testing"

// 命名与反向识别必须一致：这里生成的每个名字都要能读回。
func TestNamingRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want Carrier
	}{
		{
			name: "客户机系统盘",
			got:  ClientCloneName("aa:bb:cc:dd:ee:ff"),
			want: Carrier{Kind: CarrierClient, MAC: "AABBCCDDEEFF", LUN: 0},
		},
		{
			name: "客户机数据盘",
			got:  ClientDataCloneName("aa:bb:cc:dd:ee:ff", 2),
			want: Carrier{Kind: CarrierClient, MAC: "AABBCCDDEEFF", LUN: 2},
		},
		{
			name: "超管机克隆",
			got:  SuperClientCloneName("00-11-22-33-44-55"),
			want: Carrier{Kind: CarrierSuper, MAC: "001122334455", LUN: 0},
		},
		{
			name: "被顶替的配置",
			got:  SupersededConfigName("img_default"),
			want: Carrier{Kind: CarrierSuperseded},
		},
		{
			name: "默认配置",
			got:  DefaultConfigName("win11"),
			want: Carrier{Kind: CarrierEntity},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Classify(tc.got)
			if c.Kind != tc.want.Kind || c.MAC != tc.want.MAC || c.LUN != tc.want.LUN {
				t.Fatalf("Classify(%q) = %+v, want kind=%s mac=%q lun=%d",
					tc.got, c, tc.want.Kind, tc.want.MAC, tc.want.LUN)
			}
			if c.Name != tc.got {
				t.Fatalf("name = %q, want it preserved", c.Name)
			}
		})
	}
}

// 健康检查的临时克隆必须能被识别：它会在导入后挡住合并几秒，操作者该看到「稍候」而非「删掉」。
func TestInspectCloneIsRecognisable(t *testing.T) {
	name := InspectCloneName("win11")
	if c := Classify(name); c.Kind != CarrierInspect {
		t.Fatalf("Classify(%q) = %s, want inspect", name, c.Kind)
	}
}

// 超管前缀包含客户机前缀，反向识别时判断顺序不能错。
func TestSuperCloneIsNotMistakenForAClientClone(t *testing.T) {
	if c := Classify(SuperClientCloneName("aabbccddeeff")); c.Kind != CarrierSuper {
		t.Fatalf("kind = %s, want super", c.Kind)
	}
}

func TestImageNameFoldsToASCIIAndStaysReadable(t *testing.T) {
	free := func(string) bool { return false }
	for name, want := range map[string]string{
		"win11":      "win11",
		"Win 11 x64": "win-11-x64",
		"教学镜像win11":  "_win11",
	} {
		if got := ImageName(name, free); got != want {
			t.Fatalf("ImageName(%q) = %q, want %q", name, got, want)
		}
	}
	// 折叠后无可读内容时用兜底名，"____" 这种数据集名没法看。
	if got := ImageName("教学镜像", free); got != "image" {
		t.Fatalf("all-CJK name gave %q", got)
	}
	// 不同名字可能折叠成同一串，第二个不得占用第一个的 ID。
	taken := func(id string) bool { return id == "_win" || id == "_win_default" }
	if got := ImageName("教学镜像win", taken); got == "_win" {
		t.Fatalf("collided with the existing dataset: %q", got)
	}
	for _, r := range ImageName("教学镜像", free) {
		if r > 127 {
			t.Fatal("id must stay ascii")
		}
	}
}

func TestDatasetIDStripsThePool(t *testing.T) {
	if got := DatasetID("tank/img_default"); got != "img_default" {
		t.Fatalf("got %q", got)
	}
	if got := DatasetID("img_default"); got != "img_default" {
		t.Fatalf("bare name should pass through, got %q", got)
	}
}

// 还原点显示名原样保留（如 "装完office"），承载它的快照名必须是 ascii，与镜像同理。
func TestSnapshotNameFoldsToASCIIAndAvoidsCollisions(t *testing.T) {
	free := func(string) bool { return false }
	for display, want := range map[string]string{
		"r1":          "r1",
		"装完office":    "office", // the fold leaves padding; it carries nothing
		"2026.07.28":  "2026.07.28",
		"2026Q3-驱动更新": "2026q3",
		"装完 office":   "office",
	} {
		if got := SnapshotName(display, free); got != want {
			t.Fatalf("SnapshotName(%q) = %q, want %q", display, got, want)
		}
	}
	// 折叠后无可读内容。
	if got := SnapshotName("装完了", free); got != "point" {
		t.Fatalf("all-CJK name gave %q", got)
	}
	// 折叠成同一串的两个名字不得落到同一快照上，且后缀要可读（会出现在 `zfs list` 里）。
	taken := func(name string) bool { return name == "office" }
	if got := SnapshotName("装完office", taken); got != "office-2" {
		t.Fatalf("collision gave %q", got)
	}
	for _, r := range SnapshotName("装完office", free) {
		if r > 127 {
			t.Fatal("snapshot name must stay ascii")
		}
	}
}

// 导出用的临时 ndexport- 快照与复制、备份标记一样只用于记账，绝不是还原点。
func TestExportSnapshotIsSystemSnapshot(t *testing.T) {
	if !SystemSnapshot(ExportSnapshotPrefix + "1755600000000000000") {
		t.Fatal("ndexport- snapshot must be bookkeeping")
	}
	if SystemSnapshot("装完office") || SystemSnapshot("0") {
		t.Fatal("restore points must stay visible")
	}
}

func TestExportScratchCloneLivesUnderRunAndIsNotAnEntity(t *testing.T) {
	name := ExportCloneName("vmdk_default")
	if c := Classify(name); c.Kind != CarrierExport {
		t.Fatalf("classify %q = %s", name, c.Kind)
	}
	if ContainerFor(name) != RunRoot {
		t.Fatalf("export scratch clone must not be replicated: container = %s", ContainerFor(name))
	}
}

// 适配结果回读的临时克隆与体检克隆同类：只活几秒、落在 run/ 下、不随复制和备份离开本节点。
func TestAdaptCheckCloneIsATransientRunCarrier(t *testing.T) {
	name := AdaptCheckCloneName("aa:bb:cc:dd:ee:ff")
	if name != "NDADAPTCHK-AABBCCDDEEFF" {
		t.Fatalf("name = %q", name)
	}
	c := Classify(name)
	if c.Kind != CarrierInspect || c.MAC != "AABBCCDDEEFF" {
		t.Fatalf("classified as %#v, want an inspect carrier of that machine", c)
	}
	if ContainerFor(name) != RunRoot {
		t.Fatalf("container = %q, want %q", ContainerFor(name), RunRoot)
	}
}
