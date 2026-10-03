package tasks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

func waitTaskStatus(t *testing.T, st store.Store, id string, want domain.TaskStatus) domain.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := st.Tasks().Get(context.Background(), id)
		if err == nil && task.Status == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not reach %s (last: %+v, err: %v)", id, want, task, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTaskRunnerAsyncSuccess(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	r := Runner{Store: st, Now: func() time.Time { return now }, Async: true}

	task, err := r.Run(context.Background(), domain.TaskTypeCreatePool, "tank", func(ctx context.Context, task domain.Task) error {
		return r.Finish(ctx, st, task, "done")
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskStatusPending {
		t.Fatalf("initial status = %s, want %s", task.Status, domain.TaskStatusPending)
	}
	got := waitTaskStatus(t, st, task.ID, domain.TaskStatusSuccess)
	if got.Progress != 100 || got.Result != "done" {
		t.Fatalf("progress/result = %d/%q, want 100/done", got.Progress, got.Result)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want injected clock %v", got.CreatedAt, now)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Fatalf("finished_at = %v, want injected clock %v", got.FinishedAt, now)
	}
}

func TestTaskRunnerAsyncFailure(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	r := Runner{Store: st, Now: func() time.Time { return now }, Async: true}

	task, err := r.Run(context.Background(), domain.TaskTypeCreatePool, "tank", func(ctx context.Context, task domain.Task) error {
		return errors.New("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitTaskStatus(t, st, task.ID, domain.TaskStatusFailed)
	if got.Error != "boom" {
		t.Fatalf("error = %q, want boom", got.Error)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Fatalf("finished_at = %v, want injected clock %v", got.FinishedAt, now)
	}
}

func TestTaskRunnerSyncFailure(t *testing.T) {
	st := newTestStore(t)
	r := Runner{Store: st}

	task, err := r.Run(context.Background(), domain.TaskTypeCreatePool, "tank", func(ctx context.Context, task domain.Task) error {
		if task.Status != domain.TaskStatusRunning {
			t.Errorf("sync task status = %s, want %s", task.Status, domain.TaskStatusRunning)
		}
		return errors.New("boom")
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	got, getErr := st.Tasks().Get(context.Background(), task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != domain.TaskStatusFailed || got.Error != "boom" {
		t.Fatalf("task = %s/%q, want failed/boom", got.Status, got.Error)
	}
}

func TestTaskRunnerUniqueIDsUnderFixedClock(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	r := Runner{Store: st, Now: func() time.Time { return now }}

	noop := func(ctx context.Context, task domain.Task) error {
		return r.Finish(ctx, st, task, "ok")
	}
	first, err := r.Run(context.Background(), domain.TaskTypeImageHealthCheck, "img-1", noop)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Run(context.Background(), domain.TaskTypeImageHealthCheck, "img-1", noop)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("task IDs collided under fixed clock: %s", first.ID)
	}
}

// 每条任务记下执行节点；一个进程即一个节点，启动时设一次。
func TestRunnerRecordsThisNode(t *testing.T) {
	st := newTestStore(t)
	SetNode("192.168.10.3")
	t.Cleanup(func() { SetNode("") })
	task, err := (Runner{Store: st}).Create(context.Background(), domain.TaskTypeCreatePool, "data")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Tasks().Get(context.Background(), task.ID)
	if err != nil || got.Node != "192.168.10.3" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// 失败时不能用开工时的副本覆盖整行，否则执行中写入的目标和进度会丢，看不出失败的是哪个镜像。
func TestTaskRunnerFailKeepsWhatTheTaskRecordedWhileRunning(t *testing.T) {
	st := newTestStore(t)
	runner := Runner{Store: st}
	task, err := runner.Run(context.Background(), domain.TaskTypeImportImage, "", func(ctx context.Context, task domain.Task) error {
		task.TargetRef, task.Progress, task.Message = "win11", 42, "转换写入 40%"
		if err := st.Tasks().Update(ctx, task); err != nil {
			t.Fatal(err)
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("want error")
	}
	got, err := st.Tasks().Get(context.Background(), task.ID)
	if err != nil || got.Status != domain.TaskStatusFailed || got.TargetRef != "win11" || got.Progress != 42 || got.Message != "转换写入 40%" {
		t.Fatalf("task = %#v err = %v", got, err)
	}
}
