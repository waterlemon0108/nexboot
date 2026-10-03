package tasks

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

// Runner 管理各服务共用的任务生命周期：建任务行、同步或后台执行、记录失败终态。
// 成功由操作自己经 Finish 记录，以便并入操作的事务。
type Runner struct {
	Store store.Store
	Now   func() time.Time
	Async bool
}

// Run 为 typ/target 建任务并执行 fn。Async 时 fn 在后台 goroutine 跑、立即返回待执行任务；
// 否则同步执行，返回的非 nil 错误已记到任务上。
func (r Runner) Run(ctx context.Context, typ domain.TaskType, target string, fn func(context.Context, domain.Task) error) (domain.Task, error) {
	task, err := r.Create(ctx, typ, target)
	if err != nil {
		return domain.Task{}, err
	}
	if r.Async {
		launchTask(r.Store, task, fn, r.Fail)
		return task, nil
	}
	if err := fn(ctx, task); err != nil {
		_ = r.Fail(ctx, task, err)
		return task, err
	}
	return task, nil
}

// node 是本进程所建任务记录的节点地址；一个进程即一个节点，启动时设一次。
var node atomic.Value

// SetNode 设置此后新建任务记录的节点地址。
func SetNode(addr string) { node.Store(addr) }

func thisNode() string {
	addr, _ := node.Load().(string)
	return addr
}

func (r Runner) Create(ctx context.Context, typ domain.TaskType, target string) (domain.Task, error) {
	task := domain.Task{
		Node: thisNode(),
		// ID 用真实时钟纳秒：注入固定的 Now 时同类型任务会撞 ID。
		ID:        fmt.Sprintf("task-%s-%d", typ, time.Now().UnixNano()),
		Type:      typ,
		TargetRef: target,
		Status:    initialTaskStatus(r.Async),
		CreatedAt: r.now(),
	}
	return task, r.Store.Tasks().Create(ctx, task)
}

// Finish 经 st 把任务标为成功，st 可以是操作提交自身写入的事务。
func (r Runner) Finish(ctx context.Context, st store.Store, task domain.Task, result string) error {
	task.Status = domain.TaskStatusSuccess
	task.Progress = 100
	task.Result = result
	finished := r.now()
	task.FinishedAt = &finished
	return st.Tasks().Update(ctx, task)
}

// Fail 用操作者能懂的话记录失败：界面原样展示该字段，不能塞 zfs 原文（参数、池路径、usage 横幅）。
func (r Runner) Fail(ctx context.Context, task domain.Task, cause error) error {
	// 以库里的行为准：调用方的副本是开工时的，行里还有执行中写入的目标和进度。
	if stored, err := r.Store.Tasks().Get(ctx, task.ID); err == nil {
		task = stored
	}
	task.Status = domain.TaskStatusFailed
	task.Error = zfs.OperatorMessage(cause)
	finished := r.now()
	task.FinishedAt = &finished
	return r.Store.Tasks().Update(ctx, task)
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
