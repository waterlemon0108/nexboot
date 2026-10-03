package assets

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrGroupDiskExists              = errs.Conflict("该挂载目标已存在")
	ErrGroupDiskMountTargetRequired = errs.Invalid("挂载目标必填")
	ErrGroupDiskBadLetter           = errs.Invalid("Windows 分组的数据盘盘符必须是 D: 到 Z: 的单个盘符")
	ErrGroupDiskBadPath             = errs.Invalid("Linux 分组的数据盘挂载点要写成 /data 这样的绝对路径，不能是 / 或 /etc、/usr 等系统目录，也不能带 .. 或结尾的 /")
	ErrGroupDiskMismatch            = errs.Invalid("数据盘的镜像与配置不匹配")
)

type GroupDiskService struct {
	Store store.Store
	Now   func() time.Time
}

type GroupDiskListResult struct {
	Items []domain.GroupDisk `json:"items"`
	Total int                `json:"total"`
}

type GroupDiskRequest struct {
	MountTarget string `json:"mount_target"`
	ImageID     string `json:"image_id"`
	ConfigID    string `json:"config_id"`
}

// GroupDataDisks 按决定 LUN 与盘符的顺序（存储顺序，每次读取一致）列出分组数据盘。
// 启动路径把第 i 块导出为 DataDiskLUN(i)，凡需按 LUN 点名数据盘的地方（如超管机关机）都读同一列表。
func GroupDataDisks(ctx context.Context, st store.Store, groupID string) ([]domain.GroupDisk, error) {
	disks, err := st.GroupDisks().List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]domain.GroupDisk, 0, len(disks))
	for _, disk := range disks {
		if disk.GroupID == groupID {
			items = append(items, disk)
		}
	}
	return items, nil
}

// DataDiskLUN 返回分组第 i 块数据盘导出的 LUN。LUN 0 是系统盘，数据盘从 1 起；只在这里决定。
func DataDiskLUN(index int) int { return index + 1 }

func (s GroupDiskService) List(ctx context.Context, groupID string) (GroupDiskListResult, error) {
	if _, err := s.Store.Groups().Get(ctx, groupID); err != nil {
		return GroupDiskListResult{}, err
	}
	items, err := GroupDataDisks(ctx, s.Store, groupID)
	if err != nil {
		return GroupDiskListResult{}, err
	}
	if items == nil {
		items = []domain.GroupDisk{}
	}
	return GroupDiskListResult{Items: items, Total: len(items)}, nil
}

func (s GroupDiskService) Create(ctx context.Context, groupID string, req GroupDiskRequest) (domain.GroupDisk, error) {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()
	if err := refuseSuperGroupChange(ctx, s.Store, strings.TrimSpace(groupID), "添加数据盘"); err != nil {
		return domain.GroupDisk{}, err
	}

	disk, err := s.prepareGroupDisk(ctx, groupID, req)
	if err != nil {
		return domain.GroupDisk{}, err
	}
	if err := s.ensureMountTargetUnique(ctx, disk.GroupID, disk.MountTarget, ""); err != nil {
		return domain.GroupDisk{}, err
	}
	if err := s.Store.GroupDisks().Create(ctx, disk); err != nil {
		return domain.GroupDisk{}, err
	}
	return disk, nil
}

func (s GroupDiskService) Delete(ctx context.Context, id string) error {
	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()
	disk, err := s.Store.GroupDisks().Get(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseSuperGroupChange(ctx, s.Store, disk.GroupID, "删除数据盘"); err != nil {
		return err
	}
	return s.Store.GroupDisks().Delete(ctx, id)
}

func (s GroupDiskService) prepareGroupDisk(ctx context.Context, groupID string, req GroupDiskRequest) (domain.GroupDisk, error) {
	groupID = strings.TrimSpace(groupID)
	group, err := s.Store.Groups().Get(ctx, groupID)
	if err != nil {
		return domain.GroupDisk{}, err
	}
	mountTarget := strings.TrimSpace(req.MountTarget)
	if mountTarget == "" {
		return domain.GroupDisk{}, ErrGroupDiskMountTargetRequired
	}
	// Windows 分组用真实盘符：注入客户机的开机脚本在启动时应用它，所以必须是规范化后的 D:–Z:。
	if group.SystemImageID != "" {
		sysImg, err := s.Store.Images().Get(ctx, group.SystemImageID)
		if err != nil {
			return domain.GroupDisk{}, err
		}
		if sysImg.OSType == domain.OSTypeLinux && !linuxMountPathOK(mountTarget) {
			return domain.GroupDisk{}, ErrGroupDiskBadPath
		}
		if sysImg.OSType == domain.OSTypeWindows {
			letter := strings.ToUpper(strings.TrimSuffix(mountTarget, ":"))
			if len(letter) != 1 || letter[0] < 'D' || letter[0] > 'Z' {
				return domain.GroupDisk{}, ErrGroupDiskBadLetter
			}
			mountTarget = letter + ":"
		}
	}
	imageID := strings.TrimSpace(req.ImageID)
	configID := strings.TrimSpace(req.ConfigID)
	if imageID == "" || configID == "" {
		return domain.GroupDisk{}, ErrGroupDiskMismatch
	}
	img, err := s.Store.Images().Get(ctx, imageID)
	if err != nil {
		return domain.GroupDisk{}, err
	}
	// 旧记录可能仍指向「用途」出现前的系统镜像；新数据盘只能来自数据盘镜像。
	if img.Purpose != domain.ImagePurposeData {
		return domain.GroupDisk{}, errs.Invalid(fmt.Sprintf("%s 是系统盘镜像，不能作数据盘；请选数据盘镜像，或在镜像详情里把它的用途改为数据盘", img.Name))
	}
	cfg, err := s.Store.Configs().Get(ctx, configID)
	if err != nil {
		return domain.GroupDisk{}, err
	}
	if cfg.ImageID != imageID {
		return domain.GroupDisk{}, ErrGroupDiskMismatch
	}
	return domain.GroupDisk{
		ID:          fmt.Sprintf("group-disk-%d", s.now().UnixNano()),
		GroupID:     groupID,
		MountTarget: mountTarget,
		ImageID:     imageID,
		ConfigID:    configID,
	}, nil
}

func (s GroupDiskService) ensureMountTargetUnique(ctx context.Context, groupID, mountTarget, currentID string) error {
	disks, err := s.Store.GroupDisks().List(ctx)
	if err != nil {
		return err
	}
	for _, disk := range disks {
		if disk.ID != currentID && disk.GroupID == groupID && disk.MountTarget == mountTarget {
			return ErrGroupDiskExists
		}
	}
	return nil
}

func (s GroupDiskService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// 挂到这些目录上会遮住系统自己的文件。
var linuxSystemDirs = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/proc", "/root", "/run", "/sbin", "/snap", "/sys", "/tmp", "/usr", "/var"}

func linuxMountPathOK(p string) bool {
	if !strings.HasPrefix(p, "/") || p == "/" || path.Clean(p) != p || strings.ContainsAny(p, "\t\n\r") {
		return false
	}
	for _, dir := range linuxSystemDirs {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return false
		}
	}
	return true
}
