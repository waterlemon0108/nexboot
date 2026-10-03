package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tianwei/diskless/internal/domain"
)

type imageHealthReportRepository struct {
	repository[domain.ImageHealthReport]
}

func (r imageHealthReportRepository) GetByImage(ctx context.Context, imageID string) (domain.ImageHealthReport, error) {
	v, err := r.codec.scan(r.q.QueryRowContext(ctx,
		"SELECT "+joinColumns(r.codec.columns)+" FROM "+r.codec.table+" WHERE image_id = "+r.placeholder(1),
		imageID,
	))
	return scanOne(v, err, r.codec.table, imageID)
}

var imageHealthReportCodec = entityCodec[domain.ImageHealthReport]{
	table:   "image_health_reports",
	columns: []string{"id", "image_id", "level", "partition_style", "boot_modes", "items", "nic_pci_ids", "created_at"},
	values: func(v domain.ImageHealthReport) []any {
		items, err := json.Marshal(v.Items)
		if err != nil || v.Items == nil {
			items = []byte("[]")
		}
		return []any{v.ID, v.ImageID, string(v.Level), v.PartitionStyle, strings.Join(v.BootModes, ","), string(items), strings.Join(v.NICPCIIDs, ","), timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.ImageHealthReport, error) {
		var v domain.ImageHealthReport
		var level, modes, items, nics, created string
		if err := s.Scan(&v.ID, &v.ImageID, &level, &v.PartitionStyle, &modes, &items, &nics, &created); err != nil {
			return v, err
		}
		if nics != "" {
			v.NICPCIIDs = strings.Split(nics, ",")
		}
		if modes != "" {
			v.BootModes = strings.Split(modes, ",")
		}
		v.Level = domain.HealthLevel(level)
		if err := json.Unmarshal([]byte(items), &v.Items); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.CreatedAt = t
		return v, err
	},
}

var driverPackCodec = entityCodec[domain.DriverPack]{
	table: "driver_packs",
	columns: []string{
		"id", "name", "category", "os_type", "arch", "version", "release_date",
		"vendor", "signed", "hwids", "status", "recommended", "storage_path", "created_at",
	},
	values: func(v domain.DriverPack) []any {
		return []any{
			v.ID, v.Name, string(v.Category), string(v.OSType), v.Arch, v.Version, v.ReleaseDate,
			v.Vendor, v.Signed, stringsValue(v.HWIDs), string(v.Status), v.Recommended, v.StoragePath, timeValue(v.CreatedAt),
		}
	},
	scan: func(s rowScanner) (domain.DriverPack, error) {
		var v domain.DriverPack
		var category, osType, status, hwids, created string
		if err := s.Scan(&v.ID, &v.Name, &category, &osType, &v.Arch, &v.Version, &v.ReleaseDate,
			&v.Vendor, &v.Signed, &hwids, &status, &v.Recommended, &v.StoragePath, &created); err != nil {
			return v, err
		}
		var err error
		if v.HWIDs, err = parseStrings(hwids); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.Category = domain.DriverPackCategory(category)
		v.OSType = domain.OSType(osType)
		v.Status = domain.DriverPackStatus(status)
		v.CreatedAt = t
		return v, err
	},
}

var driverBundleCodec = entityCodec[domain.DriverBundle]{
	table:   "driver_bundles",
	columns: []string{"id", "name", "os_type", "created_at"},
	values: func(v domain.DriverBundle) []any {
		return []any{v.ID, v.Name, string(v.OSType), timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.DriverBundle, error) {
		var v domain.DriverBundle
		var osType, created string
		if err := s.Scan(&v.ID, &v.Name, &osType, &created); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.OSType, v.CreatedAt = domain.OSType(osType), t
		return v, err
	},
}

var driverBundlePackCodec = entityCodec[domain.DriverBundlePack]{
	table:   "driver_bundle_packs",
	columns: []string{"id", "bundle_id", "pack_id"},
	values: func(v domain.DriverBundlePack) []any {
		return []any{v.ID, v.BundleID, v.PackID}
	},
	scan: func(s rowScanner) (domain.DriverBundlePack, error) {
		var v domain.DriverBundlePack
		err := s.Scan(&v.ID, &v.BundleID, &v.PackID)
		return v, err
	},
}
