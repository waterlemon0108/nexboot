package control

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/tianwei/diskless/internal/boot"
	"github.com/tianwei/diskless/internal/control/adapt"
	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

// ErrUnknownTerminal 表示 MAC 未登记为客户机。
var ErrUnknownTerminal = errs.NotFound("未知终端")

// bootFlight 合并同一 MAC 的并发 /boot：iPXE 会在慢请求返回前重试，
// 两次拆除/克隆/导出并发跑会互相破坏，所有调用方共享一次执行与结果。
var bootFlight singleflight.Group

const bootBuildTimeout = 2 * time.Minute

// BootService 处理客户机开机：准备克隆与 LUN，生成 iPXE 脚本。
type BootService struct {
	Store   store.Store
	Storage storage.StorageAgent
	Builder boot.BootBuilder
	Logger  *slog.Logger
	// APIPort 写入 Windows 开机脚本，供其回调 API（默认 8080）。
	APIPort string
	// PortalAddr 非空时替代存储 agent 自身地址写入 sanhook（HA 下为 VIP）。
	// 客户机连哪里由控制面决定，agent 只知道自己是谁。
	PortalAddr string
	// Place 非空时按放置规则选节点并把落点记到客户机上；为 nil 时走单机路径。
	Place *place.Router
	Now   func() time.Time
}

// BuildBootScript 为 mac 准备开机所需的 LUN 并返回 iPXE 脚本。
func (s BootService) BuildBootScript(ctx context.Context, mac string) (string, error) {
	if mac == "" {
		return "", fmt.Errorf("mac is required")
	}
	script, err, _ := bootFlight.Do(storage.NormalizeMAC(mac), func() (any, error) {
		// 与调用方解绑：被放弃的请求不能取消重试正在等待的执行，
		// 也不能把 zfs/targetcli 杀在半途留下残局。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bootBuildTimeout)
		defer cancel()
		return s.buildBootScript(ctx, storage.NormalizeMAC(mac))
	})
	if err != nil {
		return "", err
	}
	return script.(string), nil
}

// terminalByMAC 是开机相关入口共用的 MAC 解析，未知 MAC 映射为 ErrUnknownTerminal。
func (s BootService) terminalByMAC(ctx context.Context, mac string) (domain.Terminal, error) {
	if mac == "" {
		return domain.Terminal{}, fmt.Errorf("mac is required")
	}
	t, err := s.Store.Terminals().GetByMAC(ctx, storage.NormalizeMAC(mac))
	if errs.IsNotFound(err) {
		return domain.Terminal{}, ErrUnknownTerminal
	}
	return t, err
}

func (s BootService) buildBootScript(ctx context.Context, mac string) (string, error) {
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return "", err
	}
	g, err := s.Store.Groups().Get(ctx, t.GroupID)
	if err != nil {
		return "", err
	}
	img, err := s.Store.Images().Get(ctx, g.SystemImageID)
	if err != nil {
		return "", err
	}
	// 普通机从配置当前应用的还原点开机，分组上缓存的 id 只用于引用保护，
	// 这样刚做的发布或回退本次开机立即生效。
	systemReductionID := g.SystemReductionID
	if cfg, err := s.Store.Configs().Get(ctx, g.SystemConfigID); err != nil {
		return "", err
	} else if cfg.DefaultReductionID != nil && *cfg.DefaultReductionID != "" {
		systemReductionID = *cfg.DefaultReductionID
	}
	if t.IsSuper {
		if err := assets.EnsureSuperAvailable(ctx, s.Store, t); err != nil {
			return "", err
		}
		systemReductionID, err = s.latestReductionID(ctx, g.SystemConfigID)
		if err != nil {
			return "", err
		}
	}
	systemSnapshot, err := s.reductionSnapshotName(ctx, systemReductionID)
	if err != nil {
		return "", err
	}
	dataDisks, err := s.dataDiskSources(ctx, g.ID, t.IsSuper)
	if err != nil {
		return "", err
	}
	// 上次会话的拆除在 CreateClientLUN 内与导出一并完成。
	// Windows 克隆都需要开机脚本：恢复 C-1 清掉的网关与 DNS，并按配置重设数据盘盘符。
	// 脚本对所有客户机相同，导入时已烘焙当前版本的镜像经 CoW 链继承，这里留空即可
	// 跳过挂载克隆；版本为空或过期说明镜像早于烘焙，退回逐台注入。
	var mountScript []byte
	if img.OSType == domain.OSTypeWindows && img.MountScriptVersion != adapt.MountScriptVersion(s.APIPort) {
		mountScript = adapt.RenderMountScript(s.APIPort)
	}
	agent, portal := storage.ClientHostAgent(s.Storage), s.PortalAddr
	if s.Place != nil {
		// 规则见 place.Router.ForClient；不绑分组时也会自动分散。
		placed := s.Place.ForClient(ctx, g, t)
		agent, portal = placed.Agent, placed.Portal
		if !placed.Healthy && s.Logger != nil {
			s.Logger.Warn("分组绑定的存储节点不可达，本次开机回退到本机承载", "group", g.ID, "terminal", t.ID)
		}
		// 建盘前先记：建到一半失败时，清理也要能找到这台节点上的残留。
		t = s.recordPlacement(ctx, t, placed.ServerID)
	}
	req := storage.ClientReq{
		MAC:     t.MAC,
		IP:      t.IP,
		GroupID: t.GroupID,
		Super:   t.IsSuper,
		System: storage.ClientSource{
			ImageID:      g.SystemImageID,
			ConfigID:     g.SystemConfigID,
			SnapshotName: systemSnapshot,
		},
		DataDisks:   dataDisks,
		MountScript: mountScript,
	}
	lun, err := agent.CreateClientLUN(ctx, req)
	if err != nil && isMissingSnapshot(err) && s.Storage != nil && agent != storage.ClientHostAgent(s.Storage) {
		// 落点节点还没复制到这个还原点（刚导入的镜像最多等一分钟），
		// 本机是写入者、目录必然最新，改由本机服务这次开机。
		if s.Logger != nil {
			s.Logger.Warn("placed node does not have the restore point yet; serving this boot locally",
				"terminal", t.ID, "snapshot", systemSnapshot, "error", err)
		}
		agent, portal = storage.ClientHostAgent(s.Storage), s.PortalAddr
		lun, err = agent.CreateClientLUN(ctx, req)
		if s.Place != nil {
			t = s.recordPlacement(ctx, t, s.Place.NodeID)
		}
	}
	if err != nil {
		return "", explainMissingSnapshot(err, systemSnapshot)
	}
	if portal != "" {
		lun.Server = portal
	}
	return s.Builder.Build(t, g, img, lun)
}

// recordPlacement 把落点记到客户机上，删除和移动按它找克隆所在节点；尽力而为，失败不阻塞开机。
func (s BootService) recordPlacement(ctx context.Context, t domain.Terminal, serverID string) domain.Terminal {
	if t.StorageServerID != nil && *t.StorageServerID == serverID {
		return t
	}
	rec := t
	rec.StorageServerID = &serverID
	if err := s.Store.Terminals().Update(ctx, rec); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("terminal placement record failed", "terminal", t.ID, "error", err)
		}
		return t
	}
	return rec
}

// groupDisks 按稳定顺序返回分组数据盘，该顺序同时决定 LUN 号与盘符。
func (s BootService) groupDisks(ctx context.Context, groupID string) ([]domain.GroupDisk, error) {
	return assets.GroupDataDisks(ctx, s.Store, groupID)
}

// DataDiskLetter 是提供给 mount-disks.ps1 的一对 LUN→盘符，脚本每次开机重设盘符。
type DataDiskLetter struct {
	LUN    int    `json:"lun"`
	Letter string `json:"letter"`
}

// DataDiskLetters 返回客户机数据盘的盘符（规整为大写字母，"D:" -> "D"）。
// LUN 与开机导出同出 dataDiskLUN 和同一列表，盘符不会对错盘。
func (s BootService) DataDiskLetters(ctx context.Context, mac string) ([]DataDiskLetter, error) {
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return nil, err
	}
	disks, err := s.groupDisks(ctx, t.GroupID)
	if err != nil {
		return nil, err
	}
	linux, err := s.groupRunsLinux(ctx, t.GroupID)
	if err != nil {
		return nil, err
	}
	letters := make([]DataDiskLetter, 0, len(disks))
	for i, disk := range disks {
		if linux {
			letters = append(letters, DataDiskLetter{LUN: dataDiskLUN(i), Letter: strings.TrimSpace(disk.MountTarget)})
			continue
		}
		letter := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(disk.MountTarget), ":"))
		letters = append(letters, DataDiskLetter{LUN: dataDiskLUN(i), Letter: letter})
	}
	return letters, nil
}

// groupRunsLinux 判断分组是否为 Linux：Linux 的挂载目标是路径而不是盘符。
func (s BootService) groupRunsLinux(ctx context.Context, groupID string) (bool, error) {
	g, err := s.Store.Groups().Get(ctx, groupID)
	if err != nil {
		return false, err
	}
	img, err := s.Store.Images().Get(ctx, g.SystemImageID)
	if err != nil {
		return false, err
	}
	return img.OSType == domain.OSTypeLinux, nil
}

// dataDiskLUN 返回第 i 块数据盘的 LUN 号，编号规则在 assets。
func dataDiskLUN(index int) int { return assets.DataDiskLUN(index) }

// NetConfig 是提供给开机脚本的分组网络设置。C-1 在 sanhook 前清掉网关，
// iBFT 不带网关与 DNS，由脚本在开机后恢复。
type NetConfig struct {
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

// RegisteredIP 返回本系统写进 dnsmasq 分给该 MAC 的地址，而非调用方自称的地址。
// /boot 系列据此拒绝替别的机器应答；未知客户机或尚无地址时返回空，视为放行。
func (s BootService) RegisteredIP(ctx context.Context, mac string) (string, error) {
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return "", nil // 未知 MAC 由各自的处理器答 404，这里不越俎代庖
	}
	return strings.TrimSpace(t.IP), nil
}

// NetConfig 返回客户机所在分组的网关与 DNS。
func (s BootService) NetConfig(ctx context.Context, mac string) (NetConfig, error) {
	t, err := s.terminalByMAC(ctx, mac)
	if err != nil {
		return NetConfig{}, err
	}
	g, err := s.Store.Groups().Get(ctx, t.GroupID)
	if err != nil {
		return NetConfig{}, err
	}
	cfg := NetConfig{Gateway: strings.TrimSpace(g.Gateway), DNS: []string{}}
	for _, dns := range []string{g.DNS1, g.DNS2} {
		if dns = strings.TrimSpace(dns); dns != "" {
			cfg.DNS = append(cfg.DNS, dns)
		}
	}
	return cfg, nil
}

func (s BootService) dataDiskSources(ctx context.Context, groupID string, latest bool) ([]storage.ClientSource, error) {
	disks, err := s.groupDisks(ctx, groupID)
	if err != nil {
		return nil, err
	}
	sources := make([]storage.ClientSource, 0, len(disks))
	for i, disk := range disks {
		cfg, err := s.Store.Configs().Get(ctx, disk.ConfigID)
		if err != nil {
			return nil, err
		}
		if cfg.ImageID != disk.ImageID || cfg.DefaultReductionID == nil || *cfg.DefaultReductionID == "" {
			return nil, assets.ErrGroupDiskMismatch
		}
		reductionID := *cfg.DefaultReductionID
		if latest {
			reductionID, err = s.latestReductionID(ctx, disk.ConfigID)
			if err != nil {
				return nil, err
			}
		}
		snapshotName, err := s.reductionSnapshotName(ctx, reductionID)
		if err != nil {
			return nil, err
		}
		sources = append(sources, storage.ClientSource{
			ImageID:      disk.ImageID,
			ConfigID:     disk.ConfigID,
			SnapshotName: snapshotName,
			MountTarget:  disk.MountTarget,
			LUN:          dataDiskLUN(i),
		})
	}
	return sources, nil
}

func (s BootService) reductionSnapshotName(ctx context.Context, reductionID string) (string, error) {
	reduction, err := s.Store.Reductions().Get(ctx, reductionID)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(strings.TrimSpace(reduction.Name), "@")
	if name == "" {
		return "", fmt.Errorf("reduction snapshot is required")
	}
	return name, nil
}

func (s BootService) latestReductionID(ctx context.Context, configID string) (string, error) {
	reductions, err := s.Store.Reductions().List(ctx)
	if err != nil {
		return "", err
	}
	var latest string
	var latestAt time.Time
	for _, reduction := range reductions {
		if reduction.ConfigID != configID || reduction.Status != domain.ReductionStatusReady {
			continue
		}
		if latest == "" || reduction.CreatedAt.After(latestAt) {
			latest = reduction.ID
			latestAt = reduction.CreatedAt
		}
	}
	if latest == "" {
		return "", fmt.Errorf("ready reduction: %w", errs.ErrNotFound)
	}
	return latest, nil
}

// isMissingSnapshot 判断开机所需快照是否不在被请求的池上。
// 还原点记录与池上快照可能脱节，此时整个分组都开不了机。
func isMissingSnapshot(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") && strings.Contains(msg, "cannot open")
}

// explainMissingSnapshot 把「cannot open ...: dataset does not exist」改写成
// 点名还原点、告诉操作者怎么处理的提示。
func explainMissingSnapshot(err error, snapshot string) error {
	if err == nil {
		return nil
	}
	if !isMissingSnapshot(err) {
		return err
	}
	name := strings.TrimPrefix(strings.TrimSpace(snapshot), "@")
	if name == "" {
		name = "（未命名）"
	}
	return fmt.Errorf("还原点 %s 的快照在存储池上已丢失，客户机无法从它开机。"+
		"请在该配置下改用其它还原点（把它设为当前应用点），再删除这个已损坏的还原点。原因：%w", name, err)
}
