package assets

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/config"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/control/tasks"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrImageExists = errs.Conflict("镜像已存在")
	ErrImageInUse  = errs.Conflict("镜像正在使用中")
)

type ImageService struct {
	Store             store.Store
	Storage           storage.StorageAgent
	Now               func() time.Time
	Async             bool
	ImportDir         string
	ImportDirResolver func(context.Context) (string, error)
	// NodeAddr 是本机在控制台中的地址。服务器目录类接口的文件只在某一节点上，
	// 应答都要写明是哪台。
	NodeAddr string
	// MountScriptBake 返回导入时要烘焙进该 OS 镜像的启动脚本及其版本，开机路径据此
	// 免去逐台注入；成对返回保证记录的版本与写入的字节一致。以注入方式提供，因为脚本
	// 在 adapt 包，assets 不能依赖它（见 internal/control/doc.go）。为 nil 时不烘焙，每次开机注入。
	MountScriptBake func(domain.OSType) (script []byte, version string)
	// Replicating 返回正在接收这些数据集的备机；为 nil 表示没有。
	Replicating func(ids ...string) []string
	// DirFree 返回目录所在文件系统的剩余字节，供导出到目录的空间预检；为 nil 时用 statfs。
	DirFree func(dir string) (int64, error)
}

type ImportImageRequest struct {
	Name       string        `json:"name"`
	SourcePath string        `json:"source_path"`
	OSType     domain.OSType `json:"os_type"`
	// Purpose 为 "system"（默认）或 "data"：数据盘不烘焙启动脚本、不做可启动检查，只能作数据盘用。
	Purpose string `json:"purpose"`
	// DeleteSourceAfter 在导入成功后删除源文件。仅由浏览器上传流程设置（文件是它自己产生的），
	// 避免几十 GB 的副本占着克隆所需的磁盘；默认不开，操作者自己放的文件不能替他删。
	DeleteSourceAfter bool `json:"delete_source_after"`
}

// BlankImageRequest 新建空白数据盘镜像：指定大小的稀疏卷，含一个已格式化分区。
// Label 是客户机看到的卷标，为空时用盘名。
type BlankImageRequest struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	Filesystem string `json:"filesystem"` // ntfs | ext4
	Label      string `json:"label"`
}

type SetPurposeRequest struct {
	Purpose string `json:"purpose"`
}

type ImportImageResult struct {
	TaskID      string `json:"task_id"`
	ImageID     string `json:"image_id"`
	ConfigID    string `json:"config_id"`
	ReductionID string `json:"reduction_id"`
}

// ImageItem 是列表中的镜像：Size 为客户机看到的逻辑大小，Used 为镜像及其配置在池上
// 的实际占用（压缩后、仅写时复制增量）。查不到池时 Used 为 nil，不用 0 代替未知。
type ImageItem struct {
	domain.Image
	Used *int64
}

type ImageListResult struct {
	Items []ImageItem `json:"items"`
	Total int         `json:"total"`
}

type ImageDetail struct {
	Image   domain.Image    `json:"image"`
	Configs []domain.Config `json:"configs"`
}

func (s ImageService) ImportImage(ctx context.Context, req ImportImageRequest) (ImportImageResult, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.SourcePath = strings.TrimSpace(req.SourcePath)
	if req.Name == "" || req.SourcePath == "" || req.OSType == "" {
		return ImportImageResult{}, errs.Invalid("name, source_path and os_type are required")
	}
	if req.OSType != domain.OSTypeWindows && req.OSType != domain.OSTypeLinux {
		return ImportImageResult{}, errs.Invalid("invalid os_type: " + string(req.OSType))
	}
	purpose, err := parsePurpose(req.Purpose)
	if err != nil {
		return ImportImageResult{}, err
	}
	req.Purpose = string(purpose)
	importDir, err := s.importDir(ctx)
	if err != nil {
		return ImportImageResult{}, err
	}
	if err := validateImportImageSource(req.SourcePath, importDir); err != nil {
		return ImportImageResult{}, err
	}
	claim, err := claimImageName(req.Name, "导入")
	if err != nil {
		return ImportImageResult{}, err
	}
	defer claim.Drop()
	if _, err := s.Store.Images().GetByName(ctx, req.Name); err == nil {
		return ImportImageResult{}, ErrImageExists
	} else if !errs.IsNotFound(err) {
		return ImportImageResult{}, err
	}
	if err := s.ensureImportFits(ctx, req.SourcePath); err != nil {
		return ImportImageResult{}, err
	}

	var imported storage.ImportImageResult
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeImportImage, "", func(ctx context.Context, task domain.Task) error {
		var err error
		imported, err = s.executeImportImage(ctx, task, req, importDir)
		if err != nil {
			// 失败时保留源文件，操作者多半要改参数重来。
			return err
		}
		if req.DeleteSourceAfter {
			// 删除失败不算导入失败，否则操作者会误以为镜像有问题而重导。
			_ = os.Remove(req.SourcePath)
		}
		return nil
	})
	if err != nil {
		return ImportImageResult{}, err
	}
	if s.Async {
		return ImportImageResult{TaskID: task.ID}, nil
	}
	return ImportImageResult{
		TaskID:      task.ID,
		ImageID:     imported.Image.ID,
		ConfigID:    imported.Config.ID,
		ReductionID: imported.Reduction.ID,
	}, nil
}

func parsePurpose(raw string) (domain.ImagePurpose, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "system":
		return domain.ImagePurposeSystem, nil
	case "data":
		return domain.ImagePurposeData, nil
	}
	return "", errs.Invalid("镜像用途只能是 system（系统盘）或 data（数据盘）")
}

const blankImageMinBytes = 1 << 30

// CreateBlankImage 新建只含一个已格式化分区的数据盘镜像，内容由超管机写入。提交时校验
// 名称、大小、文件系统，以及逻辑大小不超过池可用空间（虽是稀疏卷，池永远装不下的盘必是填错了）。
func (s ImageService) CreateBlankImage(ctx context.Context, req BlankImageRequest) (ImportImageResult, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return ImportImageResult{}, errs.Invalid("请填写数据盘镜像的名称")
	}
	if req.SizeBytes < blankImageMinBytes {
		return ImportImageResult{}, errs.Invalid("数据盘大小至少 1 GiB")
	}
	fs := strings.ToLower(strings.TrimSpace(req.Filesystem))
	if fs != "ntfs" && fs != "ext4" {
		return ImportImageResult{}, errs.Invalid("文件系统只能是 NTFS（Windows 客户机）或 ext4（Linux 客户机）")
	}
	req.Filesystem = fs
	claim, err := claimImageName(req.Name, "新建")
	if err != nil {
		return ImportImageResult{}, err
	}
	defer claim.Drop()
	if _, err := s.Store.Images().GetByName(ctx, req.Name); err == nil {
		return ImportImageResult{}, ErrImageExists
	} else if !errs.IsNotFound(err) {
		return ImportImageResult{}, err
	}
	if s.Storage != nil {
		if usage, err := s.Storage.SpaceUsage(ctx); err == nil && req.SizeBytes > usage.PoolAvailable {
			return ImportImageResult{}, errs.Conflict(fmt.Sprintf("数据盘大小 %s 超过存储池可用 %s，请改小或先给存储池腾出空间", fmtBytes(req.SizeBytes), fmtBytes(usage.PoolAvailable)))
		}
	}
	var created storage.ImportImageResult
	task, err := claim.Run(ctx, s.runner(), domain.TaskTypeCreateBlankImage, "", func(ctx context.Context, task domain.Task) error {
		var err error
		created, err = s.executeCreateBlank(ctx, task, req)
		return err
	})
	if err != nil {
		return ImportImageResult{}, err
	}
	if s.Async {
		return ImportImageResult{TaskID: task.ID}, nil
	}
	return ImportImageResult{TaskID: task.ID, ImageID: created.Image.ID, ConfigID: created.Config.ID, ReductionID: created.Reduction.ID}, nil
}

func (s ImageService) executeCreateBlank(ctx context.Context, task domain.Task, req BlankImageRequest) (storage.ImportImageResult, error) {
	// 池与数据库在此一起变更，中间不能插入复制轮次（见 storage.ChangeCatalogue）；
	// 只需几秒，复制可以等。
	defer storage.ChangeCatalogue()()
	storageReq := storage.BlankImageReq{
		Name: req.Name, SizeBytes: req.SizeBytes, Filesystem: req.Filesystem, Label: strings.TrimSpace(req.Label),
		OnTarget: func(imageID string) { recordTarget(ctx, s.Store, &task, imageID) },
		OnProgress: func(percent int, message string) {
			t := task
			t.Status = domain.TaskStatusRunning
			t.Progress = percent
			t.Message = message
			_ = s.Store.Tasks().Update(ctx, t)
		},
	}
	created, err := s.Storage.CreateBlankImage(ctx, storageReq)
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Images().Create(ctx, created.Image); err != nil {
			return err
		}
		if err := tx.Configs().Create(ctx, created.Config); err != nil {
			return err
		}
		if err := tx.Reductions().Create(ctx, created.Reduction); err != nil {
			return err
		}
		task.TargetRef = created.Image.ID
		return s.runner().Finish(ctx, tx, task, created.Image.ID)
	}); err != nil {
		if rollbackErr := s.Storage.RollbackImportImage(context.Background(), storage.ImportImageReq{Name: req.Name}, created); rollbackErr != nil {
			return storage.ImportImageResult{}, errors.Join(err, rollbackErr)
		}
		return storage.ImportImageResult{}, err
	}
	return created, nil
}

// SetPurpose 修改镜像用途。若有分组正以原用途绑定它则拒绝，否则该分组下次开机会出错。
func (s ImageService) SetPurpose(ctx context.Context, id string, req SetPurposeRequest) (domain.Image, error) {
	img, err := s.Store.Images().Get(ctx, id)
	if err != nil {
		return domain.Image{}, err
	}
	purpose, err := parsePurpose(req.Purpose)
	if err != nil {
		return domain.Image{}, err
	}
	if purpose == img.Purpose || (img.Purpose == "" && purpose == domain.ImagePurposeSystem) {
		return img, nil
	}
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return domain.Image{}, err
	}
	var users []string
	for _, g := range ix.groups {
		if purpose == domain.ImagePurposeData && g.SystemImageID == id {
			users = append(users, g.Name)
		}
	}
	if purpose == domain.ImagePurposeSystem {
		groupName := map[string]string{}
		for _, g := range ix.groups {
			groupName[g.ID] = g.Name
		}
		for _, d := range ix.disks {
			if d.ImageID == id {
				users = append(users, groupName[d.GroupID]+" 的数据盘 "+d.MountTarget)
			}
		}
	}
	if len(users) > 0 {
		return domain.Image{}, errs.Conflict(fmt.Sprintf("请先在分组里改绑其他镜像再改用途：%s 正被 %s 使用", img.Name, strings.Join(users, "、")))
	}
	img.Purpose = purpose
	if err := s.Store.Images().Update(ctx, img); err != nil {
		return domain.Image{}, err
	}
	if purpose == domain.ImagePurposeData {
		s.dropHealthReport(ctx, img.ID)
	}
	return img, nil
}

// importStreamSize 估算池需要接收的字节数。gzip 末 4 字节记录的解压长度对 4 GiB 取模，
// 只有大于文件本身时才可信；回绕时退回文件大小作为下限。
func importStreamSize(path string) (int64, bool) {
	size, ok := sourceDataSize(path)
	if !ok {
		return 0, false
	}
	isize, gzipped := gzipUncompressedSize(path)
	if gzipped && isize > size {
		return isize, true
	}
	return size, true
}

func gzipUncompressedSize(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return 0, false
	}
	var tail [4]byte
	if _, err := f.Seek(-4, io.SeekEnd); err != nil {
		return 0, true
	}
	if _, err := io.ReadFull(f, tail[:]); err != nil {
		return 0, true
	}
	return int64(binary.LittleEndian.Uint32(tail[:])), true
}

// ensureImportFits 在提交时拒绝源数据已超过池剩余空间的导入，免得拷到一半才失败。
// 只在确定放不下时拒绝；读不到源或问不到池时交给导入本身报真实错误。
func (s ImageService) ensureImportFits(ctx context.Context, sourcePath string) error {
	need, ok := importStreamSize(sourcePath)
	if !ok || s.Storage == nil {
		return nil
	}
	usage, err := s.Storage.SpaceUsage(ctx)
	if err != nil {
		slog.Warn("import space check skipped", "source", sourcePath, "error", err)
		return nil
	}
	if need <= usage.PoolAvailable {
		return nil
	}
	return errs.Conflict(fmt.Sprintf("请先给存储池腾出空间再导入 %s：可用 %s，源文件数据 %s。删除不用的镜像或还原点，或给存储池加盘",
		filepath.Base(sourcePath), fmtBytes(usage.PoolAvailable), fmtBytes(need)))
}

// poolAvailable 返回池剩余空间；问不到时返回 nil 表示未知，而不是 0。
func (s ImageService) poolAvailable(ctx context.Context) *int64 {
	if s.Storage == nil {
		return nil
	}
	usage, err := s.Storage.SpaceUsage(ctx)
	if err != nil {
		slog.Warn("pool space unavailable", "error", err)
		return nil
	}
	available := usage.PoolAvailable
	return &available
}

func fmtBytes(b int64) string {
	const gib = 1 << 30
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}

// recordTarget 在存储层选定对象后立即写入任务的 TargetRef：数据集先于数据库行出现，
// 一致性检查会跳过活动任务点名的对象。只改目标，不动进度。
func recordTarget(ctx context.Context, st store.Store, task *domain.Task, id string) {
	task.TargetRef = id
	if stored, err := st.Tasks().Get(ctx, task.ID); err == nil {
		stored.TargetRef = id
		_ = st.Tasks().Update(ctx, stored)
	}
}

func (s ImageService) executeImportImage(ctx context.Context, task domain.Task, req ImportImageRequest, importDir string) (storage.ImportImageResult, error) {
	var bakeScript []byte
	var bakeVersion string
	if s.MountScriptBake != nil {
		bakeScript, bakeVersion = s.MountScriptBake(req.OSType)
	}
	purpose := domain.ImagePurpose(req.Purpose)
	if purpose == "" {
		purpose = domain.ImagePurposeSystem
	}
	if purpose == domain.ImagePurposeData {
		bakeScript = nil // 数据盘上没有东西会执行它
	}
	importReq := storage.ImportImageReq{
		Name:        req.Name,
		SourcePath:  req.SourcePath,
		ImportDir:   importDir,
		OSType:      req.OSType,
		Purpose:     purpose,
		MountScript: bakeScript,
		OnTarget:    func(imageID string) { recordTarget(ctx, s.Store, &task, imageID) },
		OnProgress: func(percent int, message string) {
			t := task
			t.Status = domain.TaskStatusRunning
			t.Progress = percent
			t.Message = message
			_ = s.Store.Tasks().Update(ctx, t)
		},
	}
	imported, err := s.Storage.ImportImage(ctx, importReq)
	if err != nil {
		return storage.ImportImageResult{}, err
	}
	// 存储层确认脚本已写入才记录烘焙版本，否则开机路径会跳过注入，客户机没有默认路由。
	if imported.MountScriptInjected {
		imported.Image.MountScriptVersion = bakeVersion
	}

	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Images().Create(ctx, imported.Image); err != nil {
			return err
		}
		if err := tx.Configs().Create(ctx, imported.Config); err != nil {
			return err
		}
		if err := tx.Reductions().Create(ctx, imported.Reduction); err != nil {
			return err
		}
		task.TargetRef = imported.Image.ID
		task.Status = domain.TaskStatusRunning
		task.Progress = 98
		task.Message = "写入数据库"
		return tx.Tasks().Update(ctx, task)
	}); err != nil {
		if rollbackErr := s.rollbackImport(context.Background(), importReq, imported); rollbackErr != nil {
			return storage.ImportImageResult{}, errors.Join(err, rollbackErr)
		}
		return storage.ImportImageResult{}, err
	}

	// 导入后体检（issue 035），尽力而为：数据已提交，体检失败只体现在它自己的任务上，
	// 不影响导入成功。数据盘不可启动，不体检。
	if imported.Image.Purpose != domain.ImagePurposeData {
		task.Progress = 99
		task.Message = "校验镜像"
		_ = s.Store.Tasks().Update(ctx, task)
		_, _ = s.RunHealthCheck(ctx, imported.Image.ID)
	}

	// 体检之后才标记完成，让 100%/「完成」覆盖整个流程而非只是拷贝阶段。
	task.Status = domain.TaskStatusSuccess
	task.Progress = 100
	task.Message = "完成"
	task.Result = imported.Image.ID
	finished := s.now()
	task.FinishedAt = &finished
	_ = s.Store.Tasks().Update(ctx, task)
	return imported, nil
}

func validateImportImageSource(sourcePath, importDir string) error {
	clean := filepath.Clean(strings.TrimSpace(sourcePath))
	importDir = filepath.Clean(strings.TrimSpace(importDir))
	if importDir == "" || !filepath.IsAbs(importDir) {
		return errs.Invalid("导入目录没有设成绝对路径，请到「系统参数」里改好导入目录再导入")
	}
	if !filepath.IsAbs(clean) || clean == importDir || !strings.HasPrefix(clean, importDir+"/") {
		return errs.Invalid(fmt.Sprintf("请把文件放到导入目录 %s 下再导入：%s 不在这个目录里", importDir, strings.TrimSpace(sourcePath)))
	}
	lower := strings.ToLower(clean)
	if !isSupportedImportImageFile(lower) {
		return errs.Invalid(fmt.Sprintf("不支持 %s 这种文件：请用 .vmdk/.vhd/.vhdx/.qcow2/.vdi/.raw/.img 磁盘文件，或本产品导出的 .zfs/.zfs.gz", filepath.Base(clean)))
	}
	return nil
}

var supportedImportExts = []string{
	// .gz 既可能是手工改名的本产品导出，也可能是客户工具产物；实际格式在导入时按内容判断。
	".gzip", ".zfs", ".gz",
	".vmdk", ".vhd", ".vhdx", ".qcow2", ".qcow", ".vdi", ".raw", ".img",
}

func isSupportedImportImageFile(path string) bool {
	for _, ext := range supportedImportExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func (s ImageService) importDir(ctx context.Context) (string, error) {
	if s.ImportDirResolver != nil {
		dir, err := s.ImportDirResolver(ctx)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(dir) != "" {
			return dir, nil
		}
	}
	if strings.TrimSpace(s.ImportDir) == "" {
		return config.DefaultImportDir, nil
	}
	return s.ImportDir, nil
}

type ImportSource struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Format string `json:"format"`
}

type ImportSourceListResult struct {
	Items     []ImportSource `json:"items"`
	ImportDir string         `json:"import_dir"`
	// Node 是应答的节点。随列表一起返回，保证路径与机器不会对不上。
	Node  string `json:"node"`
	Total int    `json:"total"`
	// PoolAvailable 是数据池剩余空间，与文件大小并列，提交前就能看出放不下；问不到池时为 nil。
	PoolAvailable *int64 `json:"pool_available"`
}

// exportableImage 是两条导出路径共用的检查：镜像须存在且状态稳定，导入中或异常的镜像不能导出。
func (s ImageService) exportableImage(ctx context.Context, id string) (domain.Image, error) {
	img, err := s.Store.Images().Get(ctx, id)
	if err != nil {
		return domain.Image{}, err
	}
	switch img.State {
	case domain.ImageStateImporting:
		return domain.Image{}, errs.Conflict(fmt.Sprintf("镜像「%s」正在导入，请等导入完成后再导出", img.Name))
	case domain.ImageStateError:
		return domain.Image{}, errs.Conflict(fmt.Sprintf("镜像「%s」状态异常，不能导出；请先重新导入", img.Name))
	}
	return img, nil
}

// ImageExportRequest 请求导出到本机磁盘。
type ImageExportRequest struct {
	ImageID string `json:"-"`
}

// ExportImageResult 给出文件落点、所在机器及对应任务。
type ExportImageResult struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
	Node   string `json:"node"`
}

// ExportImageToDir 把导出流写入本节点的导入目录，盘到盘不经网络。代价是文件落在当前
// 写入者这一台上，控制台会写明是哪台。导出中途角色切换则任务失败，不在无人关注的节点上完成。
func (s ImageService) ExportImageToDir(ctx context.Context, req ImageExportRequest) (ExportImageResult, error) {
	img, err := s.exportableImage(ctx, req.ImageID)
	if err != nil {
		return ExportImageResult{}, err
	}
	dir, err := s.importDir(ctx)
	if err != nil {
		return ExportImageResult{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ExportImageResult{}, err
	}
	if err := s.ensureExportFits(ctx, img, dir); err != nil {
		return ExportImageResult{}, err
	}
	path := filepath.Join(dir, sanitizeExportName(img.Name)+".zfs")
	// 建任务前先占住文件名，否则同一镜像的两次导出都会启动，失败的一方留下看似可导入的截断文件。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return ExportImageResult{}, errs.Conflict(fmt.Sprintf("导出文件 %s 已存在，请先移走或删除它", filepath.Base(path)))
		}
		return ExportImageResult{}, err
	}
	_ = f.Close()
	_ = os.Remove(path)

	done := beginExport(img.ID)
	task, err := s.runner().Create(ctx, domain.TaskTypeExportImage, img.ID)
	if err != nil {
		done()
		return ExportImageResult{}, err
	}
	run := func() {
		defer done()
		if err := s.executeExportImage(context.Background(), task, img, path); err != nil {
			_ = s.runner().Fail(context.Background(), task, err)
		}
	}
	if s.Async {
		go run()
	} else {
		run()
	}
	return ExportImageResult{TaskID: task.ID, Path: path, Node: s.NodeAddr}, nil
}

func (s ImageService) executeExportImage(ctx context.Context, task domain.Task, img domain.Image, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return errs.Conflict(fmt.Sprintf("导出文件 %s 已存在，请稍后再试", filepath.Base(path)))
		}
		return err
	}
	lastPercent := -1
	var lastWritten int64
	err = s.Storage.ExportImage(ctx, storage.ExportImageReq{
		ImageID: img.ID,
		W:       f,
		OnProgress: func(written, estimated int64) {
			// 每变化 1% 更新一次任务；没有估算值时百分比停在 99，改为每 256 MiB 更新。
			percent := 99
			if estimated > 0 {
				if p := int(written * 100 / estimated); p < percent {
					percent = p
				}
			}
			if percent == lastPercent && written-lastWritten < 256<<20 {
				return
			}
			lastPercent, lastWritten = percent, written
			t := task
			t.Status = domain.TaskStatusRunning
			t.Progress = percent
			if estimated > 0 {
				t.Message = fmt.Sprintf("已导出 %s / 约 %s", fmtBytes(written), fmtBytes(estimated))
			} else {
				t.Message = "已导出 " + fmtBytes(written)
			}
			_ = s.Store.Tasks().Update(ctx, t)
		},
	})
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		// 写了一半的流将来导入会中途失败，宁可不留文件。
		_ = os.Remove(path)
		return err
	}
	return s.runner().Finish(ctx, s.Store, task, filepath.Base(path))
}

// ExportDownloadName 返回下载附件名，并先做与导出流相同的检查。
func (s ImageService) ExportDownloadName(ctx context.Context, id string, compress bool) (string, error) {
	img, err := s.exportableImage(ctx, id)
	if err != nil {
		return "", err
	}
	if compress {
		return sanitizeExportName(img.Name) + ".zfs.gz", nil
	}
	return sanitizeExportName(img.Name) + ".zfs", nil
}

// StreamImage 把可再导入的镜像流直接写入 w（HTTP 响应体），不落文件、不建任务；
// 连接断开经 ctx 取消发送。raw send 带的是池的 lz4 压缩，gzip 还能再压不少，
// 但要消耗导出节点 CPU，所以由调用方选择。
func (s ImageService) StreamImage(ctx context.Context, id string, w io.Writer, compress bool) error {
	img, err := s.exportableImage(ctx, id)
	if err != nil {
		return err
	}
	defer beginExport(img.ID)()
	return s.streamExport(ctx, storage.ExportImageReq{ImageID: img.ID}, w, compress)
}

func (s ImageService) streamExport(ctx context.Context, req storage.ExportImageReq, w io.Writer, compress bool) error {
	if !compress {
		req.W = w
		return s.Storage.ExportImage(ctx, req)
	}
	// 用 BestSpeed：千兆链路上 level 1 跟得上线速且压掉大部分字节，默认级别只有其三分之一速度，
	// 反而比不压缩更慢。
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	req.W = gz
	if err := s.Storage.ExportImage(ctx, req); err != nil {
		return err
	}
	// 必须 Close 写出 gzip 尾部，否则下载看似完整，gunzip 却报截断。
	return gz.Close()
}

// ensureExportFits 拒绝预估大小超过导入目录剩余空间的导出。只在确定放不下时拒绝，
// 估算或 statfs 失败则交给导出本身报真实错误。
func (s ImageService) ensureExportFits(ctx context.Context, img domain.Image, dir string) error {
	need, err := s.Storage.ExportImageSize(ctx, img.ID)
	if err != nil || need <= 0 {
		slog.Warn("export space check skipped", "image", img.ID, "error", err)
		return nil
	}
	free, err := s.dirFree(dir)
	if err != nil {
		slog.Warn("export space check skipped", "dir", dir, "error", err)
		return nil
	}
	if need <= free {
		return nil
	}
	return errs.Conflict(fmt.Sprintf("导出目录空间不足：导出「%s」约需 %s，%s 所在磁盘仅剩 %s。请先清理该目录，或改用「下载到本地」",
		img.Name, fmtBytes(need), dir, fmtBytes(free)))
}

func (s ImageService) dirFree(dir string) (int64, error) {
	if s.DirFree != nil {
		return s.DirFree(dir)
	}
	return statfsFree(dir)
}

// sanitizeExportName 保留镜像名的可读性，只替换文件系统或下载不能承载的字符。
func sanitizeExportName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "image"
	}
	return b.String()
}

// ListImportSources 扫描导入目录，返回可导入的文件（ZFS send 流或 qemu 可转换的磁盘镜像）。
// 格式只按扩展名判断，不逐个调用 qemu-img 探测。
func (s ImageService) ListImportSources(ctx context.Context) (ImportSourceListResult, error) {
	dir, err := s.importDir(ctx)
	if err != nil {
		return ImportSourceListResult{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ImportSourceListResult{}, err
	}
	items := make([]ImportSource, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if !isSupportedImportImageFile(lower) {
			continue
		}
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		items = append(items, ImportSource{
			Name:   e.Name(),
			Path:   filepath.Join(dir, e.Name()),
			Size:   size,
			Format: importFormatLabel(lower),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return ImportSourceListResult{Items: items, ImportDir: dir, Node: s.NodeAddr, Total: len(items), PoolAvailable: s.poolAvailable(ctx)}, nil
}

func importFormatLabel(lower string) string {
	switch {
	case strings.HasSuffix(lower, ".zfs.gz"), strings.HasSuffix(lower, ".gzip"), strings.HasSuffix(lower, ".zfs"):
		return "zfs-send"
	case strings.HasSuffix(lower, ".vmdk"):
		return "vmdk"
	case strings.HasSuffix(lower, ".vhdx"):
		return "vhdx"
	case strings.HasSuffix(lower, ".vhd"):
		return "vhd"
	case strings.HasSuffix(lower, ".qcow2"), strings.HasSuffix(lower, ".qcow"):
		return "qcow2"
	case strings.HasSuffix(lower, ".vdi"):
		return "vdi"
	case strings.HasSuffix(lower, ".raw"), strings.HasSuffix(lower, ".img"):
		return "raw"
	default:
		return "unknown"
	}
}

func (s ImageService) rollbackImport(ctx context.Context, req storage.ImportImageReq, imported storage.ImportImageResult) error {
	return s.Storage.RollbackImportImage(ctx, req, imported)
}

func (s ImageService) GetTask(ctx context.Context, id string) (domain.Task, error) {
	return s.Store.Tasks().Get(ctx, id)
}

func (s ImageService) ListActiveTasks(ctx context.Context) ([]domain.Task, error) {
	return s.Store.Tasks().ListActive(ctx)
}

type TaskListQuery struct {
	Status string
	Page   int
	Size   int
}

type TaskListResult struct {
	Items []domain.Task `json:"items"`
	Total int           `json:"total"`
	Page  int           `json:"page"`
	Size  int           `json:"size"`
	// Unreachable 列出读不到任务的节点，避免把不完整的列表当成完整的。
	Unreachable []string `json:"unreachable,omitempty"`
}

func (s ImageService) ListTasks(ctx context.Context, q TaskListQuery) (TaskListResult, error) {
	page := q.Page
	if page < 1 {
		page = 1
	}
	size := q.Size
	if size < 1 || size > 100 {
		size = 20
	}
	items, total, err := s.Store.Tasks().ListPaged(ctx, q.Status, size, (page-1)*size)
	if err != nil {
		return TaskListResult{}, err
	}
	if items == nil {
		items = []domain.Task{}
	}
	return TaskListResult{Items: items, Total: total, Page: page, Size: size}, nil
}

func (s ImageService) ListImages(ctx context.Context) (ImageListResult, error) {
	images, err := s.Store.Images().List(ctx)
	if err != nil {
		return ImageListResult{}, err
	}
	configs, err := s.Store.Configs().List(ctx)
	if err != nil {
		return ImageListResult{}, err
	}
	// 占用量问不到时留空为未知，不影响列表返回。
	var volumes map[string]storage.VolumeUsage
	if s.Storage != nil {
		if usage, err := s.Storage.SpaceUsage(ctx); err != nil {
			slog.Warn("image usage unavailable", "error", err)
		} else {
			volumes = usage.Volumes
		}
	}
	configsOf := map[string][]string{}
	for _, cfg := range configs {
		configsOf[cfg.ImageID] = append(configsOf[cfg.ImageID], cfg.ID)
	}
	items := make([]ImageItem, 0, len(images))
	for _, img := range images {
		item := ImageItem{Image: img}
		if volumes != nil {
			// 镜像卷加上其所有配置克隆，即删除镜像能释放的空间；客户机克隆不计入。
			if v, ok := volumes[img.ID]; ok {
				if v.Size > 0 {
					item.Size = v.Size
				}
				used := v.Used
				for _, cfgID := range configsOf[img.ID] {
					used += volumes[cfgID].Used
				}
				item.Used = &used
			}
		}
		items = append(items, item)
	}
	return ImageListResult{Items: items, Total: len(items)}, nil
}

func (s ImageService) GetImage(ctx context.Context, id string) (ImageDetail, error) {
	img, err := s.Store.Images().Get(ctx, id)
	if err != nil {
		return ImageDetail{}, err
	}
	configs, err := s.Store.Configs().ListByImage(ctx, id)
	if err != nil {
		return ImageDetail{}, err
	}
	return ImageDetail{Image: img, Configs: configs}, nil
}

// replicatingRefusal 在备机正接收该镜像时拒绝删除：发送期间快照被占用，destroy 不会成功。
func (s ImageService) replicatingRefusal(img domain.Image, configIDs []string) error {
	if s.Replicating == nil {
		return nil
	}
	standbys := s.Replicating(append([]string{img.ID}, configIDs...)...)
	if len(standbys) == 0 {
		return nil
	}
	return errs.Conflict(fmt.Sprintf("请等备机 %s 收完镜像「%s」再删：它正在同步这个镜像，同步期间删不掉",
		strings.Join(standbys, "、"), img.Name))
}

func (s ImageService) DeleteImage(ctx context.Context, id string) error {
	img, err := s.Store.Images().Get(ctx, id)
	if err != nil {
		return err
	}
	// 只串行占用检查和claim登记；慢物理删除期间由镜像claim保护新启用，
	// 不阻塞无关镜像下的客户机操作。
	terminalAllocMu.Lock()
	var claim *imageClaim
	err = refuseSuperImageChange(ctx, s.Store, id, "删除镜像")
	if err == nil {
		claim, err = claimImages(ctx, s.Store, taskTypeDeleteImage, id)
	}
	terminalAllocMu.Unlock()
	if err != nil {
		return err
	}
	defer claim.Drop()
	if err := exportRefusal(ctx, s.Store, id, "删除"); err != nil {
		return err
	}
	configs, err := s.Store.Configs().ListByImage(ctx, id)
	if err != nil {
		return err
	}
	// 所有检查都在第一个破坏性步骤之前完成，否则后面才发现占用时前面的配置已被删掉。
	configIDs := make([]string, 0, len(configs))
	var reductionIDs []string
	for _, cfg := range configs {
		configIDs = append(configIDs, cfg.ID)
		reductions, err := s.Store.Reductions().ListByConfig(ctx, cfg.ID)
		if err != nil {
			return err
		}
		for _, reduction := range reductions {
			reductionIDs = append(reductionIDs, reduction.ID)
		}
	}
	ix, err := loadRefIndex(ctx, s.Store)
	if err != nil {
		return err
	}
	if users := ix.describeImageUsers(id, configIDs, reductionIDs); len(users) > 0 {
		return blocked(ErrImageInUse, fmt.Sprintf("请先让 %s 改用别的镜像", strings.Join(users, "、")))
	}
	// 池里可能有数据库不知道的克隆（分叉出的配置、孤儿客户机克隆）。本次本就要删的镜像及其配置
	// 不算阻碍；镜像自身也要算进去：合并在 promote 后中断会让镜像挂在配置下面。
	own := make(map[string]bool, len(configIDs)+1)
	own[id] = true
	for _, cfgID := range configIDs {
		own[cfgID] = true
	}
	for _, cfgID := range configIDs {
		dependents, err := s.Storage.ConfigDependents(ctx, cfgID)
		if err != nil {
			if !errors.Is(err, storage.ErrNotImplemented) {
				return err
			}
			continue
		}
		if blockers := filterDependents(dependents, own); len(blockers) > 0 {
			return blocked(ErrConfigHasDependents, explainBlockers(ctx, s.Store, blockers, "该镜像"))
		}
	}
	if err := s.replicatingRefusal(img, configIDs); err != nil {
		return err
	}
	// 下面的 destroy 与删行对复制而言是同一次变更。
	defer storage.ChangeCatalogue()()
	if err := s.Storage.DeleteImage(ctx, id, configIDs); err != nil {
		if zfs.IsBusy(err) {
			if refusal := s.replicatingRefusal(img, configIDs); refusal != nil {
				return refusal
			}
			return errs.Conflict(fmt.Sprintf("镜像「%s」的数据正被占用，没有删掉：请稍等一两分钟再删", img.Name))
		}
		return err
	}
	for _, cfg := range configs {
		if err := s.Store.Reductions().DeleteByConfig(ctx, cfg.ID); err != nil {
			return err
		}
		if err := s.Store.Configs().Delete(ctx, cfg.ID); err != nil {
			return err
		}
	}
	return s.Store.Images().Delete(ctx, id)
}

func (s ImageService) runner() tasks.Runner {
	return tasks.Runner{Store: s.Store, Now: s.Now, Async: s.Async}
}

func (s ImageService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
