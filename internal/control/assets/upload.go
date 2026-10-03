package assets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
)

// UploadService 以可续传的分片接收操作者本机的镜像文件，落到导入目录供现有导入流程使用。
// 文件动辄几十 GB，必须支持断点续传。会话编号带节点 ID：半成品只在当时的主机本地，
// VIP 漂移后新主机据此能说明「主机已切换」，而不是只答「会话不存在」。
type UploadService struct {
	// ImportDir 解析成品文件（及 .part 分片）所在的导入目录。
	ImportDir func(ctx context.Context) (string, error)
	// NodeID 写入会话编号，让别的节点区分「不是我的」和「从未存在」。
	NodeID string
	// FreeSpace 返回路径可用字节数；为 nil 时跳过空间检查。
	FreeSpace func(path string) (int64, error)
	// Now 可注入，供 Reap 计算会话年龄。
	Now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// BeginUploadRequest 开启上传会话。SizeBytes 必填，进度与空间检查都依赖它。
type BeginUploadRequest struct {
	FileName  string `json:"file_name"`
	SizeBytes int64  `json:"size_bytes"`
	// LastModified 是浏览器 File.lastModified（毫秒），用于区分同名同大小的不同文件；
	// 续传到错的分片上会得到长度正确但内容错乱的文件。
	LastModified int64 `json:"last_modified,omitempty"`
}

// UploadStatus 是所有上传接口统一的应答：已收多少、是否完成。
type UploadStatus struct {
	UploadID  string `json:"upload_id"`
	FileName  string `json:"file_name"`
	SizeBytes int64  `json:"size_bytes"`
	Received  int64  `json:"received"`
	Complete  bool   `json:"complete"`
	// Path 是成品文件路径，Complete 之前为空。
	Path string `json:"path,omitempty"`
	// NodeID 是持有分片的节点，控制台据此告诉操作者数据在哪台机器上。
	NodeID string `json:"node_id"`
}

type uploadMeta struct {
	FileName     string    `json:"file_name"`
	SizeBytes    int64     `json:"size_bytes"`
	LastModified int64     `json:"last_modified,omitempty"`
	NodeID       string    `json:"node_id"`
	StartedAt    time.Time `json:"started_at"`
}

const uploadsSubdir = ".uploads"

// resumable 查找同一文件（同名、同大小、同修改时间）未完成的上传。只比名字和大小
// 会把同名的不同镜像续到一起，静默损坏。多个分片匹配时取已收最多的。
func (s *UploadService) resumable(dir, name string, size, lastModified int64) (string, int64, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	bestID, bestReceived, found := "", int64(-1), false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		var meta uploadMeta
		if err := readJSONFile(filepath.Join(dir, e.Name()), &meta); err != nil {
			continue
		}
		if meta.FileName != name || meta.SizeBytes != size {
			continue
		}
		// 旧版本分片没记修改时间，不参与比对；记了的必须一致。
		if meta.LastModified != 0 && meta.LastModified != lastModified {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, id+".part"))
		if err != nil || info.Size() >= size {
			continue // 已经传完的（或分片没了的）不算断点
		}
		if info.Size() > bestReceived {
			bestID, bestReceived, found = id, info.Size(), true
		}
	}
	if !found {
		return "", 0, false
	}
	return bestID, bestReceived, true
}

func (s *UploadService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Begin 在传输任何字节前完成所有能提前做的校验，避免传了很久才被拒。
func (s *UploadService) Begin(ctx context.Context, req BeginUploadRequest) (UploadStatus, error) {
	name := filepath.Base(strings.TrimSpace(req.FileName))
	if name == "" || name == "." || name == string(filepath.Separator) ||
		strings.Contains(req.FileName, "..") {
		return UploadStatus{}, errs.Invalid("文件名不合法")
	}
	if !isSupportedImportImageFile(strings.ToLower(name)) {
		return UploadStatus{}, errs.Invalid(
			"只支持 .zfs/.gz/.gzip 流或 .vmdk/.vhd/.vhdx/.qcow2/.vdi/.raw 磁盘镜像，收到的是 " + name)
	}
	if req.SizeBytes <= 0 {
		return UploadStatus{}, errs.Invalid("需要文件大小才能断点续传")
	}
	dir, err := s.partDir(ctx)
	if err != nil {
		return UploadStatus{}, err
	}
	// 成品会落到导入目录的同名文件上，那份可能正以「导入后删除源文件」在导入，传完再发现就白传了。
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), name)); err == nil {
		return UploadStatus{}, errs.Conflict(fmt.Sprintf("请先移走或删除导入目录里的 %s，或把要上传的文件改个名：导入目录已有同名文件", name))
	} else if !os.IsNotExist(err) {
		return UploadStatus{}, err
	}
	// 先找盘上的断点，否则刷新页面后丢了内存里的 upload_id 就只能从头传。
	resumeID, resumed, canResume := s.resumable(dir, name, req.SizeBytes, req.LastModified)

	// 空间按「还差多少」判：已落盘部分已占在该目录，按全长比会把这些字节算两遍，
	// 越接近传完越容易被误拒。
	if s.FreeSpace != nil {
		needed := req.SizeBytes
		if canResume {
			needed -= resumed
		}
		if free, err := s.FreeSpace(dir); err == nil && free < needed {
			return UploadStatus{}, errs.Conflict(fmt.Sprintf(
				"导入目录只剩 %d 字节，这次上传还需要 %d 字节（文件共 %d 字节，已传 %d）。"+
					"请先清理导入目录或换一台空间更足的主机",
				free, needed, req.SizeBytes, resumed))
		}
	}

	if canResume {
		return UploadStatus{UploadID: resumeID, FileName: name, SizeBytes: req.SizeBytes,
			Received: resumed, NodeID: s.NodeID}, nil
	}

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return UploadStatus{}, err
	}
	id := s.NodeID + "-" + hex.EncodeToString(raw)
	meta := uploadMeta{FileName: name, SizeBytes: req.SizeBytes, LastModified: req.LastModified, NodeID: s.NodeID, StartedAt: s.now()}
	if err := writeJSONFile(filepath.Join(dir, id+".json"), meta); err != nil {
		return UploadStatus{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".part"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return UploadStatus{}, err
	}
	_ = f.Close()
	return UploadStatus{UploadID: id, FileName: name, SizeBytes: req.SizeBytes, NodeID: s.NodeID}, nil
}

// Status 返回已收进度，浏览器续传前据此确定起点。
func (s *UploadService) Status(ctx context.Context, id string) (UploadStatus, error) {
	dir, meta, err := s.session(ctx, id)
	if err != nil {
		return UploadStatus{}, err
	}
	received := partSize(filepath.Join(dir, id+".part"))
	return UploadStatus{
		UploadID: id, FileName: meta.FileName, SizeBytes: meta.SizeBytes,
		Received: received, Complete: received >= meta.SizeBytes, NodeID: meta.NodeID,
	}, nil
}

// Append 在调用方给出的偏移处写入一个分片。偏移必须校验：越过末尾会留下空洞，
// 文件长度对但镜像已损坏，要到很久以后的体检才暴露。与已收部分重叠属正常重试，只写超出部分。
func (s *UploadService) Append(ctx context.Context, id string, offset int64, body io.Reader) (UploadStatus, error) {
	dir, meta, err := s.session(ctx, id)
	if err != nil {
		return UploadStatus{}, err
	}
	unlock := s.lock(id)
	defer unlock()
	// 等锁期间会话可能已被 Reap 或 Abort 清掉。
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		return UploadStatus{}, errs.NotFound("上传会话不存在或已过期，请重新上传")
	}

	partPath := filepath.Join(dir, id+".part")
	received := partSize(partPath)
	if offset > received {
		return UploadStatus{}, errs.Conflict(fmt.Sprintf(
			"分片起点 %d 超出了已收到的 %d 字节，中间会留下空洞。请从 %d 继续",
			offset, received, received))
	}
	if offset < received {
		// 重发已经收下的部分：跳过重叠，只写超出的尾巴。
		skip := received - offset
		if _, err := io.CopyN(io.Discard, body, skip); err != nil && err != io.EOF {
			return UploadStatus{}, err
		}
	}
	f, err := os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return UploadStatus{}, err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return UploadStatus{}, err
	}
	if err := f.Close(); err != nil {
		return UploadStatus{}, err
	}

	received = partSize(partPath)
	out := UploadStatus{
		UploadID: id, FileName: meta.FileName, SizeBytes: meta.SizeBytes,
		Received: received, NodeID: meta.NodeID,
	}
	if received < meta.SizeBytes {
		return out, nil
	}
	if received > meta.SizeBytes {
		return UploadStatus{}, errs.Conflict(fmt.Sprintf(
			"收到的字节数 %d 超过了声明的 %d，上传已作废，请重新上传", received, meta.SizeBytes))
	}
	final, err := s.finish(ctx, id, meta, partPath)
	if err != nil {
		return UploadStatus{}, err
	}
	out.Complete, out.Path = true, final
	return out, nil
}

// Abort 丢弃操作者放弃的上传会话。
func (s *UploadService) Abort(ctx context.Context, id string) error {
	dir, _, err := s.session(ctx, id)
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, id+".part"))
	_ = os.Remove(filepath.Join(dir, id+".json"))
	return nil
}

// Reap 删除超时无人续传的会话，避免废弃分片占满运行中克隆所需的磁盘。
func (s *UploadService) Reap(ctx context.Context, olderThan time.Duration) int {
	dir, err := s.partDir(ctx)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cut, n := s.now().Add(-olderThan), 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if s.reapOne(dir, id, cut) {
			n++
		}
	}
	return n
}

// reapOne 按分片最后一次写入判断过期：大文件续传可以超过一天，按开始时间算会删掉还在传的。
// 正在写入的会话（持有会话锁）跳过，下一轮再看。
func (s *UploadService) reapOne(dir, id string, cut time.Time) bool {
	unlock, ok := s.tryLock(id)
	if !ok {
		return false
	}
	defer unlock()
	last, err := os.Stat(filepath.Join(dir, id+".part"))
	if err != nil {
		if last, err = os.Stat(filepath.Join(dir, id+".json")); err != nil {
			return false
		}
	}
	if last.ModTime().After(cut) {
		return false
	}
	_ = os.Remove(filepath.Join(dir, id+".part"))
	_ = os.Remove(filepath.Join(dir, id+".json"))
	return true
}

func (s *UploadService) finish(ctx context.Context, id string, meta uploadMeta, partPath string) (string, error) {
	importDir, err := s.ImportDir(ctx)
	if err != nil {
		return "", err
	}
	final, err := placeWithoutOverwrite(partPath, importDir, meta.FileName)
	if err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(filepath.Dir(partPath), id+".json"))
	return final, nil
}

// placeWithoutOverwrite 把分片放到导入目录，绝不覆盖已有文件：Begin 已拒绝同名，
// 上传期间才出现的同名文件则改名避让（"win11-2.zfs"），应答里的 Path 是实际落点。
// 用硬链接而非 rename，因为 rename 会静默替换目标。
func placeWithoutOverwrite(partPath, dir, name string) (string, error) {
	stem, ext := splitImportExt(name)
	for n := 1; n < 1000; n++ {
		final := filepath.Join(dir, name)
		if n > 1 {
			final = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, n, ext))
		}
		err := os.Link(partPath, final)
		if err == nil {
			_ = os.Remove(partPath)
			return final, nil
		}
		if os.IsExist(err) {
			continue
		}
		// 不支持硬链接的文件系统退回先查后 rename，只剩极窄的竞态窗口。
		if _, statErr := os.Lstat(final); statErr == nil {
			continue
		}
		if err := os.Rename(partPath, final); err != nil {
			return "", err
		}
		return final, nil
	}
	return "", errs.Conflict(fmt.Sprintf("请先清理导入目录里以 %s 开头的同名文件再上传", stem))
}

// splitImportExt 把 "win11.zfs.gz" 拆成 "win11" 和 ".zfs.gz"，避让名才仍能按扩展名识别。
func splitImportExt(name string) (string, string) {
	lower := strings.ToLower(name)
	for _, ext := range []string{".zfs.gz", ".zfs.gzip"} {
		if strings.HasSuffix(lower, ext) {
			return name[:len(name)-len(ext)], name[len(name)-len(ext):]
		}
	}
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext), ext
}

// session 解析会话编号，并在此区分「主机已切换」与「会话不存在」。
func (s *UploadService) session(ctx context.Context, id string) (string, uploadMeta, error) {
	if id == "" {
		return "", uploadMeta{}, errs.Invalid("缺少上传编号")
	}
	if s.NodeID != "" && !strings.HasPrefix(id, s.NodeID+"-") {
		owner := id
		if i := strings.LastIndex(id, "-"); i > 0 {
			owner = id[:i]
		}
		return "", uploadMeta{}, errs.Conflict(fmt.Sprintf(
			"这次上传是发往节点 %s 的，而现在的主机是 %s——主机已经切换，已传的部分留在原来那台上。请重新上传",
			owner, s.NodeID))
	}
	dir, err := s.partDir(ctx)
	if err != nil {
		return "", uploadMeta{}, err
	}
	var meta uploadMeta
	if err := readJSONFile(filepath.Join(dir, id+".json"), &meta); err != nil {
		return "", uploadMeta{}, errs.NotFound("上传会话不存在或已过期，请重新上传")
	}
	return dir, meta, nil
}

func (s *UploadService) partDir(ctx context.Context) (string, error) {
	base, err := s.ImportDir(ctx)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, uploadsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// lock 串行化同一会话的分片，并发追加会交错成长度对、内容错的文件。
func (s *UploadService) lock(id string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// tryLock 取会话锁，已被持有时立即返回 false。
func (s *UploadService) tryLock(id string) (func(), bool) {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	s.mu.Unlock()
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

func partSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
