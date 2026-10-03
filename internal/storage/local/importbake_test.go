package local

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
)

// newBakeAgent 把脚本注入与 ZFS 调用记进同一条有序日志，便于断言先后顺序。
func newBakeAgent(t *testing.T, injectErr error) (*Agent, *fakePool, *[]byte) {
	t.Helper()
	zfsFake := newFakePool()
	agent := New("server-a", zfsFake)
	var seen []byte
	agent.injectMountScriptFn = func(_ context.Context, dev string, script []byte) error {
		zfsFake.ops = append(zfsFake.ops, "inject "+dev)
		if injectErr != nil {
			return injectErr
		}
		seen = script
		return nil
	}
	return agent, zfsFake, &seen
}

func TestImportImageBakesScriptBeforeFirstSnapshot(t *testing.T) {
	// 下游都从 image@0 继承；若先打快照再写脚本，镜像标称已烘焙，客户机却都拿不到脚本。
	agent, zfsFake, seen := newBakeAgent(t, nil)
	root, sourcePath := importImageFixture(t, "win11.zfs")

	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows",
		MountScript: []byte("# startup script"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 脚本必须先于镜像的第一个快照写入。
	want := []string{
		"receive tank/nd/win11",
		"inject /dev/zvol/tank/nd/win11",
		"snapshot tank/nd/win11@0",
		"clone tank/nd/win11_default",
		"snapshot tank/nd/win11_default@0",
	}
	if !reflect.DeepEqual(zfsFake.ops, want) {
		t.Fatalf("ops = %#v, want %#v", zfsFake.ops, want)
	}
	if !result.MountScriptInjected {
		t.Fatal("MountScriptInjected = false; caller would not record the baked version")
	}
	if string(*seen) != "# startup script" {
		t.Fatalf("injected script = %q", *seen)
	}
}

func TestImportImageSurvivesBakeFailure(t *testing.T) {
	// 烘焙只是快捷路径，失败不能影响导入：调用方不记版本，开机退回逐台注入。
	agent, zfsFake, _ := newBakeAgent(t, errors.New("no windows partition found"))
	root, sourcePath := importImageFixture(t, "win11.zfs")

	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows",
		MountScript: []byte("# startup script"),
	})
	if err != nil {
		t.Fatalf("a failed bake aborted the whole import: %v", err)
	}
	if result.MountScriptInjected {
		t.Fatal("MountScriptInjected = true after the injection failed")
	}
	if result.Image.ID != "win11" || result.Config.ID != "win11_default" {
		t.Fatalf("import did not complete: %#v", result)
	}
	// 镜像仍完整建成，只是缺了快捷路径。
	want := []string{
		"receive tank/nd/win11",
		"inject /dev/zvol/tank/nd/win11",
		"snapshot tank/nd/win11@0",
		"clone tank/nd/win11_default",
		"snapshot tank/nd/win11_default@0",
	}
	if !reflect.DeepEqual(zfsFake.ops, want) {
		t.Fatalf("ops = %#v, want %#v", zfsFake.ops, want)
	}
}

func TestImportImageAdaptsLinuxBeforeFirstSnapshot(t *testing.T) {
	agent, zfsFake, _ := newBakeAgent(t, nil)
	var seen []byte
	agent.adaptLinuxFn = func(_ context.Context, dev string, script []byte) error {
		zfsFake.ops = append(zfsFake.ops, "adapt-linux "+dev)
		seen = script
		return nil
	}
	root, sourcePath := importImageFixture(t, "ubuntu.zfs")

	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "ubuntu", SourcePath: sourcePath, ImportDir: root, OSType: "linux",
		MountScript: []byte("#!/bin/sh"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"receive tank/nd/ubuntu",
		"adapt-linux /dev/zvol/tank/nd/ubuntu",
		"snapshot tank/nd/ubuntu@0",
		"clone tank/nd/ubuntu_default",
		"snapshot tank/nd/ubuntu_default@0",
	}
	if !reflect.DeepEqual(zfsFake.ops, want) {
		t.Fatalf("ops = %#v, want %#v", zfsFake.ops, want)
	}
	if !result.MountScriptInjected || string(seen) != "#!/bin/sh" {
		t.Fatalf("injected=%v script=%q", result.MountScriptInjected, seen)
	}
}

func TestImportImageSurvivesLinuxAdaptFailure(t *testing.T) {
	agent, _, _ := newBakeAgent(t, nil)
	agent.adaptLinuxFn = func(context.Context, string, []byte) error { return errNoGrubConfig }
	root, sourcePath := importImageFixture(t, "ubuntu.zfs")
	result, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "ubuntu", SourcePath: sourcePath, ImportDir: root, OSType: "linux", MountScript: []byte("#!/bin/sh"),
	})
	if err != nil || result.MountScriptInjected || result.Image.ID != "ubuntu" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestImportImageNamesItsTargetBeforeTouchingThePool(t *testing.T) {
	agent, zfsFake, _ := newBakeAgent(t, nil)
	root, sourcePath := importImageFixture(t, "win11.zfs")
	var target string
	var opsBefore int
	_, err := agent.ImportImage(context.Background(), storage.ImportImageReq{
		Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows",
		OnTarget: func(id string) { target, opsBefore = id, len(zfsFake.ops) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if target != "win11" || opsBefore != 0 {
		t.Fatalf("target = %q after %d zfs ops", target, opsBefore)
	}
}
