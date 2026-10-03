package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCleaner 记录静默过程中每一步的顺序，因为顺序就是这里的安全性质，见下面的测试。
type fakeCleaner struct {
	calls      []string
	clientMACs []string
	superMACs  []string
	superErr   error
}

func (f *fakeCleaner) ClientCloneMACs(context.Context) ([]string, error) {
	f.calls = append(f.calls, "list-client")
	return f.clientMACs, nil
}

func (f *fakeCleaner) CleanupClientClones(_ context.Context, mac string, _ []string) error {
	f.calls = append(f.calls, "clean-client:"+mac)
	return nil
}

func (f *fakeCleaner) SuperCloneMACs(context.Context) ([]string, error) {
	f.calls = append(f.calls, "list-super")
	return f.superMACs, f.superErr
}

func (f *fakeCleaner) CleanupSuperClones(_ context.Context, mac string) error {
	f.calls = append(f.calls, "clean-super:"+mac)
	return nil
}

func (f *fakeCleaner) index(want string) int {
	for i, c := range f.calls {
		if c == want {
			return i
		}
	}
	return -1
}

// 降级为备机时超管克隆也必须清掉。超管机被放在写入者上（place.go），切换后旧节点留下的克隆没有主人：
// 备机静默、离线回收器都跳过它，run/ 也不复制。超管开机又会复用同名克隆，盘内容会随写入者在两个分叉间跳，
// 此时保存会把旧内容提升为还原点并覆盖全集群更新的那份。
func TestStandbyQuiesceAlsoClearsSuperClones(t *testing.T) {
	c := &fakeCleaner{clientMACs: []string{"AA"}, superMACs: []string{"BB", "CC"}}
	var tornDown bool
	teardown := func(context.Context) error {
		c.calls = append(c.calls, "teardown")
		tornDown = true
		return nil
	}

	quiesceStandby(context.Background(), teardown, c, func(string, ...any) {})

	if !tornDown {
		t.Fatal("导出没有被拆除")
	}
	for _, mac := range []string{"BB", "CC"} {
		if c.index("clean-super:"+mac) < 0 {
			t.Fatalf("超管克隆 %s 没有被清理：%v", mac, c.calls)
		}
	}
	if c.index("clean-client:AA") < 0 {
		t.Fatalf("普通客户机克隆仍然要清：%v", c.calls)
	}
}

// 顺序就是安全性：先拆导出再销毁，拆完后本机不再供任何盘，结构上不可能销毁正被客户机使用的盘，
// 不必依赖「有没有活会话」的判断。
func TestStandbyQuiesceTearsDownExportsBeforeDestroyingAnything(t *testing.T) {
	c := &fakeCleaner{clientMACs: []string{"AA"}, superMACs: []string{"BB"}}
	teardown := func(context.Context) error {
		c.calls = append(c.calls, "teardown")
		return nil
	}

	quiesceStandby(context.Background(), teardown, c, func(string, ...any) {})

	td := c.index("teardown")
	for _, call := range c.calls {
		if !strings.HasPrefix(call, "clean-") {
			continue
		}
		if c.index(call) < td {
			t.Fatalf("%s 发生在拆除导出之前——可能销毁正在使用的盘：%v", call, c.calls)
		}
	}
}

// 一类清不掉不该拖累另一类：静默是尽力而为，剩下的下次降级再清。
func TestStandbyQuiesceKeepsGoingWhenSuperListingFails(t *testing.T) {
	c := &fakeCleaner{clientMACs: []string{"AA"}, superErr: errors.New("pool busy")}
	quiesceStandby(context.Background(), func(context.Context) error { return nil }, c,
		func(string, ...any) {})
	if c.index("clean-client:AA") < 0 {
		t.Fatalf("超管那一路失败不该让普通克隆也不清：%v", c.calls)
	}
}
