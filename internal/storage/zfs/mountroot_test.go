package zfs

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/storage"
)

func storageSpec(disks ...string) storage.PoolSpec {
	return storage.PoolSpec{Layout: domain.PoolLayoutStripe, GroupWidth: 1, Disks: disks}
}

// scriptRunner 按 argv 查表回答每条命令。
type scriptRunner struct {
	calls   []fakeCall
	answers map[string]string
}

func (r *scriptRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string{}, args...)})
	return []byte(r.answers[name+" "+strings.Join(args, " ")]), nil
}

// 池不再挂在 ZFS 默认的 /<池名>（会与系统目录抢名字，见 MountRoot），一律挂到 /ndiskless 下。
func TestPoolsMountUnderTheProductRoot(t *testing.T) {
	runner := &fakeRunner{}
	c := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = c.CreatePool(context.Background(), "tank2", storageSpec("/dev/sdb"))
	want := []string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-m", "/ndiskless/tank2", "tank2", "/dev/sdb"}
	if !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("create = %v", runner.calls[0].args)
	}
	if DBCopyDir("tank") != "/ndiskless/tank/nd/db" {
		t.Fatalf("DBCopyDir = %q", DBCopyDir("tank"))
	}
}

// 已有的池启动时迁移：库副本的显式挂载点会卡住卸载，所以先由深到浅卸、改完由浅到深挂回，再清旧位置的空目录。
func TestMigrateMountpointsMovesAPoolAtTheDefaultPath(t *testing.T) {
	runner := &scriptRunner{answers: map[string]string{
		"zpool list -H -o name":               "tank\n",
		"zfs get -H -o value mountpoint tank": "/tank\n",
		"zfs get -H -r -t filesystem -o name,property,value,source mountpoint,mounted tank": "" +
			"tank\tmountpoint\t/tank\tdefault\n" +
			"tank\tmounted\tyes\t-\n" +
			"tank/nd\tmountpoint\t/tank/nd\tinherited from tank\n" +
			"tank/nd\tmounted\tyes\t-\n" +
			"tank/nd/db\tmountpoint\t/tank/nd/db\tlocal\n" +
			"tank/nd/db\tmounted\tyes\t-\n" +
			"tank/run\tmountpoint\tnone\tlocal\n" +
			"tank/run\tmounted\tno\t-\n",
	}}
	c := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var cleaned []string
	c.removeEmptyDirs = func(dir string) { cleaned = append(cleaned, dir) }

	moved, err := c.MigrateMountpoints(context.Background())
	if err != nil || !reflect.DeepEqual(moved, []string{"tank"}) {
		t.Fatalf("moved = %v err = %v", moved, err)
	}
	var steps []string
	for _, call := range runner.calls {
		if call.name == "zfs" && (call.args[0] == "unmount" || call.args[0] == "set" || call.args[0] == "mount") {
			steps = append(steps, strings.Join(call.args, " "))
		}
	}
	want := []string{
		"unmount tank/nd/db", "unmount tank/nd", "unmount tank",
		"set mountpoint=/ndiskless/tank tank", "set mountpoint=/ndiskless/tank/nd/db tank/nd/db",
		"mount tank", "mount tank/nd", "mount tank/nd/db",
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps =\n%s\nwant\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if !reflect.DeepEqual(cleaned, []string{"/tank"}) {
		t.Fatalf("cleaned = %v", cleaned)
	}
}

// 已在 /ndiskless 下或本就不挂载的池不动。
func TestMigrateMountpointsLeavesOtherPoolsAlone(t *testing.T) {
	runner := &scriptRunner{answers: map[string]string{
		"zpool list -H -o name":               "tank\nbak\n",
		"zfs get -H -o value mountpoint tank": "/ndiskless/tank\n",
		"zfs get -H -o value mountpoint bak":  "none\n",
	}}
	c := New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.removeEmptyDirs = func(string) { t.Fatal("不该清任何目录") }
	moved, err := c.MigrateMountpoints(context.Background())
	if err != nil || len(moved) != 0 {
		t.Fatalf("moved = %v err = %v", moved, err)
	}
	for _, call := range runner.calls {
		if call.name == "zfs" && call.args[0] != "get" {
			t.Fatalf("不该改动: %v", call.args)
		}
	}
}

// 节点只上报挂在 /ndiskless/<池名> 下的产品池；ZFS 根池或运维另建的池报上去会被当成可建镜像、可销毁的池。
func TestProductPoolNamesAreThoseUnderTheProductRoot(t *testing.T) {
	runner := &scriptRunner{answers: map[string]string{
		"zpool list -H -o name":                  "data\nbackup\nrpool\nscratch\n",
		"zfs get -H -o value mountpoint data":    "/ndiskless/data\n",
		"zfs get -H -o value mountpoint backup":  "/ndiskless/backup\n",
		"zfs get -H -o value mountpoint rpool":   "/\n",
		"zfs get -H -o value mountpoint scratch": "/scratch\n",
	}}
	c := New("data", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := c.ProductPoolNames(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"data", "backup"}) {
		t.Fatalf("got %v err %v", got, err)
	}
}
