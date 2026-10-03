package adapt

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

const driverTestINF = `[Version]
Class = Net
Provider = %V%
CatalogFile = e1d.cat
DriverVer = 03/15/2024,12.19.2.60

[Manufacturer]
%V% = Intel, NTamd64

[Intel.NTamd64]
%DESC% = E1D.ndi, PCI\VEN_8086&DEV_15B7
%DESC2% = E1D.ndi, PCI\VEN_8086&DEV_15B8

[Strings]
V = "Intel"
DESC = "I219-V"
DESC2 = "I219-LM"
`

func driverTestZip(t *testing.T, withCat bool, infs map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range infs {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if withCat {
		f, err := w.Create("e1d.cat")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("catalog")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newDriverService(t *testing.T) DriverService {
	t.Helper()
	return DriverService{
		Store: newImageTestStore(t),
		Dir:   t.TempDir(),
		Now:   func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) },
	}
}

func TestDriverUploadParsesINF(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})

	item, err := service.UploadPack(ctx, UploadDriverPackRequest{
		Filename: "intel-i219.zip",
		Category: domain.DriverCategoryBootCriticalNIC,
		OSType:   domain.OSTypeWindows,
		Data:     data,
	})
	if err != nil {
		t.Fatal(err)
	}
	pack := item.Pack
	if pack.Vendor != "Intel" || pack.Version != "12.19.2.60" || pack.ReleaseDate != "03/15/2024" {
		t.Fatalf("pack = %#v", pack)
	}
	if !pack.Signed {
		t.Fatal("expected signed pack (cat present)")
	}
	if len(pack.HWIDs) != 2 {
		t.Fatalf("hwids = %v", pack.HWIDs)
	}
	if pack.Name != "intel-i219" {
		t.Fatalf("name = %q", pack.Name)
	}
	if len(item.Risks) != 0 {
		t.Fatalf("risks = %v", item.Risks)
	}

	list, err := service.ListPacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Items[0].Pack.ID != pack.ID {
		t.Fatalf("list = %#v", list)
	}
}

func TestDriverUploadRejectsInvalidArchives(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)

	if _, err := service.UploadPack(ctx, UploadDriverPackRequest{Data: []byte("not a zip")}); !errors.Is(err, ErrDriverPackInvalid) {
		t.Fatalf("err = %v", err)
	}
	noINF := driverTestZip(t, false, map[string]string{"readme.txt": "hello"})
	if _, err := service.UploadPack(ctx, UploadDriverPackRequest{Data: noINF}); !errors.Is(err, ErrDriverPackInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestDriverUnsignedPackFlagged(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, false, map[string]string{"e1d.inf": driverTestINF})

	item, err := service.UploadPack(ctx, UploadDriverPackRequest{Data: data, Category: domain.DriverCategoryNIC})
	if err != nil {
		t.Fatal(err)
	}
	if item.Pack.Signed {
		t.Fatal("expected unsigned")
	}
	found := false
	for _, r := range item.Risks {
		if r == "unsigned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("risks = %v", item.Risks)
	}
}

func TestDriverRecommendedUniquePerOverlap(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})

	a, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "v1", Category: domain.DriverCategoryBootCriticalNIC, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return time.Date(2026, 7, 2, 13, 0, 0, 0, time.UTC) }
	b, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "v2", Category: domain.DriverCategoryBootCriticalNIC, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetRecommended(ctx, a.Pack.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetRecommended(ctx, b.Pack.ID); err != nil {
		t.Fatal(err)
	}
	packA, err := service.Store.DriverPacks().Get(ctx, a.Pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	packB, err := service.Store.DriverPacks().Get(ctx, b.Pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if packA.Recommended || !packB.Recommended {
		t.Fatalf("recommended a=%v b=%v", packA.Recommended, packB.Recommended)
	}
}

func TestDriverBundleLifecycle(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})

	nic, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "nic", Category: domain.DriverCategoryBootCriticalNIC, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return time.Date(2026, 7, 2, 13, 0, 0, 0, time.UTC) }
	gpu, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "gpu", Category: domain.DriverCategoryGPU, Data: data})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateBundle(ctx, DriverBundleRequest{Name: "bad", PackIDs: []string{gpu.Pack.ID}}); !errors.Is(err, ErrDriverPackCategory) {
		t.Fatalf("err = %v", err)
	}
	bundle, err := service.CreateBundle(ctx, DriverBundleRequest{Name: "boot-set", PackIDs: []string{nic.Pack.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Bundle.OSType != domain.OSTypeWindows || len(bundle.Packs) != 1 {
		t.Fatalf("bundle = %#v", bundle)
	}

	// 被驱动包组引用的包不能删除
	if err := service.DeletePack(ctx, nic.Pack.ID); !errors.Is(err, ErrDriverPackInUse) {
		t.Fatalf("err = %v", err)
	}

	name, archive, err := service.BundleArchive(ctx, bundle.Bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name != "boot-set.zip" {
		t.Fatalf("archive name = %q", name)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	foundINF := false
	for _, f := range reader.File {
		if f.Name == "nic/e1d.inf" {
			foundINF = true
		}
	}
	if !foundINF {
		names := []string{}
		for _, f := range reader.File {
			names = append(names, f.Name)
		}
		t.Fatalf("archive entries = %v", names)
	}

	if err := service.DeleteBundle(ctx, bundle.Bundle.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePack(ctx, nic.Pack.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDriverPackStatusToggle(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})
	item, err := service.UploadPack(ctx, UploadDriverPackRequest{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if item.Pack.Category != domain.DriverCategoryNIC {
		t.Fatalf("inferred category = %s", item.Pack.Category)
	}
	updated, err := service.SetPackStatus(ctx, item.Pack.ID, domain.DriverPackDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != domain.DriverPackDisabled {
		t.Fatalf("status = %s", updated.Status)
	}
	if _, err := service.SetPackStatus(ctx, item.Pack.ID, "nope"); !errors.Is(err, ErrDriverStatusInvalid) {
		t.Fatalf("err = %v", err)
	}
}

// 禁用必须在服务端生效：直接调 API 也不能把已禁用的包放进驱动包组注入超管机。
func TestCreateBundleRejectsDisabledPack(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})

	nic, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "nic", Category: domain.DriverCategoryBootCriticalNIC, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetPackStatus(ctx, nic.Pack.ID, domain.DriverPackDisabled); err != nil {
		t.Fatal(err)
	}

	_, err = service.CreateBundle(ctx, DriverBundleRequest{Name: "boot-set", PackIDs: []string{nic.Pack.ID}})
	if !errors.Is(err, ErrDriverPackDisabled) {
		t.Fatalf("err = %v, want the disabled pack to be refused", err)
	}
}

// 包文件位置由 id 推导而非存绝对路径，驱动目录挪动或数据库恢复到别的主机后仍能找到。
func TestDriverPackSurvivesDirectoryMove(t *testing.T) {
	ctx := context.Background()
	service := newDriverService(t)
	data := driverTestZip(t, true, map[string]string{"e1d.inf": driverTestINF})

	nic, err := service.UploadPack(ctx, UploadDriverPackRequest{Name: "nic", Category: domain.DriverCategoryBootCriticalNIC, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := service.CreateBundle(ctx, DriverBundleRequest{Name: "boot-set", PackIDs: []string{nic.Pack.ID}})
	if err != nil {
		t.Fatal(err)
	}

	// 操作者挪了驱动目录，行里仍是旧路径。
	moved := t.TempDir()
	if err := os.Rename(filepath.Join(service.Dir, "packs"), filepath.Join(moved, "packs")); err != nil {
		t.Fatal(err)
	}
	service.Dir = moved

	if _, _, err := service.BundleArchive(ctx, bundle.Bundle.ID); err != nil {
		t.Fatalf("packing after the directory moved: %v", err)
	}
}
