package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/tianwei/diskless/internal/config"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

type SystemSettingsService struct {
	Store             store.Store
	FallbackImportDir string
}

type SystemSettingsRequest struct {
	ImportDir string `json:"import_dir"`
}

type SystemSettingsView struct {
	ImportDir        string `json:"import_dir"`
	DefaultImportDir string `json:"default_import_dir"`
}

func (s SystemSettingsService) Get(ctx context.Context) (SystemSettingsView, error) {
	fallback := s.fallbackImportDir()
	if s.Store == nil {
		return SystemSettingsView{ImportDir: fallback, DefaultImportDir: fallback}, nil
	}
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if errs.IsNotFound(err) {
		return SystemSettingsView{ImportDir: fallback, DefaultImportDir: fallback}, nil
	}
	if err != nil {
		return SystemSettingsView{}, err
	}
	if strings.TrimSpace(settings.ImportDir) == "" {
		settings.ImportDir = fallback
	}
	return SystemSettingsView{ImportDir: settings.ImportDir, DefaultImportDir: fallback}, nil
}

func (s SystemSettingsService) Save(ctx context.Context, req SystemSettingsRequest) (SystemSettingsView, error) {
	importDir, err := normalizeImportDir(req.ImportDir)
	if err != nil {
		return SystemSettingsView{}, err
	}
	if err := ensureImportDirUsable(importDir); err != nil {
		return SystemSettingsView{}, err
	}
	// 所有设置在同一行：只改目录，其余保留。
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil && !errs.IsNotFound(err) {
		return SystemSettingsView{}, err
	}
	settings.ID = domain.SystemSettingsDefaultID
	settings.ImportDir = importDir
	if err := s.Store.SystemSettings().Upsert(ctx, settings); err != nil {
		return SystemSettingsView{}, err
	}
	return SystemSettingsView{ImportDir: importDir, DefaultImportDir: s.fallbackImportDir()}, nil
}

func (s SystemSettingsService) ImportDir(ctx context.Context) (string, error) {
	settings, err := s.Get(ctx)
	if err != nil {
		return "", err
	}
	return settings.ImportDir, nil
}

func (s SystemSettingsService) fallbackImportDir() string {
	dir, err := normalizeImportDir(s.FallbackImportDir)
	if err != nil {
		return config.DefaultImportDir
	}
	return dir
}

func normalizeImportDir(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errs.Invalid("镜像目录不能为空")
	}
	dir := filepath.Clean(raw)
	if !filepath.IsAbs(dir) {
		return "", errs.Invalid("镜像目录必须是绝对路径")
	}
	if dir == string(filepath.Separator) {
		return "", errs.Invalid("镜像目录不能是根目录")
	}
	for _, blocked := range []string{"/dev", "/proc", "/run", "/sys"} {
		if dir == blocked || strings.HasPrefix(dir, blocked+"/") {
			return "", errs.Invalid("镜像目录不能位于 " + blocked)
		}
	}
	return dir, nil
}

func ensureImportDirUsable(dir string) error {
	parent := filepath.Dir(dir)
	if parent == string(filepath.Separator) {
		return errs.Invalid("镜像目录不能直接位于根目录下，请使用数据盘挂载点下的子目录")
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return errs.Invalid("镜像目录不存在，请先在数据盘或存储池挂载点中创建该目录")
		}
		return errs.Invalid("无法检查镜像目录：" + err.Error())
	}
	if !info.IsDir() {
		return errs.Invalid("镜像目录路径不是目录")
	}
	f, err := os.CreateTemp(dir, ".ndiskless-write-test-*")
	if err != nil {
		return errs.Invalid("镜像目录不可写：" + err.Error())
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return errs.Invalid("镜像目录写入测试失败：" + err.Error())
	}
	if err := os.Remove(name); err != nil {
		return errs.Invalid("镜像目录清理测试文件失败：" + err.Error())
	}
	return nil
}

// ReplicationRate 返回生效的批量传输限速：操作者设过则用设定值（0 表示明确不限速，
// 如无客户机的维护窗口），否则用部署默认值。source 为 "setting" 或 "default"。
func (s SystemSettingsService) ReplicationRate(ctx context.Context, defaultMBPS int) (mbps int, source string, err error) {
	if s.Store == nil {
		return defaultMBPS, "default", nil
	}
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if errs.IsNotFound(err) {
		return defaultMBPS, "default", nil
	}
	if err != nil {
		return 0, "", err
	}
	if settings.ReplicationRateMBPS == nil {
		return defaultMBPS, "default", nil
	}
	return *settings.ReplicationRateMBPS, "setting", nil
}

// SaveReplicationRate 保存限速（0 表示不限），共享行里的其它设置保持不变。
func (s SystemSettingsService) SaveReplicationRate(ctx context.Context, mbps int) error {
	if mbps < 0 {
		return errs.Invalid("同步限速必须 ≥ 0（0 表示不限速）")
	}
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil && !errs.IsNotFound(err) {
		return err
	}
	settings.ID = domain.SystemSettingsDefaultID
	settings.ReplicationRateMBPS = &mbps
	return s.Store.SystemSettings().Upsert(ctx, settings)
}

// SaveDataPool 记录本节点存放镜像/克隆的 ZFS 池，共享行里的其它设置保持不变。
// 界面建池后调用，使选择重启后仍有效；池名不是写死的 "tank"。
func (s SystemSettingsService) SaveDataPool(ctx context.Context, pool string) error {
	settings, err := s.Store.SystemSettings().Get(ctx, domain.SystemSettingsDefaultID)
	if err != nil && !errs.IsNotFound(err) {
		return err
	}
	settings.ID = domain.SystemSettingsDefaultID
	settings.DataPool = strings.TrimSpace(pool)
	return s.Store.SystemSettings().Upsert(ctx, settings)
}
