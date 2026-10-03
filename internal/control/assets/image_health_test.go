package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

func TestImportImageAutoTriggersHealthCheck(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeImageStorage{now: now, inspectReport: storage.InspectReport{
		Level:          domain.HealthWarn,
		PartitionStyle: "gpt",
		Items:          []storage.InspectItem{{Name: "bcd", Level: domain.HealthWarn, Detail: "BCD 缺失"}},
	}}
	service := ImageService{Store: st, Storage: fake, Now: func() time.Time { return now }}

	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}
	if fake.inspectReq.ImageID != "win11" || fake.inspectReq.ConfigID != "win11_default" || fake.inspectReq.SnapshotName != "0" {
		t.Fatalf("inspect req = %#v", fake.inspectReq)
	}
	report, err := st.ImageHealthReports().GetByImage(ctx, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if report.Level != domain.HealthWarn || report.PartitionStyle != "gpt" || len(report.Items) != 1 {
		t.Fatalf("report = %#v", report)
	}
	tasks, err := st.Tasks().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var healthTask *domain.Task
	for i, task := range tasks {
		if task.Type == domain.TaskTypeImageHealthCheck {
			healthTask = &tasks[i]
		}
	}
	if healthTask == nil || healthTask.Status != domain.TaskStatusSuccess || healthTask.Result != "warn" {
		t.Fatalf("health task = %#v", healthTask)
	}
}

func TestRunHealthCheckOverwritesPreviousReport(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeImageStorage{now: now, inspectReport: storage.InspectReport{Level: domain.HealthBlock, Items: []storage.InspectItem{{Name: "msiscsi", Level: domain.HealthBlock, Detail: "msiscsi.sys 缺失"}}}}
	service := ImageService{Store: st, Storage: fake, Now: func() time.Time { return now }}
	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}

	fake.inspectReport = storage.InspectReport{Level: domain.HealthOK, PartitionStyle: "mbr"}
	if _, err := service.RunHealthCheck(ctx, "win11"); err != nil {
		t.Fatal(err)
	}
	report, err := st.ImageHealthReports().GetByImage(ctx, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if report.Level != domain.HealthOK || report.PartitionStyle != "mbr" {
		t.Fatalf("report = %#v", report)
	}
}

func TestRunHealthCheckStoresTheNICsTheImageHasDriversFor(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeImageStorage{now: now}
	service := ImageService{Store: st, Storage: fake, Now: func() time.Time { return now }}
	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}
	fake.inspectReport = storage.InspectReport{Level: domain.HealthOK, NICPCIIDs: []string{"8086:15B8"}}
	if _, err := service.RunHealthCheck(ctx, "win11"); err != nil {
		t.Fatal(err)
	}
	report, err := st.ImageHealthReports().GetByImage(ctx, "win11")
	if err != nil || len(report.NICPCIIDs) != 1 || report.NICPCIIDs[0] != "8086:15B8" {
		t.Fatalf("report = %#v err=%v", report, err)
	}
}

// 数据盘没有系统，体检只会产出「找不到系统分区」的阻断报告并告警，应拒绝并清掉旧报告。
func TestRunHealthCheckRefusesADataDiskAndDropsItsStaleReport(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "steam", Name: "Steam 游戏库", OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeData, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImageHealthReports().Create(ctx, domain.ImageHealthReport{ID: "health-steam", ImageID: "steam", Level: domain.HealthBlock, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeImageStorage{now: now}
	_, err := ImageService{Store: st, Storage: fake}.RunHealthCheck(ctx, "steam")
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "Steam 游戏库") {
		t.Fatalf("err = %v", err)
	}
	if fake.inspectReq.ImageID != "" {
		t.Fatal("a data disk was inspected")
	}
	if _, err := st.ImageHealthReports().GetByImage(ctx, "steam"); !errs.IsNotFound(err) {
		t.Fatalf("stale report kept: %v", err)
	}
}

func TestSetPurposeToDataDropsTheHealthReport(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Now().UTC()
	if err := st.Images().Create(ctx, domain.Image{ID: "games", Name: "游戏盘", OSType: domain.OSTypeWindows, Purpose: domain.ImagePurposeSystem, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImageHealthReports().Create(ctx, domain.ImageHealthReport{ID: "health-games", ImageID: "games", Level: domain.HealthBlock, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := (ImageService{Store: st}).SetPurpose(ctx, "games", SetPurposeRequest{Purpose: string(domain.ImagePurposeData)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImageHealthReports().GetByImage(ctx, "games"); !errs.IsNotFound(err) {
		t.Fatalf("report kept for a data disk: %v", err)
	}
}

func TestGetHealthReturnsReportAndMissing(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	service := ImageService{Store: st, Storage: &fakeImageStorage{now: now}, Now: func() time.Time { return now }}
	if _, err := service.ImportImage(ctx, ImportImageRequest{Name: "win11", SourcePath: "/var/lib/ndiskless/imports/win11.zfs", OSType: domain.OSTypeWindows}); err != nil {
		t.Fatal(err)
	}

	report, err := service.GetHealth(ctx, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if report.ImageID != "win11" || report.Level != domain.HealthOK {
		t.Fatalf("report = %#v", report)
	}
	if _, err := service.GetHealth(ctx, "missing"); err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestGroupBindingRejectsBlockLevelImage(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	seedHealthImage(t, ctx, st, now)

	groups := GroupService{Store: st, Now: func() time.Time { return now }}
	req := GroupRequest{
		Name: "g1", StartIP: "192.168.1.10", ClientMax: 10,
		Gateway: "192.168.1.1", Netmask: "255.255.255.0",
		SystemImageID: "win11", SystemConfigID: "win11_default",
	}

	// 阻断级拒绝绑定，错误中带体检结论。
	mustPutHealthReport(t, ctx, st, domain.ImageHealthReport{
		ID: "health-win11", ImageID: "win11", Level: domain.HealthBlock,
		Items:     []domain.HealthCheckItem{{Name: "msiscsi", Level: domain.HealthBlock, Detail: "msiscsi.sys 缺失"}},
		CreatedAt: now,
	})
	if _, err := groups.Create(ctx, req); !errors.Is(err, ErrImageHealthBlocked) {
		t.Fatalf("err = %v", err)
	}

	// 警告级放行。
	report, err := st.ImageHealthReports().GetByImage(ctx, "win11")
	if err != nil {
		t.Fatal(err)
	}
	report.Level = domain.HealthWarn
	if err := st.ImageHealthReports().Update(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.Create(ctx, req); err != nil {
		t.Fatalf("warn level should pass: %v", err)
	}
}

func TestGroupBindingWithoutReportPasses(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	seedHealthImage(t, ctx, st, now)

	groups := GroupService{Store: st, Now: func() time.Time { return now }}
	if _, err := groups.Create(ctx, GroupRequest{
		Name: "g1", StartIP: "192.168.1.10", ClientMax: 10,
		Gateway: "192.168.1.1", Netmask: "255.255.255.0",
		SystemImageID: "win11", SystemConfigID: "win11_default",
	}); err != nil {
		t.Fatalf("no report should pass: %v", err)
	}
}

func seedHealthImage(t *testing.T, ctx context.Context, st *store.SQLStore, now time.Time) {
	t.Helper()
	redID := "win11_0"
	if err := st.Images().Create(ctx, domain.Image{ID: "win11", Name: "win11", OSType: domain.OSTypeWindows, State: domain.ImageStateNormal, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Configs().Create(ctx, domain.Config{ID: "win11_default", ImageID: "win11", Name: "default", DefaultReductionID: &redID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reductions().Create(ctx, domain.Reduction{ID: redID, ConfigID: "win11_default", Name: "@0", CreatedAt: now, Status: domain.ReductionStatusReady}); err != nil {
		t.Fatal(err)
	}
}

func mustPutHealthReport(t *testing.T, ctx context.Context, st *store.SQLStore, report domain.ImageHealthReport) {
	t.Helper()
	if err := st.ImageHealthReports().Create(ctx, report); err != nil {
		t.Fatal(err)
	}
}
