package zfs

import (
	"errors"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// OperatorMessage 把存储失败翻译成操作者能照着做的话。
// 异步任务失败记录的是它而不是 zfs 原始输出：原文按池路径点名数据集、引用失败的 argv，
// 有时还附整段 usage，对按按钮的人没有用。原始错误仍写进日志。
func OperatorMessage(err error) string {
	if err == nil {
		return ""
	}
	var cmdErr storage.CommandError
	if !errors.As(err, &cmdErr) {
		return err.Error()
	}
	switch {
	case IsBusy(err):
		return "存储对象正被占用（通常是有客户机还在使用），请稍后重试"
	case IsNotExist(err):
		return "操作所需的数据集或快照不存在，可能已被删除，或上一步操作没有完成"
	case IsExists(err):
		return "目标已存在，可能上一次操作已经完成"
	case IsNotClone(err):
		return "该数据集已经是链的根，无需再次提升"
	case strings.Contains(cmdErr.Output, "dependent clones"):
		return "还有派生对象依赖它，请先删除那些派生的配置或客户机克隆"
	case strings.Contains(cmdErr.Output, "conflicting snapshot"):
		return "存在同名快照冲突，请先合并或删除重名的还原点"
	case strings.Contains(cmdErr.Output, "out of space"),
		strings.Contains(cmdErr.Output, "quota exceeded"):
		return "存储池空间不足"
	case strings.Contains(cmdErr.Output, "exists and is not empty"):
		dir := "目标目录"
		if _, rest, ok := strings.Cut(cmdErr.Output, "mountpoint '"); ok {
			if d, _, ok := strings.Cut(rest, "'"); ok {
				dir = d
			}
		}
		return "挂载目录 " + dir + " 已存在且不为空，请换一个池名，或先清空该目录"
	case strings.Contains(cmdErr.Output, "name is reserved"):
		return "这个名字是 ZFS 保留名（mirror / raidz / draid / spare 开头及 log），请换一个"
	}
	return "存储操作失败：" + firstLine(cmdErr.Output)
}

// firstLine 只保留 zfs 开头那句，丢掉有时附带的 usage 横幅。
func firstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "usage:") {
			continue
		}
		return line
	}
	return strings.TrimSpace(output)
}
