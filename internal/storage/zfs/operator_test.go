package zfs

import (
	"errors"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

func TestOperatorMessageSaysWhatToDo(t *testing.T) {
	cmd := func(out string) error {
		return storage.CommandError{Name: "zfs", Output: out, Err: errors.New("exit status 1")}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"忙", cmd("cannot destroy 'tank/x': dataset is busy"), "请稍后重试"},
		{"挂载目录不空", cmd("mountpoint '/ndiskless/data' exists and is not empty\nuse '-m' option to provide a different default"),
			"挂载目录 /ndiskless/data 已存在且不为空，请换一个池名，或先清空该目录"},
		{"不存在", cmd("cannot open 'tank/x': dataset does not exist"), "没有完成"},
		{"已存在", cmd("cannot create snapshot 'tank/x@0': dataset already exists"), "已经完成"},
		{"有派生", cmd("cannot destroy 'tank/x@0': snapshot has dependent clones\nuse '-R'"), "派生"},
		{"快照冲突", cmd("cannot promote 'tank/x': conflicting snapshot '0' from parent"), "重名"},
		{"空间不足", cmd("cannot create 'tank/x': out of space"), "空间不足"},
		{"保留名", storage.CommandError{Name: "zpool", Output: "cannot create 'mirrors': name is reserved", Err: errors.New("exit status 1")}, "保留"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OperatorMessage(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("OperatorMessage = %q, want it to mention %q", got, tc.want)
			}
			if strings.Contains(got, "usage:") || strings.Contains(got, "zfs [") {
				t.Fatalf("raw command text leaked to the operator: %q", got)
			}
		})
	}
}

// zfs 有些错误会附整段 usage 横幅，那是命令行帮助，不该给操作者看。
func TestOperatorMessageDropsTheUsageBanner(t *testing.T) {
	err := storage.CommandError{
		Name:   "zfs",
		Output: "cannot do the thing: something unmapped\nusage:\n\tsnapshot [-r] [-o property=value]...\n\nFor further help...",
		Err:    errors.New("exit status 2"),
	}
	got := OperatorMessage(err)
	if strings.Contains(got, "usage") || strings.Contains(got, "-o property") {
		t.Fatalf("banner survived: %q", got)
	}
	if !strings.Contains(got, "something unmapped") {
		t.Fatalf("the actual reason was dropped: %q", got)
	}
}

// 非命令错误本就是我们自己的措辞，原样透传。
func TestOperatorMessagePassesThroughNonCommandErrors(t *testing.T) {
	if got := OperatorMessage(errors.New("配置不存在")); got != "配置不存在" {
		t.Fatalf("got %q", got)
	}
}
