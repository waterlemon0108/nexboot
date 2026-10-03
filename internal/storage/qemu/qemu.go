// Package qemu 封装 qemu-img，把外部磁盘镜像（vmdk/vhd/vhdx/qcow2/vdi/raw）转成 raw 直接写入 zvol。
// 只用于磁盘镜像导入，ZFS 流导入不经过这里。
package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// Runner 抽象 qemu-img 的执行，便于测试导入路径。
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	// RunWithProgress 把 stdout 实时写入 writer（以便解析 qemu-img -p 进度），stderr 收集后在出错时返回。
	RunWithProgress(ctx context.Context, stdout io.Writer, name string, args ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (ExecRunner) RunWithProgress(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return storage.CommandError{Name: name, Args: args, Output: strings.TrimSpace(stderr.String()), Err: err}
	}
	return nil
}

type Client struct {
	runner Runner
	logger *slog.Logger
}

func New(runner Runner, logger *slog.Logger) *Client {
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{runner: runner, logger: logger}
}

// Available 报告 PATH 中是否有 qemu-img。
func Available() bool {
	_, err := exec.LookPath("qemu-img")
	return err == nil
}

type ImageInfo struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
}

// Info 探测源镜像的格式和虚拟大小。
func (c *Client) Info(ctx context.Context, path string) (ImageInfo, error) {
	out, err := c.runner.Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return ImageInfo{}, storage.CommandError{Name: "qemu-img", Args: []string{"info", "--output=json", path}, Output: strings.TrimSpace(string(out)), Err: err}
	}
	var info ImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return ImageInfo{}, fmt.Errorf("parse qemu-img info for %s: %w", path, err)
	}
	if info.VirtualSize <= 0 {
		return ImageInfo{}, fmt.Errorf("qemu-img reported non-positive virtual size for %s", path)
	}
	return info, nil
}

// ConvertToRaw 把 srcPath 转成 raw 写入 devPath（zvol 设备节点）。srcFormat 为空时自动识别；
// onProgress 非 nil 时按 0-100 报告进度。
func (c *Client) ConvertToRaw(ctx context.Context, srcPath, srcFormat, devPath string, onProgress func(percent int)) error {
	args := []string{"convert", "-p", "-O", "raw"}
	if srcFormat != "" {
		args = append(args, "-f", srcFormat)
	}
	args = append(args, srcPath, devPath)
	c.logger.Info("qemu-img convert", "src", srcPath, "format", srcFormat, "dev", devPath)
	pw := &progressWriter{onProgress: onProgress}
	if err := c.runner.RunWithProgress(ctx, pw, "qemu-img", args...); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(100)
	}
	return nil
}

// qemu-img -p 以 \r 刷新输出 "    (73.40/100%)" 这样的进度行。
var progressRe = regexp.MustCompile(`\(\s*([0-9]+(?:\.[0-9]+)?)/100%\)`)

type progressWriter struct {
	buf        []byte
	lastPct    int
	onProgress func(int)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if w.onProgress == nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexAny(w.buf, "\r\n")
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		w.emit(line)
	}
	return len(p), nil
}

func (w *progressWriter) emit(line string) {
	m := progressRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return
	}
	pct := int(f)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if pct != w.lastPct {
		w.lastPct = pct
		w.onProgress(pct)
	}
}
