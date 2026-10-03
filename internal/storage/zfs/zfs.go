package zfs

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type PipeRunner interface {
	RunWithIO(context.Context, string, io.Reader, io.Writer, ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (ExecRunner) RunWithIO(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	return storage.CommandError{Name: name, Args: args, Output: stderr.String(), Err: err}
}

type Client struct {
	// pool 运行期可变：节点可无池启动，之后在界面建池并经 SetPool 绑定，
	// 启动路径可能与建池任务并发读写，故加锁。
	poolMu sync.RWMutex
	pool   string
	runner Runner
	logger *slog.Logger
	// statDir 判断挂载点是否真的可读；测试替换它，因为 /<pool>/nd/db 只在真有池的机器上存在。
	statDir         func(string) error
	removeEmptyDirs func(string)
}

func (c *Client) readable(dir string) error {
	if c.statDir != nil {
		return c.statDir(dir)
	}
	_, err := os.Stat(dir)
	return err
}

// poolName 在锁内读当前池名；所有读 c.pool 的地方都走这里，保证与 SetPool 并发时一致。
func (c *Client) poolName() string {
	c.poolMu.RLock()
	defer c.poolMu.RUnlock()
	return c.pool
}

// SetPool 把客户端绑定到运行期选定的池（操作者刚在界面建的池），之后的数据集路径都随之改变。
// 允许传空：尚无池的节点读回 ""。
func (c *Client) SetPool(name string) {
	c.poolMu.Lock()
	c.pool = name
	c.poolMu.Unlock()
}

type PoolStatus struct {
	Name      string
	Health    string
	Operation string
	Progress  string
	// Capacity 是 ZFS 口径的 used + available，即池真正能写入的字节数；
	// 不用 `zpool list size`，它含永远写不进去的 slop 预留（raidz 还含校验）。
	Capacity   int64
	Used       int64
	Layout     domain.PoolLayout
	GroupWidth int
	Vdevs      []domain.PoolVdev
	Disks      []domain.PoolDiskStatus
}

// VolumeUsage 是 zvol 的逻辑大小（volsize，客户机看到的）与池上实占（used，
// 压缩后；克隆只算不与源共享的块）。
type VolumeUsage struct {
	Size int64
	Used int64
}

// SpaceUsage 是一次 `zfs list` 得到的数据池占用：池根的 used/available，以及按完整数据集名索引的各卷。
type SpaceUsage struct {
	PoolUsed      int64
	PoolAvailable int64
	Volumes       map[string]VolumeUsage
}

func New(pool string, runner Runner, logger *slog.Logger) *Client {
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{pool: pool, runner: runner, logger: logger}
}

func (c *Client) Snapshot(configID, reductionID string) string {
	return c.dataset(configID) + "@" + reductionID
}

func (c *Client) Dataset(name string) string {
	return c.dataset(name)
}

func (c *Client) PoolName() string {
	return c.poolName()
}

func (c *Client) VolumePath(dataset string) string {
	return path.Join("/dev/zvol", dataset)
}

func (c *Client) Clone(ctx context.Context, snapshot, dataset string) error {
	return c.run(ctx, "clone", snapshot, dataset)
}

// CreateVolume 建一个至少 sizeBytes 的稀疏 zvol。volblockBytes > 0 时设为卷块大小，
// 并把 sizeBytes 向上取整到其倍数（zfs 要求 volsize 是 volblocksize 的倍数）。
// 用于磁盘镜像导入：qemu-img convert 直接写 /dev/zvol/<dataset>。
func (c *Client) CreateVolume(ctx context.Context, dataset string, sizeBytes, volblockBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("volume size must be positive")
	}
	args := []string{"create", "-s"}
	if volblockBytes > 0 {
		sizeBytes = roundUp(sizeBytes, volblockBytes)
		args = append(args, "-o", "volblocksize="+strconv.FormatInt(volblockBytes, 10))
	}
	args = append(args, "-V", strconv.FormatInt(sizeBytes, 10), dataset)
	return c.run(ctx, args...)
}

func roundUp(n, unit int64) int64 {
	if unit <= 0 {
		return n
	}
	if rem := n % unit; rem != 0 {
		n += unit - rem
	}
	return n
}

func (c *Client) Backup(ctx context.Context, source, target, snapshot, previousSnapshot string) error {
	if source == "" || target == "" || snapshot == "" {
		return fmt.Errorf("source, target and snapshot are required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	if parent := path.Dir(target); parent != "." && parent != "/" {
		if err := c.run(ctx, "create", "-p", parent); err != nil {
			return err
		}
	}
	sourceSnapshot := source + "@" + snapshot
	args := []string{"send", "-w", sourceSnapshot}
	if previousSnapshot != "" {
		args = append([]string{"send", "-w", "-i", source + "@" + previousSnapshot}, sourceSnapshot)
	}
	recv := []string{"recv", "-F", target}

	r, w := io.Pipe()
	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		sendErr = pipeRunner.RunWithIO(ctx, "zfs", nil, w, args...)
		_ = w.Close()
	}()
	go func() {
		defer wg.Done()
		recvErr = pipeRunner.RunWithIO(ctx, "zfs", r, io.Discard, recv...)
		_ = r.Close()
	}()
	wg.Wait()

	if sendErr != nil {
		return sendErr
	}
	return recvErr
}

func (c *Client) Rename(ctx context.Context, source, dataset string) error {
	return c.run(ctx, "rename", source, dataset)
}

// DetachMounts 解除被挪开的子树对在用挂载点的占用。
// zfs rename 不会随数据集移动显式 mountpoint：rename <pool>/nd 后旧树的 db 子集仍挂在
// /<pool>/nd/db，遮住新收下的那份，之后写入都落进不被复制的数据集。
// 只 inherit 不够：mountpoint 已是 none 的容器属性不变，ZFS 不会卸载，残留挂载会让
// /<pool>/nd 返回 EIO。所以先由深到浅显式卸载（子挂着父卸不掉），再交回属性；单个卸载失败不中止。
func (c *Client) DetachMounts(ctx context.Context, dataset string) error {
	out, err := c.runner.Run(ctx, "zfs", "list", "-H", "-o", "name", "-r", "-t", "filesystem", dataset)
	if err == nil {
		names := strings.Fields(string(out))
		for i := len(names) - 1; i >= 0; i-- {
			// 卸不掉多半是本来就没挂载；真正挂着又卸不掉的，会在后面的 inherit 或 recv 上暴露。
			_ = c.run(ctx, "unmount", "-f", names[i])
		}
	}
	return c.run(ctx, "inherit", "-r", "mountpoint", dataset)
}

// ReceiveFile 把 filePath（可为 gzip）送进 zfs receive。onProgress 非空时按已读源文件
// 百分比（0-100）回调，每个值最多报一次。
func (c *Client) ReceiveFile(ctx context.Context, filePath, dataset string, onProgress func(percent int)) error {
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support receive")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if onProgress != nil {
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
			r = &progressReader{r: r, total: info.Size(), onProgress: onProgress}
		}
	}
	// 是否压缩看文件内容而不是扩展名：按扩展名判断，gzip 命名为 .img 会让 receive 报
	// 与压缩无关的流错误，未压缩流命名为 .gz 则报 "invalid header"。
	buf := bufio.NewReader(r)
	compressed, err := looksGzipped(buf)
	if err != nil {
		return err
	}
	r = buf
	if compressed {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	return pipeRunner.RunWithIO(ctx, "zfs", r, io.Discard, "receive", "-F", dataset)
}

// progressReader 按已读字节占 total 的百分比回调 onProgress，每个百分点最多一次。
type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	lastPct    int
	onProgress func(int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		pct := int(p.read * 100 / p.total)
		if pct > 100 {
			pct = 100
		}
		if pct != p.lastPct {
			p.lastPct = pct
			p.onProgress(pct)
		}
	}
	return n, err
}

// looksGzipped 窥视 gzip 魔数但不消费。比魔数还短的文件不算 gzip 也不报错，截断流交给 zfs receive 去报。
func looksGzipped(r *bufio.Reader) (bool, error) {
	magic, err := r.Peek(2)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return magic[0] == 0x1f && magic[1] == 0x8b, nil
}

// SendSnapshot 把 snapshot 的完整 raw 流写入 w，是镜像导出的基础。
// -w 按池上原样（含压缩）传块，无需再套 gzip；完整流自包含，不受 promote 改变克隆祖先关系的影响。
func (c *Client) SendSnapshot(ctx context.Context, snapshot string, w io.Writer) error {
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send")
	}
	c.logger.Info("zfs command", "args", []string{"send", "-w", snapshot})
	return pipeRunner.RunWithIO(ctx, "zfs", nil, w, "send", "-w", snapshot)
}

// ReceiveStream 把完整流收成一个新数据集。不加 -F：覆盖已有数据集会把它回滚并替换掉。
func (c *Client) ReceiveStream(ctx context.Context, dataset string, r io.Reader) error {
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support receive")
	}
	args := []string{"recv", "-u", "-x", "mountpoint", dataset}
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", r, io.Discard, args...)
}

// RollbackVolume 把数据集回滚到 snapshot，并销毁其后的快照。
func (c *Client) RollbackVolume(ctx context.Context, dataset, snapshot string) error {
	return c.run(ctx, "rollback", "-r", dataset+"@"+snapshot)
}

// SendSize 取 zfs send 干跑给出的大小估计，只用于进度分母和空间预检，不做精确计量。
func (c *Client) SendSize(ctx context.Context, snapshot string) (int64, error) {
	args := []string{"send", "-nP", "-w", snapshot}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "size" {
			if size, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return size, nil
			}
		}
	}
	return 0, fmt.Errorf("no size line in zfs send -nP output for %s: %q", snapshot, strings.TrimSpace(string(out)))
}

// Written 是数据集自克隆源快照以来写入的字节数：对超管机的盘即保存会存下的改动量，
// 间隔采样两次可判断是否已静止。
func (c *Client) Written(ctx context.Context, dataset string) (int64, error) {
	args := []string{"get", "-Hp", "-o", "value", "written", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid written %q for %s: %w", strings.TrimSpace(string(out)), dataset, err)
	}
	return size, nil
}

// Used 是数据集及其后代占用的空间。
func (c *Client) Used(ctx context.Context, dataset string) (int64, error) {
	args := []string{"get", "-Hp", "-o", "value", "used", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid used %q for %s: %w", strings.TrimSpace(string(out)), dataset, err)
	}
	return size, nil
}

// Referenced 是数据集在池上引用的字节（压缩后，含共享克隆块），约等于完整 raw send 的体量。
// 比干跑 send 便宜（不需要快照），供提交时的空间预检用。
func (c *Client) Referenced(ctx context.Context, dataset string) (int64, error) {
	args := []string{"get", "-Hp", "-o", "value", "referenced", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid referenced %q for %s: %w", strings.TrimSpace(string(out)), dataset, err)
	}
	return size, nil
}

func (c *Client) Promote(ctx context.Context, dataset string) error {
	return c.run(ctx, "promote", dataset)
}

func (c *Client) SnapshotVolume(ctx context.Context, dataset, snapshot string) error {
	return c.run(ctx, "snapshot", dataset+"@"+snapshot)
}

func (c *Client) DestroySnapshot(ctx context.Context, dataset, snapshot string) error {
	return c.run(ctx, "destroy", dataset+"@"+snapshot)
}

func (c *Client) Destroy(ctx context.Context, dataset string) error {
	return c.run(ctx, "destroy", "-r", dataset)
}

// IsBusy 判断 err 是否为 zfs 因数据集仍被占用而拒绝（"dataset is busy"），例如 LIO 拆除后
// 内核延迟释放 zvol 独占。CLI 报错文本的解析归本包，调用方统一走这里。
func IsBusy(err error) bool {
	var cmdErr storage.CommandError
	return errors.As(err, &cmdErr) && strings.Contains(cmdErr.Output, "dataset is busy")
}

// 以下三个判定让多步操作可从中断处重跑：ZFS 无事务，半途失败只能重跑并把「已是目标状态」当成功。
// 措辞取自 zfs(8) 原文。

// IsNotExist 判断 destroy 的目标已不存在。
func IsNotExist(err error) bool {
	var cmdErr storage.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	return strings.Contains(cmdErr.Output, "dataset does not exist") ||
		strings.Contains(cmdErr.Output, "could not find any snapshots to destroy") ||
		// zpool 措辞不同；把「已不存在」当成功的删除也要认得池不存在。
		strings.Contains(cmdErr.Output, "no such pool")
}

// IsExists 判断 create/snapshot/clone/rename 要产生的对象已存在。
func IsExists(err error) bool {
	var cmdErr storage.CommandError
	return errors.As(err, &cmdErr) && strings.Contains(cmdErr.Output, "dataset already exists")
}

// IsNotClone 判断 promote 发现数据集已在链头，即同一序列里之前的 promote 已成功。
func IsNotClone(err error) bool {
	var cmdErr storage.CommandError
	return errors.As(err, &cmdErr) && strings.Contains(cmdErr.Output, "not a cloned filesystem")
}

func (c *Client) ListDependentClones(ctx context.Context, dataset string) ([]string, error) {
	return c.listClones(ctx, func(origin string) bool {
		return strings.HasPrefix(origin, dataset+"@")
	})
}

// ListSnapshotClones 列出从某个快照克隆出的卷；ListDependentClones 针对整个数据集。
// 前者回答「还原点能否删」，后者回答「配置能否删」。
func (c *Client) ListSnapshotClones(ctx context.Context, snapshot string) ([]string, error) {
	return c.listClones(ctx, func(origin string) bool { return origin == snapshot })
}

// ListOrigins 返回池内每个卷的克隆源；根卷（zfs 显示 "-"）不列出。
func (c *Client) ListOrigins(ctx context.Context) (map[string]string, error) {
	args := []string{"list", "-H", "-t", "volume", "-o", "name,origin"}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	origins := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[1] == "-" {
			continue
		}
		origins[fields[0]] = fields[1]
	}
	return origins, nil
}

func (c *Client) listClones(ctx context.Context, match func(origin string) bool) ([]string, error) {
	args := []string{"list", "-H", "-t", "volume", "-o", "name,origin"}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	var clones []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if match(fields[1]) {
			clones = append(clones, fields[0])
		}
	}
	return clones, nil
}

// CreatePool 按 spec 建池，布局完全由这一条命令行的 vdev 语法决定（`mirror a b mirror c d` 为镜像，
// `a b c d` 为无冗余条带）。统一 ashift=12（之后移除设备要求全池 ashift 一致）和 lz4
// （产品依赖的精简置备要求零块不占空间）。
func (c *Client) CreatePool(ctx context.Context, name string, spec storage.PoolSpec) error {
	vdevs, err := vdevArgs(spec)
	if err != nil {
		return err
	}
	args := append([]string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", PoolMountpoint(name), name}, vdevs...)
	return c.runZPool(ctx, args...)
}

// vdevArgs 把 spec 的数据 vdev 渲染成 zpool 语法。
func vdevArgs(spec storage.PoolSpec) ([]string, error) {
	if len(spec.Disks) == 0 {
		return nil, fmt.Errorf("pool disks are required")
	}
	if spec.Layout == "" || spec.Layout == domain.PoolLayoutStripe {
		return append([]string{}, spec.Disks...), nil
	}
	if !spec.Layout.Valid() {
		return nil, fmt.Errorf("unsupported pool layout %q", spec.Layout)
	}
	width := spec.GroupWidth
	if width < 2 || len(spec.Disks)%width != 0 {
		return nil, fmt.Errorf("%s pool needs disks in whole groups of %d, got %d", spec.Layout, width, len(spec.Disks))
	}
	out := make([]string, 0, len(spec.Disks)+len(spec.Disks)/width)
	for i := 0; i < len(spec.Disks); i += width {
		out = append(out, string(spec.Layout))
		out = append(out, spec.Disks[i:i+width]...)
	}
	return out, nil
}

func (c *Client) DestroyPool(ctx context.Context, name string) error {
	return c.runZPool(ctx, "destroy", name)
}

// LabelClear 清掉已销毁池留在成员盘上的 ZFS 标签。`zpool destroy` 不清标签，
// 带标签的盘会被视为占用，不清则产品销毁的池的盘无法再被产品使用。
func (c *Client) LabelClear(ctx context.Context, device string) error {
	return c.runZPool(ctx, "labelclear", "-f", device)
}

// AddDisk 按布局语法向池添加整组 vdev：条带加裸盘，镜像加 `mirror a b`，raidz 加 `raidz2 …` 组。
func (c *Client) AddDisk(ctx context.Context, name string, spec storage.PoolSpec) error {
	vdevs, err := vdevArgs(spec)
	if err != nil {
		return err
	}
	args := append([]string{"add", name}, vdevs...)
	return c.runZPool(ctx, args...)
}

// AttachDisk 把 disk 挂到 target 作镜像：裸盘变两路镜像，n 路变 n+1 路。后台 resilver，池全程在线。
func (c *Client) AttachDisk(ctx context.Context, name, target, disk string) error {
	return c.runZPool(ctx, "attach", name, target, disk)
}

// RemoveDisk 移除一个顶层 vdev（裸盘或按名如 mirror-0 的整组），先迁走数据。
// 池里有 raidz vdev 时 ZFS 会拒绝，调用方需先用操作者能懂的话拦住。
func (c *Client) RemoveDisk(ctx context.Context, name, disk string) error {
	return c.runZPool(ctx, "remove", name, disk)
}

// DetachDisk 从镜像组摘掉一路，组原地变窄、不拷贝数据；两路摘一路剩裸盘。
func (c *Client) DetachDisk(ctx context.Context, name, disk string) error {
	return c.runZPool(ctx, "detach", name, disk)
}

func (c *Client) ReplaceDisk(ctx context.Context, name, oldDisk, newDisk string) error {
	return c.runZPool(ctx, "replace", name, oldDisk, newDisk)
}

// AddSpecial 添加 special vdev（元数据和小块迁到上面）。它丢了整池就丢，所以总以镜像添加，从不裸加。
func (c *Client) AddSpecial(ctx context.Context, name string, disks []string) error {
	if len(disks) < 2 {
		return fmt.Errorf("a special vdev must be mirrored: need at least 2 disks, got %d", len(disks))
	}
	args := append([]string{"add", name, "special", "mirror"}, disks...)
	return c.runZPool(ctx, args...)
}

// RemoveSpecial 按 zpool 组名（如 mirror-3）移除整个 special 组，设备移除会把元数据迁回数据 vdev。
func (c *Client) RemoveSpecial(ctx context.Context, name, group string) error {
	return c.runZPool(ctx, "remove", name, group)
}

// AddSpare 添加热备盘并开启 autoreplace，坏盘自动被接替。
func (c *Client) AddSpare(ctx context.Context, name string, disks []string) error {
	args := append([]string{"add", name, "spare"}, disks...)
	if err := c.runZPool(ctx, args...); err != nil {
		return err
	}
	return c.runZPool(ctx, "set", "autoreplace=on", name)
}

func (c *Client) RemoveSpare(ctx context.Context, name, disk string) error {
	return c.runZPool(ctx, "remove", name, disk)
}

// RaidzExpansion 报告本节点能否给该池的 raidz 组加一块盘，不能时说明缺哪一半：
// 用户态工具须为 OpenZFS 2.3+（2.2 的 zpool 不认此特性，与内核模块无关），且池的特性开关须开启。
func (c *Client) RaidzExpansion(ctx context.Context, name string) (bool, string) {
	out, err := c.runner.Run(ctx, "zpool", "version")
	if err != nil {
		return false, "无法读取 ZFS 版本：" + strings.TrimSpace(string(out))
	}
	major, minor, version := parseZFSVersion(string(out))
	if major < 2 || (major == 2 && minor < 3) {
		return false, fmt.Sprintf("本机 ZFS 用户态版本 %s 不支持 raidz 单盘扩容（需 ≥ 2.3）", version)
	}
	out, err = c.runner.Run(ctx, "zpool", "get", "-Hp", "-o", "value", "feature@raidz_expansion", name)
	state := strings.TrimSpace(string(out))
	if err != nil || state == "" {
		return false, "存储池 " + name + " 不识别 raidz_expansion 特性（ZFS 内核模块或用户态版本不成套）"
	}
	if state != "enabled" && state != "active" {
		return false, "存储池 " + name + " 未启用 raidz_expansion 特性（zpool upgrade 可开启）"
	}
	return true, ""
}

// parseZFSVersion 把 "zfs-2.2.2-0ubuntu9.4"（zpool version 首行，即用户态版本）解析为主次版本号和展示用字符串。
func parseZFSVersion(out string) (major, minor int, version string) {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	version = strings.TrimPrefix(line, "zfs-")
	if i := strings.IndexAny(version, "-_ "); i > 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		major, _ = strconv.Atoi(parts[0])
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor, version
}

func (c *Client) AddReadCache(ctx context.Context, name string, disks []string) error {
	args := append([]string{"add", name, "cache"}, disks...)
	return c.runZPool(ctx, args...)
}

func (c *Client) RemoveReadCache(ctx context.Context, name, disk string) error {
	return c.runZPool(ctx, "remove", name, disk)
}

func (c *Client) AddWriteCache(ctx context.Context, name string, disks []string) error {
	args := append([]string{"add", name, "log"}, disks...)
	return c.runZPool(ctx, args...)
}

func (c *Client) RemoveWriteCache(ctx context.Context, name, disk string) error {
	return c.runZPool(ctx, "remove", name, disk)
}

func (c *Client) FlushWriteCache(ctx context.Context, name string) error {
	return c.runZPool(ctx, "sync", name)
}

func (c *Client) PoolStatus(ctx context.Context, name string) (PoolStatus, error) {
	listArgs := []string{"list", "-Hp", "-o", "name,health", name}
	out, err := c.runner.Run(ctx, "zpool", listArgs...)
	if err != nil {
		return PoolStatus{}, storage.CommandError{Name: "zpool", Args: listArgs, Output: string(out), Err: err}
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return PoolStatus{}, fmt.Errorf("invalid zpool list output: %q", string(out))
	}
	used, available, err := c.poolSpace(ctx, name)
	if err != nil {
		return PoolStatus{}, err
	}
	details, err := c.poolDetails(ctx, name)
	if err != nil {
		return PoolStatus{}, err
	}
	return PoolStatus{Name: fields[0], Capacity: used + available, Used: used, Health: fields[1], Operation: details.Operation, Progress: details.Progress,
		Layout: details.Layout, GroupWidth: details.GroupWidth, Vdevs: details.Vdevs, Disks: details.Disks}, nil
}

// poolSpace 读池根的 used 和 available，即 ZFS 判定 `out of space` 所依据的口径。
func (c *Client) poolSpace(ctx context.Context, name string) (used, available int64, err error) {
	args := []string{"list", "-Hp", "-o", "used,available", name}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("invalid zfs list output: %q", string(out))
	}
	if used, err = strconv.ParseInt(fields[0], 10, 64); err != nil {
		return 0, 0, err
	}
	if available, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
		return 0, 0, err
	}
	return used, available, nil
}

// SpaceUsage 列一次数据池，返回池的 used/available 及各卷的逻辑大小与实占。
// 数值解析不了的行（正在创建的卷）跳过，不让整个列表失败。
func (c *Client) SpaceUsage(ctx context.Context) (SpaceUsage, error) {
	args := []string{"list", "-Hp", "-r", "-t", "filesystem,volume", "-o", "name,type,volsize,used,available", c.poolName()}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return SpaceUsage{}, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	usage := SpaceUsage{Volumes: map[string]VolumeUsage{}}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 5 {
			continue
		}
		name, kind := fields[0], fields[1]
		used, errUsed := strconv.ParseInt(fields[3], 10, 64)
		if name == c.poolName() {
			available, errAvail := strconv.ParseInt(fields[4], 10, 64)
			if errUsed == nil && errAvail == nil {
				usage.PoolUsed, usage.PoolAvailable = used, available
			}
			continue
		}
		if kind != "volume" {
			continue
		}
		size, errSize := strconv.ParseInt(fields[2], 10, 64)
		if errSize != nil || errUsed != nil {
			continue
		}
		usage.Volumes[name] = VolumeUsage{Size: size, Used: used}
	}
	return usage, nil
}

// VolumeSize 读 zvol 的 volsize。
func (c *Client) VolumeSize(ctx context.Context, dataset string) (int64, error) {
	args := []string{"get", "-Hp", "-o", "value", "volsize", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return 0, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid volsize %q for %s: %w", strings.TrimSpace(string(out)), dataset, err)
	}
	return size, nil
}

type poolDetails struct {
	Operation  string
	Progress   string
	Layout     domain.PoolLayout
	GroupWidth int
	Vdevs      []domain.PoolVdev
	Disks      []domain.PoolDiskStatus
}

func (c *Client) poolDetails(ctx context.Context, name string) (poolDetails, error) {
	out, err := c.runner.Run(ctx, "zpool", "status", "-P", name)
	if err != nil {
		return poolDetails{}, storage.CommandError{Name: "zpool", Args: []string{"status", "-P", name}, Output: string(out), Err: err}
	}
	return parsePoolDetails(string(out), name), nil
}

// parsePoolDetails 解析 `zpool status -P` 的 config 树。段标题（logs/cache/special/spares/dedup）切换角色；
// 非设备路径的名字开启顶层组（mirror-N、raidz2-N），其下设备是组成员，与段同级的设备是独立裸 vdev。
// 深度取自两空格缩进，嵌套伪 vdev（replacing-N、spare-N）透明，成员仍归外层组。
// 组编号全池共享（log 镜像也叫 mirror-1），所以组的用途看角色不看名字。
func parsePoolDetails(out, poolName string) poolDetails {
	lines := strings.Split(out, "\n")
	inConfig := false
	role := domain.PoolDiskRoleData
	var details poolDetails
	var current *domain.PoolVdev // 当前打开的顶层组
	currentDepth := -1
	flush := func() {
		if current != nil {
			details.Vdevs = append(details.Vdevs, *current)
			current = nil
			currentDepth = -1
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if op, progress := parsePoolProgress(trimmed); op != "" {
			details.Operation = op
			if progress != "" {
				details.Progress = progress
			}
		} else if details.Progress == "" {
			details.Progress = parsePercentDone(trimmed)
		}
		if trimmed == "" {
			if inConfig {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "NAME") {
			inConfig = true
			continue
		}
		if !inConfig {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 1 {
			continue
		}
		name := fields[0]
		if name == poolName {
			continue
		}
		if sectionRole, ok := statusSectionRole(name); ok {
			flush()
			role = sectionRole
			continue
		}
		depth := statusDepth(line)
		status := ""
		if len(fields) >= 2 {
			status = fields[1]
		}
		if !strings.HasPrefix(name, "/") {
			// 组。比当前组更深说明是 replacing-N 之类伪 vdev：透明，成员并入当前组。
			if current != nil && depth > currentDepth {
				continue
			}
			flush()
			current = &domain.PoolVdev{Name: name, Kind: vdevKind(name), Role: role, Status: status}
			currentDepth = depth
			continue
		}
		disk := domain.PoolDiskStatus{Path: name, Role: role, Status: status}
		if current != nil && depth > currentDepth {
			disk.Vdev = current.Name
			current.Disks = append(current.Disks, disk)
			details.Disks = append(details.Disks, disk)
			continue
		}
		// 与段同级的裸盘自成一个 vdev；spares 不是 vdev，没有组。
		flush()
		if role != domain.PoolDiskRoleSpare {
			disk.Vdev = name
		}
		details.Vdevs = append(details.Vdevs, domain.PoolVdev{Name: name, Kind: "disk", Role: role, Status: status, Disks: []domain.PoolDiskStatus{disk}})
		details.Disks = append(details.Disks, disk)
	}
	flush()
	details.Layout, details.GroupWidth = inferLayout(details.Vdevs)
	return details
}

func statusSectionRole(name string) (domain.PoolDiskRole, bool) {
	switch name {
	case "cache":
		return domain.PoolDiskRoleReadCache, true
	case "logs":
		return domain.PoolDiskRoleWriteCache, true
	case "special":
		return domain.PoolDiskRoleSpecial, true
	case "spares":
		return domain.PoolDiskRoleSpare, true
	case "dedup":
		// 产品不跟踪的角色，其盘既非数据也非缓存；归到 special，至少显示为「非数据」。
		return domain.PoolDiskRoleSpecial, true
	}
	return "", false
}

// statusDepth 是 config 行的嵌套层级：前导 tab 后每级两个空格。无缩进的输出（旧夹具）全为 0，
// 仍能解析，只是每块盘都当裸 vdev。
func statusDepth(line string) int {
	line = strings.TrimLeft(line, "\t")
	spaces := len(line) - len(strings.TrimLeft(line, " "))
	return spaces / 2
}

// vdevKind 取组名中的类型："mirror-0" -> "mirror"，"raidz2-1" -> "raidz2"，"indirect-3" -> "indirect"。
func vdevKind(name string) string {
	if i := strings.LastIndex(name, "-"); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i]
		}
	}
	return name
}

// inferLayout 从数据组推断布局。数据组里有裸盘即视为条带（丢它就丢全部）；
// 同类型的组按该类型、取首组宽度；产品不建的类型（draid、镜像与 raidz 混用）回落为条带，
// 这对「加盘」提供的选项是安全的答案。
func inferLayout(vdevs []domain.PoolVdev) (domain.PoolLayout, int) {
	kind := ""
	width := 0
	for _, v := range vdevs {
		if v.Role != domain.PoolDiskRoleData || v.Kind == "indirect" {
			continue
		}
		if v.Kind == "disk" {
			return domain.PoolLayoutStripe, 1
		}
		if kind == "" {
			kind, width = v.Kind, len(v.Disks)
			continue
		}
		if v.Kind != kind {
			return domain.PoolLayoutStripe, 1
		}
	}
	layout := domain.PoolLayout(kind)
	if kind == "" || !layout.Valid() {
		return domain.PoolLayoutStripe, 1
	}
	if width < 1 {
		width = 1
	}
	return layout, width
}

// parsePoolProgress 只认 scan:/remove: 行里的进行中：已完成的记录、status 说明和 REMOVED 设备行
// 同样含 resilver/remov 字样，按子串匹配会让页面一直显示进行中。
func parsePoolProgress(line string) (string, string) {
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "scan:") && strings.Contains(lower, "resilver in progress"):
		return "resilver", parsePercentDone(line)
	case strings.HasPrefix(lower, "remove:") && strings.Contains(lower, "in progress"):
		return "remove", parsePercentDone(line)
	default:
		return "", ""
	}
}

func parsePercentDone(line string) string {
	fields := strings.Fields(line)
	for i, field := range fields {
		if strings.HasSuffix(field, "%") && i+1 < len(fields) && strings.TrimRight(fields[i+1], ",") == "done" {
			return strings.TrimRight(field, ",")
		}
	}
	return ""
}

// SnapshotsOf 按 createtxg 从旧到新列出数据集自身的快照（裸名）。
// 还原点由操作者命名，按名字排序与时间无关。
func (c *Client) SnapshotsOf(ctx context.Context, dataset string) ([]string, error) {
	args := []string{"list", "-H", "-t", "snapshot", "-o", "name", "-s", "createtxg", "-d", "1", dataset}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	var snaps []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if _, snap, ok := strings.Cut(name, "@"); ok {
			snaps = append(snaps, snap)
		}
	}
	return snaps, nil
}

// ListSnapshots 列出池内全部快照，用于把库里的还原点与池实际内容对账。
func (c *Client) ListSnapshots(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, "snapshot")
}

// ListPoolNames 列出本机导入的全部 zpool。备份池从这里推导而不让操作者填：节点最多两个池，
// 一个存数据，另一个就是备份池，让人填只会填错；而且在目录副本可能滞后的备机上也准确。
func (c *Client) ListPoolNames(ctx context.Context) ([]string, error) {
	args := []string{"list", "-H", "-o", "name"}
	out, err := c.runner.Run(ctx, "zpool", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zpool", Args: args, Output: string(out), Err: err}
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

func (c *Client) ListDatasets(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, "")
}

// listNames 列出某类数据集名（"" 为默认的文件系统和卷，"snapshot" 为快照）。
func (c *Client) listNames(ctx context.Context, kind string) ([]string, error) {
	args := []string{"list", "-H", "-o", "name"}
	if kind != "" {
		args = []string{"list", "-H", "-t", kind, "-o", "name"}
	}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	lines := strings.Split(string(out), "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func (c *Client) dataset(name string) string {
	return path.Join(c.poolName(), storage.ContainerFor(name), name)
}

func (c *Client) run(ctx context.Context, args ...string) error {
	c.logger.Info("zfs command", "args", args)
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err == nil {
		return nil
	}
	return storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
}

func (c *Client) runZPool(ctx context.Context, args ...string) error {
	c.logger.Info("zpool command", "args", args)
	out, err := c.runner.Run(ctx, "zpool", args...)
	if err == nil {
		return nil
	}
	return storage.CommandError{Name: "zpool", Args: args, Output: string(out), Err: err}
}

// CommandError 是共享命令错误的别名，保留 zfs.CommandError 的现有引用。
type CommandError = storage.CommandError

var IsCommandError = storage.IsCommandError
