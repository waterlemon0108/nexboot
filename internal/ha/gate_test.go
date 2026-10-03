package ha

import (
	"errors"
	"strings"
	"testing"
)

// 闸门是单写入者守卫：在允许修改的节点上打开，在备机上关闭。关闭时必须用运维能执行的话指出主机在哪。
func TestGateOpenAndClosed(t *testing.T) {
	g := NewOpenGate()
	if err := g.Allow(); err != nil {
		t.Fatalf("open gate refused: %v", err)
	}

	g.Close("本节点为备机", "http://192.168.50.10:8080")
	err := g.Allow()
	if err == nil {
		t.Fatal("closed gate allowed a write")
	}
	var closed *ClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("err = %T, want *ClosedError", err)
	}
	if closed.ActiveURL != "http://192.168.50.10:8080" || !strings.Contains(closed.Error(), "备机") {
		t.Fatalf("closed = %#v", closed)
	}

	g.Open()
	if err := g.Allow(); err != nil {
		t.Fatalf("reopened gate refused: %v", err)
	}
}

// 一台机器一个进程：第二个实例必须明确拒绝，而不是对同一个池静默地重复运行所有清理任务。
func TestAcquireProcessLockRefusesASecondHolder(t *testing.T) {
	path := t.TempDir() + "/ndiskless.lock"
	release, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireProcessLock(path); err == nil {
		t.Fatal("second acquire succeeded")
	} else if !strings.Contains(err.Error(), "已有") {
		t.Fatalf("err = %v", err)
	}
	release()
	release2, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}
