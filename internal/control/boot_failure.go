package control

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

// RecordBootFailure 记录客户机 iPXE 脚本放弃时上报的失败。
func (s BootService) RecordBootFailure(ctx context.Context, mac, stage, code, platform string) error {
	if stage != domain.BootStageSanhook && stage != domain.BootStageSanboot {
		return errs.Invalid(fmt.Sprintf("未知的开机阶段 %q", stage))
	}
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return err
	}
	imageID, modes, err := s.bootModes(ctx, t)
	if err != nil {
		return err
	}
	// 盘挂上了却引导不了，引导方式不符就是最具体的原因。
	if stage == domain.BootStageSanboot && bootModeMismatch(platform, modes) {
		stage = domain.BootStageBootMode
	}
	return s.Store.BootFailures().Upsert(ctx, domain.BootFailure{
		MAC: storage.NormalizeMAC(t.MAC), Stage: stage, Code: ipxeErrno(code),
		Platform: platform, ImageID: imageID, At: s.now(),
	})
}

// CheckBootMode 在客户机尝试前检查：固件启动不了即将下发的镜像时先记一次失败。
func (s BootService) CheckBootMode(ctx context.Context, mac, platform string) error {
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return err
	}
	imageID, modes, err := s.bootModes(ctx, t)
	if err != nil || !bootModeMismatch(platform, modes) {
		return err
	}
	return s.Store.BootFailures().Upsert(ctx, domain.BootFailure{
		MAC: storage.NormalizeMAC(t.MAC), Stage: domain.BootStageBootMode,
		Platform: platform, ImageID: imageID, At: s.now(),
	})
}

func (s BootService) bootModes(ctx context.Context, t domain.Terminal) (string, []string, error) {
	g, err := s.Store.Groups().Get(ctx, t.GroupID)
	if err != nil {
		return "", nil, err
	}
	report, err := s.Store.ImageHealthReports().GetByImage(ctx, g.SystemImageID)
	if errs.IsNotFound(err) {
		return g.SystemImageID, nil, nil
	}
	return g.SystemImageID, report.BootModes, err
}

func bootModeMismatch(platform string, modes []string) bool {
	if len(modes) == 0 {
		return false
	}
	switch platform {
	case "efi":
		return !slices.Contains(modes, domain.BootModeUEFI)
	case "pcbios":
		return !slices.Contains(modes, domain.BootModeBIOS)
	}
	return false
}

// ipxeErrno 按 ipxe.org/err 的写法显示 iPXE 错误码。
func ipxeErrno(code string) string {
	n, err := strconv.ParseUint(code, 10, 32)
	if err != nil || n == 0 {
		return code
	}
	return fmt.Sprintf("0x%08x", n)
}

func (s BootService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
