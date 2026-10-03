package assets

import (
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func refIndexFixture() refIndex {
	return refIndex{
		groups: []domain.Group{
			{ID: "g1", SystemImageID: "img1", SystemConfigID: "cfg1", SystemReductionID: "red1"},
		},
		disks: []domain.GroupDisk{
			{ID: "d1", GroupID: "g1", ImageID: "img2", ConfigID: "cfg2"},
		},
		clones: []domain.ClientClone{
			{ID: "c1", TerminalMAC: "AABBCCDDEEFF", ConfigID: "cfg3", ReductionID: "red3"},
			{ID: "c2", TerminalMAC: "AABBCCDDEEFF", ConfigID: "cfg3", ReductionID: "red4"},
			{ID: "c3", TerminalMAC: "001122334455", ConfigID: "cfg4", ReductionID: "red5"},
		},
	}
}

func TestRefIndexConfigInUse(t *testing.T) {
	ix := refIndexFixture()
	for id, want := range map[string]bool{
		"cfg1": true,  // a group boots it
		"cfg2": true,  // a group disk binds it
		"cfg3": true,  // a client clone holds it
		"cfg9": false, // unreferenced
	} {
		if ix.configInUse(id) != want {
			t.Fatalf("configInUse(%s) != %v", id, want)
		}
	}
}

func TestRefIndexReductionInUse(t *testing.T) {
	ix := refIndexFixture()
	for id, want := range map[string]bool{
		"red1": true,  // a group's system reduction
		"red4": true,  // a clone holds it
		"red9": false, // unreferenced
	} {
		if ix.reductionInUse(id) != want {
			t.Fatalf("reductionInUse(%s) != %v", id, want)
		}
	}
}

// describeImageUsers 兼作在用判断：非空即拒绝删除，内容就是告诉操作者要处理的对象。
func TestRefIndexDescribeImageUsers(t *testing.T) {
	ix := refIndexFixture()
	for _, tc := range []struct {
		name       string
		id         string
		configIDs  []string
		reductions []string
		wantUsed   bool
	}{
		{"分组直接引用", "img1", nil, nil, true},
		{"数据盘引用", "img2", nil, nil, true},
		{"克隆挂在它的配置上", "img3", []string{"cfg3"}, nil, true},
		{"克隆挂在它的还原点上", "img3", []string{"cfgX"}, []string{"red5"}, true},
		{"无人引用", "img3", []string{"cfgX"}, []string{"redX"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			users := ix.describeImageUsers(tc.id, tc.configIDs, tc.reductions)
			if (len(users) > 0) != tc.wantUsed {
				t.Fatalf("users = %#v, wantUsed=%v", users, tc.wantUsed)
			}
			for _, u := range users {
				if !strings.HasPrefix(u, "分组 ") && !strings.HasPrefix(u, "客户机 ") {
					t.Fatalf("user %q should name a group or a machine", u)
				}
			}
		})
	}
}

func TestClonesOfMAC(t *testing.T) {
	ix := refIndexFixture()
	got := clonesOfMAC(ix.clones, "AABBCCDDEEFF")
	if len(got) != 2 || got[0].ID != "c1" || got[1].ID != "c2" {
		t.Fatalf("clonesOfMAC = %#v", got)
	}
	if rest := clonesOfMAC(ix.clones, "FFFFFFFFFFFF"); rest != nil {
		t.Fatalf("no-match should be nil, got %#v", rest)
	}
}

// 每类阻塞各有指引：健康检查会自己结束、fork 配置要删、运行中的机器要关，不能统一写成「请删除这些」。
func TestExplainBlockersGivesEachKindItsOwnInstruction(t *testing.T) {
	ctx := t.Context()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "img-1", Name: "win", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "cfg-fork", ImageID: "img-1", Name: "美术教室", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		datasets []string
		want     string
	}{
		{
			name:     "health check",
			datasets: []string{"tank/INSPECT-WIN11-1785233086610422533"},
			want:     "镜像体检正在进行，请稍后重试",
		},
		{
			name:     "forked config",
			datasets: []string{"tank/cfg-fork"},
			want:     "请先删除由该配置派生出的配置：美术教室",
		},
		{
			name:     "running client",
			datasets: []string{"tank/CLIENT-AABBCCDDEEFF", "tank/CLIENT-AABBCCDDEEFF-DATA-1"},
			want:     "请先关闭正在使用的客户机：AA:BB:CC:DD:EE:FF",
		},
		{
			name:     "super machine",
			datasets: []string{"tank/SCLIENT-001122334455"},
			want:     "请先关闭正在使用的超管机：00:11:22:33:44:55",
		},
		{
			name:     "several kinds at once",
			datasets: []string{"tank/INSPECT-X-1", "tank/cfg-fork", "tank/CLIENT-AABBCCDDEEFF"},
			want:     "镜像体检正在进行，请稍后重试；请先删除由该配置派生出的配置：美术教室；请先关闭正在使用的客户机：AA:BB:CC:DD:EE:FF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explainBlockers(ctx, st, tc.datasets, "该配置"); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
