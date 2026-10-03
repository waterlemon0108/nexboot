package tasks

import (
	"context"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func initialTaskStatus(async bool) domain.TaskStatus {
	if async {
		return domain.TaskStatusPending
	}
	return domain.TaskStatusRunning
}

func launchTask(st store.Store, task domain.Task, run func(context.Context, domain.Task) error, fail func(context.Context, domain.Task, error) error) {
	go func() {
		ctx := context.Background()
		task.Status = domain.TaskStatusRunning
		task.Progress = 0
		if err := st.Tasks().Update(ctx, task); err != nil {
			_ = fail(ctx, task, err)
			return
		}
		if err := run(ctx, task); err != nil {
			_ = fail(ctx, task, err)
		}
	}()
}

func FailInterruptedTasks(ctx context.Context, st store.Store, now func() time.Time) error {
	tasks, err := st.Tasks().List(ctx)
	if err != nil {
		return err
	}
	finished := now()
	for _, task := range tasks {
		if task.Status != domain.TaskStatusPending && task.Status != domain.TaskStatusRunning {
			continue
		}
		task.Status = domain.TaskStatusFailed
		task.Error = "service restarted during task execution"
		task.FinishedAt = &finished
		if err := st.Tasks().Update(ctx, task); err != nil {
			return err
		}
	}
	return nil
}
