package adapt

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/config"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/driverinf"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrDriverPackInvalid          = errs.Invalid("驱动包必须是包含至少一个 .inf 的 zip")
	ErrDriverPackInUse            = errs.Conflict("驱动包已被驱动集引用")
	ErrDriverBundleInvalid        = errs.Invalid("驱动集名称与启动关键网卡驱动包必填")
	ErrDriverBundleExists         = errs.Conflict("驱动集已存在")
	ErrDriverPackCategory         = errs.Invalid("驱动集只能包含启动关键网卡类驱动包")
	ErrDriverPackDisabled         = errs.Invalid("驱动包已禁用，请先启用后再加入驱动集")
	ErrDriverInjectionUnavailable = errs.Conflict("驱动注入未配置，无法固化驱动")
	ErrDriverStatusInvalid        = errs.Invalid("驱动包状态无效")
	ErrDriverBundleMixedOS        = errs.Invalid("驱动集内驱动包的操作系统类型必须一致")
	ErrDriverCategoryInvalid      = errs.Invalid("驱动包类别无效")
)

// staleDriverAge 之前的驱动包在列表中标为有风险。
const staleDriverAge = 5 * 365 * 24 * time.Hour

type DriverService struct {
	Store store.Store
	Dir   string
	Now   func() time.Time
}

type UploadDriverPackRequest struct {
	Name     string
	Category domain.DriverPackCategory
	OSType   domain.OSType
	Arch     string
	Filename string
	Data     []byte
}

type DriverPackItem struct {
	Pack  domain.DriverPack `json:"pack"`
	Risks []string          `json:"risks"`
}

type DriverPackListResult struct {
	Items []DriverPackItem `json:"items"`
	Total int              `json:"total"`
}

type DriverBundleRequest struct {
	Name    string   `json:"name"`
	PackIDs []string `json:"pack_ids"`
}

type DriverBundleItem struct {
	Bundle domain.DriverBundle `json:"bundle"`
	Packs  []domain.DriverPack `json:"packs"`
}

type DriverBundleListResult struct {
	Items []DriverBundleItem `json:"items"`
	Total int                `json:"total"`
}

func (s DriverService) UploadPack(ctx context.Context, req UploadDriverPackRequest) (DriverPackItem, error) {
	if len(req.Data) == 0 {
		return DriverPackItem{}, ErrDriverPackInvalid
	}
	reader, err := zip.NewReader(bytes.NewReader(req.Data), int64(len(req.Data)))
	if err != nil {
		return DriverPackItem{}, ErrDriverPackInvalid
	}

	var parsed []driverinf.Driver
	hasCatalog := false
	for _, f := range reader.File {
		lower := strings.ToLower(f.Name)
		switch {
		case strings.HasSuffix(lower, ".cat"):
			hasCatalog = true
		case strings.HasSuffix(lower, ".inf"):
			rc, err := f.Open()
			if err != nil {
				return DriverPackItem{}, err
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return DriverPackItem{}, err
			}
			d, err := driverinf.Parse(data)
			if err != nil {
				continue // 容忍混入的非驱动 inf 文件
			}
			parsed = append(parsed, d)
		}
	}
	if len(parsed) == 0 {
		return DriverPackItem{}, ErrDriverPackInvalid
	}

	merged := mergeDrivers(parsed)
	now := s.now()
	pack := domain.DriverPack{
		ID:          fmt.Sprintf("drvpack-%d", now.UnixNano()),
		Name:        strings.TrimSpace(req.Name),
		Category:    req.Category,
		OSType:      req.OSType,
		Arch:        strings.TrimSpace(req.Arch),
		Version:     merged.Version,
		ReleaseDate: merged.ReleaseDate,
		Vendor:      merged.Provider,
		Signed:      hasCatalog,
		HWIDs:       merged.HWIDs,
		Status:      domain.DriverPackEnabled,
		CreatedAt:   now,
	}
	if pack.Name == "" {
		pack.Name = defaultPackName(req.Filename, merged)
	}
	if pack.OSType == "" {
		pack.OSType = domain.OSTypeWindows
	}
	if pack.OSType != domain.OSTypeWindows && pack.OSType != domain.OSTypeLinux {
		return DriverPackItem{}, fmt.Errorf("invalid os_type: %s", pack.OSType)
	}
	if pack.Arch == "" {
		pack.Arch = "x64"
	}
	if pack.Category == "" {
		pack.Category = inferCategory(merged.ClassName)
	}
	if !validDriverCategory(pack.Category) {
		return DriverPackItem{}, ErrDriverCategoryInvalid
	}

	dir := filepath.Join(s.driverDir(), "packs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return DriverPackItem{}, err
	}
	pack.StoragePath = filepath.Join(dir, pack.ID+".zip")
	if err := os.WriteFile(pack.StoragePath, req.Data, 0o644); err != nil {
		return DriverPackItem{}, err
	}
	if err := s.Store.DriverPacks().Create(ctx, pack); err != nil {
		_ = os.Remove(pack.StoragePath)
		return DriverPackItem{}, err
	}
	return DriverPackItem{Pack: pack, Risks: s.packRisks(pack)}, nil
}

func (s DriverService) ListPacks(ctx context.Context) (DriverPackListResult, error) {
	packs, err := s.Store.DriverPacks().List(ctx)
	if err != nil {
		return DriverPackListResult{}, err
	}
	items := make([]DriverPackItem, 0, len(packs))
	for _, pack := range packs {
		items = append(items, DriverPackItem{Pack: pack, Risks: s.packRisks(pack)})
	}
	return DriverPackListResult{Items: items, Total: len(items)}, nil
}

func (s DriverService) SetPackStatus(ctx context.Context, id string, status domain.DriverPackStatus) (domain.DriverPack, error) {
	if status != domain.DriverPackEnabled && status != domain.DriverPackDisabled {
		return domain.DriverPack{}, ErrDriverStatusInvalid
	}
	pack, err := s.Store.DriverPacks().Get(ctx, id)
	if err != nil {
		return domain.DriverPack{}, err
	}
	pack.Status = status
	if err := s.Store.DriverPacks().Update(ctx, pack); err != nil {
		return domain.DriverPack{}, err
	}
	return pack, nil
}

// SetRecommended 把一个驱动包设为推荐版本，并清掉同类别、HWID 覆盖有重叠的其它包的推荐标记。
func (s DriverService) SetRecommended(ctx context.Context, id string) (domain.DriverPack, error) {
	pack, err := s.Store.DriverPacks().Get(ctx, id)
	if err != nil {
		return domain.DriverPack{}, err
	}
	packs, err := s.Store.DriverPacks().List(ctx)
	if err != nil {
		return domain.DriverPack{}, err
	}
	hwids := map[string]bool{}
	for _, hw := range pack.HWIDs {
		hwids[hw] = true
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		for _, other := range packs {
			if other.ID == pack.ID || other.Category != pack.Category || !other.Recommended {
				continue
			}
			if !hwidsOverlap(hwids, other.HWIDs) {
				continue
			}
			other.Recommended = false
			if err := tx.DriverPacks().Update(ctx, other); err != nil {
				return err
			}
		}
		pack.Recommended = true
		return tx.DriverPacks().Update(ctx, pack)
	}); err != nil {
		return domain.DriverPack{}, err
	}
	return pack, nil
}

func (s DriverService) DeletePack(ctx context.Context, id string) error {
	pack, err := s.Store.DriverPacks().Get(ctx, id)
	if err != nil {
		return err
	}
	links, err := s.Store.DriverBundlePacks().List(ctx)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.PackID == id {
			return ErrDriverPackInUse
		}
	}
	if err := s.Store.DriverPacks().Delete(ctx, id); err != nil {
		return err
	}
	if pack.StoragePath != "" {
		_ = os.Remove(s.packPath(pack))
	}
	return nil
}

func (s DriverService) CreateBundle(ctx context.Context, req DriverBundleRequest) (DriverBundleItem, error) {
	name := strings.TrimSpace(req.Name)
	packIDs := uniqueStrings(req.PackIDs)
	if name == "" || len(packIDs) == 0 {
		return DriverBundleItem{}, ErrDriverBundleInvalid
	}
	bundles, err := s.Store.DriverBundles().List(ctx)
	if err != nil {
		return DriverBundleItem{}, err
	}
	for _, b := range bundles {
		if b.Name == name {
			return DriverBundleItem{}, ErrDriverBundleExists
		}
	}
	packs := make([]domain.DriverPack, 0, len(packIDs))
	osType := domain.OSType("")
	for _, id := range packIDs {
		pack, err := s.Store.DriverPacks().Get(ctx, id)
		if err != nil {
			return DriverBundleItem{}, err
		}
		if pack.Category != domain.DriverCategoryBootCriticalNIC {
			return DriverBundleItem{}, ErrDriverPackCategory
		}
		// 禁用由服务端把关：即使调用方直接指定，已禁用的包也不能进入超管机。
		if pack.Status == domain.DriverPackDisabled {
			return DriverBundleItem{}, ErrDriverPackDisabled
		}
		if osType == "" {
			osType = pack.OSType
		} else if osType != pack.OSType {
			return DriverBundleItem{}, ErrDriverBundleMixedOS
		}
		packs = append(packs, pack)
	}
	now := s.now()
	bundle := domain.DriverBundle{
		ID:        fmt.Sprintf("drvbundle-%d", now.UnixNano()),
		Name:      name,
		OSType:    osType,
		CreatedAt: now,
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.DriverBundles().Create(ctx, bundle); err != nil {
			return err
		}
		for i, pack := range packs {
			link := domain.DriverBundlePack{
				ID:       fmt.Sprintf("%s-%d", bundle.ID, i),
				BundleID: bundle.ID,
				PackID:   pack.ID,
			}
			if err := tx.DriverBundlePacks().Create(ctx, link); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return DriverBundleItem{}, err
	}
	return DriverBundleItem{Bundle: bundle, Packs: packs}, nil
}

func (s DriverService) ListBundles(ctx context.Context) (DriverBundleListResult, error) {
	bundles, err := s.Store.DriverBundles().List(ctx)
	if err != nil {
		return DriverBundleListResult{}, err
	}
	links, err := s.Store.DriverBundlePacks().List(ctx)
	if err != nil {
		return DriverBundleListResult{}, err
	}
	packs, err := s.Store.DriverPacks().List(ctx)
	if err != nil {
		return DriverBundleListResult{}, err
	}
	packByID := make(map[string]domain.DriverPack, len(packs))
	for _, pack := range packs {
		packByID[pack.ID] = pack
	}
	items := make([]DriverBundleItem, 0, len(bundles))
	for _, bundle := range bundles {
		item := DriverBundleItem{Bundle: bundle, Packs: []domain.DriverPack{}}
		for _, link := range links {
			if link.BundleID != bundle.ID {
				continue
			}
			if pack, ok := packByID[link.PackID]; ok {
				item.Packs = append(item.Packs, pack)
			}
		}
		items = append(items, item)
	}
	return DriverBundleListResult{Items: items, Total: len(items)}, nil
}

func (s DriverService) DeleteBundle(ctx context.Context, id string) error {
	if _, err := s.Store.DriverBundles().Get(ctx, id); err != nil {
		return err
	}
	links, err := s.Store.DriverBundlePacks().List(ctx)
	if err != nil {
		return err
	}
	return s.Store.Tx(ctx, func(tx store.Store) error {
		for _, link := range links {
			if link.BundleID != id {
				continue
			}
			if err := tx.DriverBundlePacks().Delete(ctx, link.ID); err != nil {
				return err
			}
		}
		return tx.DriverBundles().Delete(ctx, id)
	})
}

// BundleArchive 把驱动包组打成一个 zip，每个成员包一个目录，供超管适配工具使用。
func (s DriverService) BundleArchive(ctx context.Context, id string) (string, []byte, error) {
	bundle, err := s.Store.DriverBundles().Get(ctx, id)
	if err != nil {
		return "", nil, err
	}
	links, err := s.Store.DriverBundlePacks().List(ctx)
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	out := zip.NewWriter(&buf)
	for _, link := range links {
		if link.BundleID != id {
			continue
		}
		pack, err := s.Store.DriverPacks().Get(ctx, link.PackID)
		if err != nil {
			return "", nil, err
		}
		if err := s.appendPackToArchive(out, pack); err != nil {
			return "", nil, err
		}
	}
	if err := out.Close(); err != nil {
		return "", nil, err
	}
	return bundle.Name + ".zip", buf.Bytes(), nil
}

func (s DriverService) appendPackToArchive(out *zip.Writer, pack domain.DriverPack) error {
	src, err := zip.OpenReader(s.packPath(pack))
	if err != nil {
		return fmt.Errorf("open pack %s: %w", pack.ID, err)
	}
	defer src.Close()
	folder := sanitizeArchiveName(pack.Name)
	if folder == "" {
		folder = pack.ID
	}
	for _, f := range src.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name, ok := safeArchivePath(f.Name)
		if !ok {
			continue
		}
		w, err := out.Create(folder + "/" + name)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, rc); err != nil {
			_ = rc.Close()
			return err
		}
		_ = rc.Close()
	}
	return nil
}

func safeArchivePath(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == "/" || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func sanitizeArchiveName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s DriverService) packRisks(pack domain.DriverPack) []string {
	risks := []string{}
	if !pack.Signed {
		risks = append(risks, "unsigned")
	}
	if len(pack.HWIDs) == 0 {
		risks = append(risks, "no_hwids")
	}
	if t, ok := parseDriverDate(pack.ReleaseDate); ok && s.now().Sub(t) > staleDriverAge {
		risks = append(risks, "outdated")
	}
	return risks
}

func parseDriverDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"01/02/2006", "1/2/2006", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func mergeDrivers(drivers []driverinf.Driver) driverinf.Driver {
	var merged driverinf.Driver
	seen := map[string]bool{}
	for _, d := range drivers {
		if merged.Provider == "" {
			merged.Provider = d.Provider
		}
		if merged.ClassName == "" {
			merged.ClassName = d.ClassName
		}
		if merged.Version == "" {
			merged.Version = d.Version
		}
		if merged.ReleaseDate == "" {
			merged.ReleaseDate = d.ReleaseDate
		}
		if merged.CatalogFile == "" {
			merged.CatalogFile = d.CatalogFile
		}
		for _, hw := range d.HWIDs {
			if !seen[hw] {
				seen[hw] = true
				merged.HWIDs = append(merged.HWIDs, hw)
			}
		}
	}
	return merged
}

func defaultPackName(filename string, merged driverinf.Driver) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSpace(filename)), filepath.Ext(filename))
	if base != "" && base != "." {
		return base
	}
	if merged.Provider != "" {
		return strings.TrimSpace(merged.Provider + " " + merged.ClassName)
	}
	return "driver-pack"
}

func inferCategory(class string) domain.DriverPackCategory {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "net":
		return domain.DriverCategoryNIC
	case "display":
		return domain.DriverCategoryGPU
	case "media", "audioprocessingobject":
		return domain.DriverCategoryAudio
	case "system", "hdc", "scsiadapter", "usb":
		return domain.DriverCategoryChipset
	default:
		return domain.DriverCategoryOther
	}
}

func validDriverCategory(c domain.DriverPackCategory) bool {
	switch c {
	case domain.DriverCategoryBootCriticalNIC, domain.DriverCategoryNIC, domain.DriverCategoryGPU,
		domain.DriverCategoryAudio, domain.DriverCategoryChipset, domain.DriverCategoryOther:
		return true
	}
	return false
}

func hwidsOverlap(set map[string]bool, others []string) bool {
	for _, hw := range others {
		if set[hw] {
			return true
		}
	}
	return false
}

// packPath 由包 id 和当前驱动目录推导文件位置，不信任行里存的路径：存绝对路径时
// 目录一挪或数据库恢复到布局不同的主机就失效。旧行记录的路径在文件确实存在时仍然采用。
func (s DriverService) packPath(pack domain.DriverPack) string {
	derived := filepath.Join(s.driverDir(), "packs", pack.ID+".zip")
	if pack.StoragePath != "" && pack.StoragePath != derived {
		if _, err := os.Stat(pack.StoragePath); err == nil {
			return pack.StoragePath
		}
	}
	return derived
}

func (s DriverService) driverDir() string {
	if strings.TrimSpace(s.Dir) == "" {
		return config.DefaultDriverDir
	}
	return s.Dir
}

func (s DriverService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// uniqueStrings 去空白、去空、去重；assets 包里有同样的私有实现，复制一份好过跨域 import。
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
