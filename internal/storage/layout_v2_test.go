package storage

import "testing"

// 目录（镜像、配置及其快照）在节点间复制，客户机克隆不复制；靠名字所属容器才能用一条 `zfs send -R` 分开。
func TestContainerForSplitsCatalogueFromRuntime(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"win11", CatalogueRoot},
		{"win11_default", CatalogueRoot},
		{"win11_default_before_super", CatalogueRoot},
		{"CLIENT-AABBCCDDEEFF", RunRoot},
		{"CLIENT-AABBCCDDEEFF-DATA-1", RunRoot},
		{"SCLIENT-AABBCCDDEEFF", RunRoot},
		{"INSPECT-WIN11-123", RunRoot},
	}
	for _, tc := range cases {
		if got := ContainerFor(tc.name); got != tc.want {
			t.Errorf("ContainerFor(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 复制和备份标记只用于记账：列还原点时不得展示，恢复时不得收编。
func TestSystemSnapshotRecognizesMarkers(t *testing.T) {
	for _, name := range []string{"rep-1755600000", "ndbackup-20260819"} {
		if !SystemSnapshot(name) {
			t.Errorf("SystemSnapshot(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"0", "point", "2026.08.19", "repair-done", "final"} {
		if SystemSnapshot(name) {
			t.Errorf("SystemSnapshot(%q) = true, want false", name)
		}
	}
}

// db 和 files 是目录容器自己的子项（数据库副本、驱动库），扫描池时不得误当成镜像收编。
func TestReservedCatalogueChildren(t *testing.T) {
	for _, name := range []string{"db", "files"} {
		if !ReservedCatalogueChild(name) {
			t.Errorf("ReservedCatalogueChild(%q) = false, want true", name)
		}
	}
	if ReservedCatalogueChild("win11") {
		t.Error("ReservedCatalogueChild(win11) = true, want false")
	}
}
