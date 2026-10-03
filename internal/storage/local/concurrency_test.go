package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// 同一台超管机的两次保存（连点保存、或保存撞上数据盘发布）都会 promote、改名并共用暂存名
// <配置>_before_super；交错时配置会从池里消失，客户机开不了机。保存必须按客户机串行，
// 第二次等第一次做完再按当时状态决定是否执行。
func TestTwoSuperSavesOfTheSameMachineDoNotInterleave(t *testing.T) {
	ctx := context.Background()
	for _, at := range []string{
		"promote tank/run/SCLIENT-AABBCCDDEEFF",
		"rename tank/nd/cfg",
		"rename tank/run/SCLIENT-AABBCCDDEEFF",
		"snapshot tank/nd/cfg@v1",
		"destroy tank/nd/cfg_before_super",
	} {
		t.Run(at, func(t *testing.T) {
			z := seedSuperPool()
			agent := New("server-a", z)
			secondDone := make(chan error, 1)
			var arm func(op string)
			arm = func(op string) {
				if !strings.HasPrefix(op, at) {
					z.beforeOp = arm
					return
				}
				go func() {
					second := superStopReq()
					second.ReductionName = "@v2"
					_, err := agent.SuperStop(ctx, second)
					secondDone <- err
				}()
				// 第一次停在这一步：没有互斥的话，第二次会趁这时做完
				select {
				case err := <-secondDone:
					secondDone <- err
				case <-time.After(200 * time.Millisecond):
				}
			}
			z.beforeOp = arm
			_, firstErr := agent.SuperStop(ctx, superStopReq())
			secondErr := <-secondDone

			state := strings.Join(z.state(), " ")
			if !strings.Contains(state, "tank/nd/cfg@") || strings.Contains(state, "_before_super") || strings.Contains(state, "SCLIENT") {
				t.Fatalf("交错后池不完整：%s（第一次 err=%v，第二次 err=%v）", state, firstErr, secondErr)
			}
			if firstErr != nil || secondErr == nil {
				t.Fatalf("应当第一次保存成功、第二次因没有可保存的超管盘而失败：第一次 err=%v，第二次 err=%v", firstErr, secondErr)
			}
		})
	}
}

// 回收决定之后、动手之前客户机开机了：回收若照删会删掉刚建的克隆。回收须在客户机锁内再确认一次。
func TestReclaimSkipsAClientThatBootedAfterTheDecision(t *testing.T) {
	ctx := context.Background()
	z := poolWith("tank/nd/cfg")
	z.snaps["tank/nd/cfg@0"] = true
	agent := New("server-a", z)
	agent.zvolMountsFn = func() []zfs.ZvolMount { return nil }
	decided := time.Now()
	if _, err := agent.CreateClientLUN(ctx, storage.ClientReq{MAC: "aa:bb:cc:dd:ee:ff", System: storage.ClientSource{ConfigID: "cfg", SnapshotName: "0"}}); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := agent.ReclaimIdleClientClones(ctx, "aa:bb:cc:dd:ee:ff", decided)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed || !strings.Contains(strings.Join(z.state(), " "), "CLIENT-AABBCCDDEEFF") {
		t.Fatalf("刚开机的客户机克隆被回收了：reclaimed=%v，池=%v", reclaimed, z.state())
	}

	// 决定晚于这次开机：照常回收
	reclaimed, err = agent.ReclaimIdleClientClones(ctx, "aa:bb:cc:dd:ee:ff", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed || strings.Contains(strings.Join(z.state(), " "), "CLIENT-AABBCCDDEEFF") {
		t.Fatalf("早已关机的客户机克隆应被回收：reclaimed=%v，池=%v", reclaimed, z.state())
	}
}
