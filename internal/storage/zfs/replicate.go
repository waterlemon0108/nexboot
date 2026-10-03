package zfs

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// 目录容器在节点间以一条 `send -R` 复制流移动：接收端按 GUID 跟随改名、promote 和销毁，
// 合并或超管保存把克隆 promote 到父之上后，逐数据集增量做不到这一点。
// 这里只拼参数、搬字节；发什么、何时发由上层复制器决定。

// GUIDEntry 是 `zfs list -o name,guid,origin` 的一行：数据集或快照、其 GUID，克隆还带其源快照。
type GUIDEntry struct {
	Name   string
	GUID   string
	Origin string
	// Created 是快照创建时间（epoch 秒）。它随流传递，始终是产生快照那台节点上的时间；
	// createtxg 则会被接收端按落盘顺序重新分配。
	Created int64
}

// GUIDInventory 是节点在复制根下持有的内容，按 GUID 寻址，两端不依赖名字也能找到共同基点。
type GUIDInventory struct {
	Entries []GUIDEntry
}

func (inv GUIDInventory) HasSnapshot(name string) bool {
	for _, e := range inv.Entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

// LatestCommonSnapshot 返回两份清单中 GUID 相同的最新容器快照（@ 之后部分），没有则返回 ""，
// 此时只能发完整流。跨节点复制两端是同一根；本机备份比较的是数据池容器与备份池下的副本，
// 所以两端各自给根。「最新」指本清单中最后一个，`zfs list` 按创建排序。
func (inv GUIDInventory) LatestCommonSnapshot(other GUIDInventory, root, otherRoot string) string {
	theirs := map[string]string{}
	otherPrefix := otherRoot + "@"
	for _, e := range other.Entries {
		if name, ok := strings.CutPrefix(e.Name, otherPrefix); ok {
			theirs[name] = e.GUID
		}
	}
	prefix := root + "@"
	common := ""
	for _, e := range inv.Entries {
		name, ok := strings.CutPrefix(e.Name, prefix)
		if !ok {
			continue
		}
		if guid, ok := theirs[name]; ok && guid == e.GUID {
			common = name
		}
	}
	return common
}

// SnapshotRecursive 对 root 及其下所有数据集打一个原子递归快照。
func (c *Client) SnapshotRecursive(ctx context.Context, root, name string) error {
	if root == "" || name == "" {
		return fmt.Errorf("root and snapshot name are required")
	}
	return c.run(ctx, "snapshot", "-r", root+"@"+name)
}

// SendReplication 把 root@toSnap 的 raw 复制流写入 w。给定 fromSnap 时为增量并带上全部中间快照（-I），
// 两轮之间建的还原点也会一并到达；否则发完整流。
func (c *Client) SendReplication(ctx context.Context, root, fromSnap, toSnap string, w io.Writer) error {
	if root == "" || toSnap == "" {
		return fmt.Errorf("root and target snapshot are required")
	}
	if w == nil {
		return fmt.Errorf("stream sink is required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	args := []string{"send", "-R", "-w"}
	if fromSnap != "" {
		args = append(args, "-I", root+"@"+fromSnap)
	}
	args = append(args, root+"@"+toSnap)
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", nil, w, args...)
}

// SendResume 按中断接收留下的 token 续传。
func (c *Client) SendResume(ctx context.Context, token string, w io.Writer) error {
	if token == "" {
		return fmt.Errorf("resume token is required")
	}
	if w == nil {
		return fmt.Errorf("stream sink is required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	args := []string{"send", "-t", token}
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", nil, w, args...)
}

// SendDataset 只发一个数据集、不含子集：from（完整快照名）非空为增量，origin 非空为相对克隆源，
// 都空为完整流；to 是该数据集的快照名。递归流收不下时用它逐个数据集追平。
func (c *Client) SendDataset(ctx context.Context, dataset, from, origin, to string, w io.Writer) error {
	if dataset == "" || to == "" || w == nil {
		return fmt.Errorf("dataset, target snapshot and stream sink are required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	args := []string{"send", "-w"}
	switch {
	case origin != "":
		args = append(args, "-i", origin)
	case from != "":
		args = append(args, "-I", from)
	}
	args = append(args, dataset+"@"+to)
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", nil, w, args...)
}

// ReceiveDataset 接收单个数据集的流（见 SendDataset），与递归接收一样可续传。
func (c *Client) ReceiveDataset(ctx context.Context, dataset string, r io.Reader) error {
	if dataset == "" || r == nil {
		return fmt.Errorf("dataset and stream source are required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	args := []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", dataset}
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", r, io.Discard, args...)
}

// ReceiveReplication 把复制流收进 root：-F 跟随发送端回滚，-u 不挂载，-s 断流时保留续传 token，
// -x 拒收发送端的 mountpoint。
// mountpoint 是带发送端池名的绝对路径，`send -R` 会照带；在接收端事后改回也不行：每轮复制
// 又改回去，健康检查的修复反复卸载重挂，进行中的接收报 "failed to read from stream"，复制完全停摆。
func (c *Client) ReceiveReplication(ctx context.Context, root string, r io.Reader) error {
	if root == "" {
		return fmt.Errorf("root is required")
	}
	if r == nil {
		return fmt.Errorf("stream source is required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	args := []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", root}
	c.logger.Info("zfs command", "args", args)
	return pipeRunner.RunWithIO(ctx, "zfs", r, io.Discard, args...)
}

// ListAsideCopies 按时间从旧到新列出 root 被挪开的兄弟数据集（"<root>-rebuilding-*"、"<root>-diverged-*"）。
// 后缀是纳秒时间戳，不会冲突也不会复用，不列出来就没人能回收。
// 按时间戳而非字符串排序，"…-9" 排在 "…-10" 前，最后一个是最值得留着人工恢复的。
func (c *Client) ListAsideCopies(ctx context.Context, root string) ([]string, error) {
	parent := path.Dir(root)
	if parent == "." || parent == "/" {
		return nil, nil
	}
	out, err := c.runner.Run(ctx, "zfs", "list", "-H", "-o", "name", "-d", "1", parent)
	if err != nil {
		return nil, storage.CommandError{Name: "zfs", Args: []string{"list", parent}, Output: string(out), Err: err}
	}
	type aside struct {
		name  string
		stamp int64
	}
	var found []aside
	for _, name := range strings.Fields(string(out)) {
		for _, marker := range []string{root + "-rebuilding-", root + "-diverged-"} {
			suffix, ok := strings.CutPrefix(name, marker)
			if !ok {
				continue
			}
			stamp, cerr := strconv.ParseInt(suffix, 10, 64)
			if cerr != nil {
				continue // 不是本机造的那种名字，别碰
			}
			found = append(found, aside{name: name, stamp: stamp})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].stamp < found[j].stamp })
	names := make([]string, 0, len(found))
	for _, a := range found {
		names = append(names, a.name)
	}
	return names, nil
}

// ResumeToken 返回中断接收留下 token 的数据集及 token，没有可续传的返回 ""。
// 递归接收中断时 token 在正在写的子数据集上，不在 root 上。
func (c *Client) ResumeToken(ctx context.Context, root string) (string, string, error) {
	if root == "" {
		return "", "", fmt.Errorf("root is required")
	}
	args := []string{"get", "-H", "-r", "-t", "filesystem,volume", "-o", "name,value", "receive_resume_token", root}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return "", "", storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	dataset, token := "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] == "-" {
			continue
		}
		if fields[0] == root {
			return root, fields[1], nil
		}
		if dataset == "" {
			dataset, token = fields[0], fields[1]
		}
	}
	return dataset, token, nil
}

// AbortResume 丢弃半收的流及其续传 token，让下一次拉取从干净状态开始。
// 不可续传的 token（发送端已清掉其引用的标记、流损坏）否则会让每轮都以同样方式失败，节点永远收不到目录。
func (c *Client) AbortResume(ctx context.Context, root string) error {
	if root == "" {
		return fmt.Errorf("root is required")
	}
	args := []string{"recv", "-A", root}
	if out, err := c.runner.Run(ctx, "zfs", args...); err != nil {
		return storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	return nil
}

// ListGUIDs 按创建顺序列出 root 下所有数据集和快照及其 GUID 与 origin。
func (c *Client) ListGUIDs(ctx context.Context, root string) (GUIDInventory, error) {
	if root == "" {
		return GUIDInventory{}, fmt.Errorf("root is required")
	}
	args := []string{"list", "-H", "-p", "-r", "-t", "all", "-o", "name,guid,origin,creation", root}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return GUIDInventory{}, storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	var inv GUIDInventory
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		origin := fields[2]
		if origin == "-" {
			origin = ""
		}
		entry := GUIDEntry{Name: fields[0], GUID: fields[1], Origin: origin}
		if len(fields) > 3 {
			entry.Created, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		inv.Entries = append(inv.Entries, entry)
	}
	return inv, nil
}

// PruneSnapshots 在 root 上递归销毁带 prefix 的标记快照，只留最新 keep 个。还原点不带该前缀，永不清理；
// 调用方保留的数量不少于接收端可能还要用作增量基点的数量。接收端无需清理：-F 会镜像发送端的删除。
func (c *Client) PruneSnapshots(ctx context.Context, root, prefix string, keep int) error {
	return c.PruneSnapshotsExcept(ctx, root, prefix, keep, nil)
}

// PruneSnapshotsExcept 同 PruneSnapshots，但另外保留 pinned 中列出的标记（裸快照名），不论多旧。
func (c *Client) PruneSnapshotsExcept(ctx context.Context, root, prefix string, keep int, pinned []string) error {
	if root == "" || prefix == "" {
		return fmt.Errorf("root and prefix are required")
	}
	if keep <= 0 {
		return fmt.Errorf("keep must be positive")
	}
	args := []string{"list", "-H", "-p", "-d", "1", "-t", "snapshot", "-o", "name,createtxg", root}
	out, err := c.runner.Run(ctx, "zfs", args...)
	if err != nil {
		return storage.CommandError{Name: "zfs", Args: args, Output: string(out), Err: err}
	}
	type marker struct {
		name string
		txg  int64
	}
	var markers []marker
	snapPrefix := root + "@" + prefix
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 2 || !strings.HasPrefix(fields[0], snapPrefix) {
			continue
		}
		txg, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		markers = append(markers, marker{name: fields[0], txg: txg})
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].txg < markers[j].txg })
	if len(markers) <= keep {
		return nil
	}
	spare := map[string]bool{}
	for _, name := range pinned {
		spare[root+"@"+name] = true
	}
	for _, m := range markers[:len(markers)-keep] {
		if spare[m.name] {
			continue
		}
		if err := c.run(ctx, "destroy", "-r", m.name); err != nil {
			return err
		}
	}
	return nil
}

// Replicate 在本机以一条 send|recv 管道把 root@snapshot 复制到 target，给定 base 时为增量。
// 本机备份用它，跨节点复制器则经 HTTP 传同样的流。接收端去掉 mountpoint：副本与源同机，
// 继承 "/<pool>/nd/db" 会让开机时 `zfs mount -a` 与在用数据集争同一目录。
func (c *Client) Replicate(ctx context.Context, root, target, base, snapshot string) error {
	if root == "" || target == "" || snapshot == "" {
		return fmt.Errorf("root, target and snapshot are required")
	}
	pipeRunner, ok := c.runner.(PipeRunner)
	if !ok {
		return fmt.Errorf("zfs runner does not support send/recv")
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		args := []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", target}
		c.logger.Info("zfs command", "args", args)
		err := pipeRunner.RunWithIO(ctx, "zfs", pr, io.Discard, args...)
		_ = pr.Close()
		done <- err
	}()
	sendErr := c.SendReplication(ctx, root, base, snapshot, pw)
	_ = pw.Close()
	recvErr := <-done
	if sendErr != nil {
		return sendErr
	}
	return recvErr
}

// EnsureFilesystem 在文件系统不存在时创建它及缺失的父级；已存在则不动，类似 mkdir -p。
func (c *Client) EnsureFilesystem(ctx context.Context, dataset string) error {
	if dataset == "" {
		return fmt.Errorf("dataset is required")
	}
	return c.run(ctx, "create", "-p", dataset)
}
