package ha

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 写入者挂了、由落后的备机接任，复制随后会把别处已有的内容（如还原点）回滚掉。所以接任前要比较：
// 可联系的对端里只要有一台比本机新，就不接任，并把 VIP 让给数据更新的那台。
func TestActivateRefusedWhenAPeerHoldsANewerCatalogue(t *testing.T) {
	c, exits := newTestController(t)
	c.StepDownFile = filepath.Join(t.TempDir(), "step-down")
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 3})
	c.CatalogueMark = func(context.Context) string { return "rep-100" }
	c.HoldsVIP = func() bool { return true }
	// 让给它的前提是它接得住：健康才拿得到 VIP
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "node-b", Role: "standby", Epoch: 3, Healthy: true, Mark: "rep-200"}}

	err := c.Activate(context.Background(), "keepalived master")
	if err == nil || !strings.Contains(err.Error(), "落后") || !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("应拒绝并点名更新的那台: %v", err)
	}
	if len(*exits) != 0 {
		t.Fatal("不该重启成主机")
	}
	if !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatal("手里有虚 IP 就要让出去，否则谁也不服务")
	}
}

// 对端是健康的主机、正在接受写入，只是 VIP 刚被本机拿走。那几秒的写入还没进复制标记，
// 比标记看不出来；健康的主机还在，就不抢。
func TestActivateRefusedWhileAHealthyWriterAnswers(t *testing.T) {
	c, exits := newTestController(t)
	c.StepDownFile = filepath.Join(t.TempDir(), "step-down")
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 173})
	c.CatalogueMark = func(context.Context) string { return "rep-100" }
	c.HoldsVIP = func() bool { return true }
	c.ClusterPeers = func(context.Context) []PeerStatusClient {
		return []PeerStatusClient{
			fakePeerStatus{status: PeerStatus{NodeID: "node-c", Role: "active", Epoch: 173, Healthy: true, Mark: "rep-100"}},
			fakePeerStatus{status: PeerStatus{NodeID: "node-b", Role: "standby", Epoch: 173, Mark: "rep-100"}},
		}
	}

	err := c.Activate(context.Background(), "self-heal: VIP held while standby")
	if err == nil || !strings.Contains(err.Error(), "node-c") {
		t.Fatalf("健康的主机还在，应拒绝并点名它: %v", err)
	}
	if len(*exits) != 0 || !StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatalf("不该接任，且要让出虚 IP：exits=%v", *exits)
	}
}

// 主机还在但已不健康（库写不进、池坏了）：它服务不了客户机，接任是对的，不能因为它还能应答就让集群没有写入者。
func TestActivateProceedsOverAnUnhealthyWriter(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	c.CatalogueMark = func(context.Context) string { return "rep-100" }
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "node-b", Role: "active", Epoch: 5, Healthy: false, Mark: "rep-100"}}

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != 1 {
		t.Fatalf("应接任: exits=%v", *exits)
	}
}

// 双方一样新（计划切换排空后就是这样）：照常接任。
func TestActivateProceedsWhenNoPeerIsNewer(t *testing.T) {
	c, exits := newTestController(t)
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	c.CatalogueMark = func(context.Context) string { return "rep-200" }
	c.Peer = fakePeerStatus{status: PeerStatus{NodeID: "node-b", Role: "standby", Epoch: 5, Mark: "rep-200"}}

	if err := c.Activate(context.Background(), "planned switch"); err != nil {
		t.Fatal(err)
	}
	if len(*exits) != 1 {
		t.Fatalf("应接任: exits=%v", *exits)
	}
}

// 写入者挂了、谁也联系不上，本机的 DB 副本里有某配置而池里没有。最常见的原因是写入者刚删了它：
// 删除随复制流先到，DB 副本下一轮才跟上。此时拒绝的话每台备机都会拒绝，集群再无主机；
// 所以接任并记下缺了什么，没有谁比本机更完整，等下去也不会变好。
func TestActivateProceedsWhenIncompleteAndNobodyAnswers(t *testing.T) {
	c, exits := newTestController(t)
	dir := t.TempDir()
	c.StepDownFile = filepath.Join(dir, "step-down")
	c.ReportFile = filepath.Join(dir, "ha-takeover.json")
	_ = WriteRoleState(c.RoleFile, RoleState{Role: "standby", Epoch: 5})
	c.HoldsVIP = func() bool { return true }
	c.CatalogueIncomplete = func(context.Context) []string { return []string{"配置「办公」"} }

	if err := c.Activate(context.Background(), "keepalived master"); err != nil {
		t.Fatalf("没有更完整的节点可等，应接任: %v", err)
	}
	if len(*exits) != 1 || StepDownActive(c.StepDownFile, time.Minute) {
		t.Fatalf("应接任且不让出虚 IP：exits=%v", *exits)
	}
	report, ok := ReadTakeoverReport(c.ReportFile)
	if !ok || len(report.Missing) != 1 || report.Missing[0] != "配置「办公」" {
		t.Fatalf("接任记录应列出缺少的内容: %+v ok=%v", report, ok)
	}
}
