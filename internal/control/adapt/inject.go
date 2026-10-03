package adapt

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/place"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrAdaptationNotSuper    = errs.Invalid("该终端不是超管机")
	ErrAdaptationEmptyBundle = errs.Invalid("驱动集内没有驱动包")
	ErrAdaptationNoPending   = errs.Invalid("该终端没有待检测的驱动注入")
	ErrAdaptationOnline      = errs.Invalid("请先关闭超管机后再注入驱动")
	ErrAdaptationOnlineCheck = errs.Invalid("无法确认超管机已关机，请检查存储会话后重试")
	ErrAdaptationOverwrite   = errs.Invalid("该终端已有未固化的驱动注入，请确认覆盖后再注入")
)

// AdaptationService 实现离线驱动注入：把驱动包和开机安装脚本放进超管机克隆（InjectDriver），
// 适配后的盘由操作者经 StopSuper 固化为还原点。
type AdaptationService struct {
	Store     store.Store
	Now       func() time.Time
	Storage   storage.StorageAgent
	BundleZip func(ctx context.Context, bundleID string) ([]byte, error)
	Logger    *slog.Logger
	// APIPort 写入 mount-disks.ps1，供客户机调用 /boot/data-disks（默认 8080）。
	APIPort string
	// Place 非空时把注入路由到持有超管机持久克隆的节点；为 nil 时在本机。
	Place *place.Router
}

// hostFor 返回持有该客户机克隆的 agent。
func (s AdaptationService) hostFor(ctx context.Context, t domain.Terminal) storage.ClientHostAgent {
	if s.Place == nil {
		return s.Storage
	}
	return s.Place.ForTerminal(ctx, t).Agent
}

func (s AdaptationService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

//go:embed auto-adapt.ps1
var installDriverScript []byte

//go:embed mount-disks.ps1
var mountDisksScript []byte

//go:embed nd-datadisks.sh
var linuxMountScript []byte

const mountScriptPortPlaceholder = "__ND_API_PORT__"

// RenderMountScript 把 API 端口写进盘符开机脚本（服务器 IP 由脚本从 iSCSI 会话取得，
// 只需写端口）。开机流程和驱动注入共用。
func RenderMountScript(apiPort string) []byte {
	return bytes.ReplaceAll(mountDisksScript, []byte(mountScriptPortPlaceholder), []byte(normalPort(apiPort)))
}

// RenderLinuxMountScript 生成 Linux 镜像的数据盘挂载脚本；服务器地址从 iBFT 读取，只需写端口。
func RenderLinuxMountScript(apiPort string) []byte {
	return bytes.ReplaceAll(linuxMountScript, []byte(mountScriptPortPlaceholder), []byte(normalPort(apiPort)))
}

// LinuxMountScriptVersion 是 Linux 烘焙版的 MountScriptVersion。
func LinuxMountScriptVersion(apiPort string) string {
	sum := sha256.Sum256(append(RenderLinuxMountScript(apiPort), storage.LinuxLayoutID()...))
	return hex.EncodeToString(sum[:])[:16]
}

func normalPort(apiPort string) string {
	if port := strings.TrimSpace(apiPort); port != "" {
		return port
	}
	return "8080"
}

func (s AdaptationService) mountScript() []byte {
	return RenderMountScript(s.APIPort)
}

// MountScriptVersion 标识一次烘焙注入。镜像记录导入时的版本，开机路径比对字符串即可
// 判断克隆是否已带脚本，不必挂载每台客户机的 NTFS 分区。版本为空或过期即视为未烘焙，
// 退回逐台注入，脚本或 API 端口变化不会让客户机沿用旧脚本。
func MountScriptVersion(apiPort string) string {
	// 布局（ndmount.cmd、scripts.ini/gpt.ini 注册及位置）也算进版本，
	// 布局变化会自动让已烘焙镜像失效。
	sum := sha256.Sum256(append(RenderMountScript(apiPort), storage.MountScriptLayoutID()...))
	return hex.EncodeToString(sum[:])[:16]
}

// InjectResult 是一次离线驱动注入的结果。
type InjectResult struct {
	BundleName string   `json:"bundle_name"`
	Steps      []string `json:"steps"`
}

// InjectDriver 把开机驱动包和一次性安装脚本离线放进超管机持久克隆（复用 PrepareSuperAdaptation），
// 每次注入都附带盘符脚本。下次开机时驱动以 SYSTEM 安装（不自动关机），操作者确认后关机，
// 再经 StopSuper 存为还原点。
// bundleID 可为空：此时只注入盘符脚本（适配脚本仍跑一次并写 done.txt），
// 已适配的分组无需驱动包也能用上盘符功能。
func (s AdaptationService) InjectDriver(ctx context.Context, terminalID, bundleID string, overwrite bool) (InjectResult, error) {
	terminal, err := s.Store.Terminals().Get(ctx, strings.TrimSpace(terminalID))
	if err != nil {
		return InjectResult{}, err
	}
	if !terminal.IsSuper {
		return InjectResult{}, ErrAdaptationNotSuper
	}
	if s.Storage == nil || s.BundleZip == nil {
		return InjectResult{}, ErrDriverInjectionUnavailable
	}
	if err := s.ensureSuperOffline(ctx, terminal); err != nil {
		return InjectResult{}, err
	}
	resetClone := terminal.PendingBundleID != nil
	if resetClone && !overwrite {
		return InjectResult{}, ErrAdaptationOverwrite
	}
	bundleID = strings.TrimSpace(bundleID)
	bundleName := "仅盘符脚本（未附带驱动集）"
	var zip []byte
	if bundleID != "" {
		bundle, err := s.Store.DriverBundles().Get(ctx, bundleID)
		if err != nil {
			return InjectResult{}, err
		}
		if err := s.ensureBundleNotEmpty(ctx, bundle.ID); err != nil {
			return InjectResult{}, err
		}
		bundleName = bundle.Name
	}
	source, err := s.superSource(ctx, terminal)
	if err != nil {
		return InjectResult{}, err
	}
	if bundleID != "" {
		zip, err = s.BundleZip(ctx, bundleID)
		if err != nil {
			return InjectResult{}, err
		}
	}
	if err := s.hostFor(ctx, terminal).PrepareSuperAdaptation(ctx, storage.SuperAdaptationReq{
		MAC:         terminal.MAC,
		Source:      source,
		BundleZip:   zip,
		AdaptScript: installDriverScript,
		ResetClone:  resetClone,
		MountScript: s.mountScript(),
	}); err != nil {
		return InjectResult{}, err
	}
	now := s.now()
	terminal.PendingBundleID = &bundleID
	terminal.PendingBundleAt = &now
	if err := s.Store.Terminals().Update(ctx, terminal); err != nil {
		return InjectResult{}, err
	}
	payload, confirm := "驱动集与盘符脚本", "驱动装好"
	if bundleID == "" {
		payload, confirm = "盘符脚本", "盘符正确"
	}
	return InjectResult{
		BundleName: bundleName,
		Steps: []string{
			fmt.Sprintf("1. 已把%s离线注入超管机克隆盘（无需进 Windows 下载）。", payload),
			"2. 把该超管机开机一次 —— 开机时自动完成安装（不会自动关机）。",
			fmt.Sprintf("3. 在客机内确认%s后，正常关机。", confirm),
			"4. 回到本页点『停机存还原点』把改动固化成新还原点。",
		},
	}, nil
}

func (s AdaptationService) ensureSuperOffline(ctx context.Context, terminal domain.Terminal) error {
	if terminal.State == domain.TerminalStateOnline {
		return ErrAdaptationOnline
	}
	macs, err := s.Storage.ActiveClientMACs(ctx)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("unable to verify terminal offline before driver injection", "terminal", terminal.ID, "mac", terminal.MAC, "error", err)
		}
		return ErrAdaptationOnlineCheck
	}
	want := storage.NormalizeMAC(terminal.MAC)
	for _, mac := range macs {
		if storage.NormalizeMAC(mac) == want {
			return ErrAdaptationOnline
		}
	}
	return nil
}

// InjectResultView 表示最近注入的驱动包是否已在超管机上装完，读自持久克隆的临时快照+克隆，
// 不碰正在使用的启动盘。
type InjectResultView struct {
	BundleID   string    `json:"bundle_id"`
	InjectedAt time.Time `json:"injected_at"`
	Done       bool      `json:"done"` // adapt.ps1 已跑完（done.txt 存在）
	OK         bool      `json:"ok"`   // 退出码 0（驱动已安装并绑定）
	Log        string    `json:"log"`  // done.txt 原文，如 "OK=False EXIT=1 AT=..."
}

// CheckResult 读取当前待固化注入的结果，操作者开机一次后调用，不必登录查看磁盘。
func (s AdaptationService) CheckResult(ctx context.Context, terminalID string) (InjectResultView, error) {
	terminal, err := s.Store.Terminals().Get(ctx, strings.TrimSpace(terminalID))
	if err != nil {
		return InjectResultView{}, err
	}
	if !terminal.IsSuper {
		return InjectResultView{}, ErrAdaptationNotSuper
	}
	if terminal.PendingBundleID == nil {
		return InjectResultView{}, ErrAdaptationNoPending
	}
	if s.Storage == nil {
		return InjectResultView{}, ErrDriverInjectionUnavailable
	}
	result, err := s.hostFor(ctx, terminal).ReadSuperAdaptationResult(ctx, terminal.MAC)
	if err != nil {
		return InjectResultView{}, err
	}
	view := InjectResultView{BundleID: *terminal.PendingBundleID, Done: result.Done, OK: result.OK, Log: result.Log}
	if terminal.PendingBundleAt != nil {
		view.InjectedAt = *terminal.PendingBundleAt
	}
	return view, nil
}

func (s AdaptationService) ensureBundleNotEmpty(ctx context.Context, bundleID string) error {
	links, err := s.Store.DriverBundlePacks().List(ctx)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.BundleID == bundleID {
			return nil
		}
	}
	return ErrAdaptationEmptyBundle
}

// superSource 返回超管机开机用的（配置，快照）：分组系统配置最新的就绪还原点。
func (s AdaptationService) superSource(ctx context.Context, terminal domain.Terminal) (storage.ClientSource, error) {
	group, err := s.Store.Groups().Get(ctx, terminal.GroupID)
	if err != nil {
		return storage.ClientSource{}, err
	}
	reductions, err := s.Store.Reductions().List(ctx)
	if err != nil {
		return storage.ClientSource{}, err
	}
	var latest *domain.Reduction
	for i, r := range reductions {
		if r.ConfigID != group.SystemConfigID || r.Status != domain.ReductionStatusReady {
			continue
		}
		if latest == nil || r.CreatedAt.After(latest.CreatedAt) {
			latest = &reductions[i]
		}
	}
	if latest == nil {
		return storage.ClientSource{}, fmt.Errorf("group has no ready reduction to adapt: %w", errs.ErrNotFound)
	}
	snapshot := strings.TrimPrefix(strings.TrimSpace(latest.Name), "@")
	if snapshot == "" {
		return storage.ClientSource{}, fmt.Errorf("reduction snapshot is required")
	}
	return storage.ClientSource{ImageID: group.SystemImageID, ConfigID: group.SystemConfigID, SnapshotName: snapshot}, nil
}
