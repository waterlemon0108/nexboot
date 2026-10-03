package local

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
)

// 导出用临时 ndexport- 快照冻结镜像，整份裸流写入调用方，按试算大小报进度；成功失败都不留快照。
func TestExportImageStreamsAndCleansUpSnapshot(t *testing.T) {
	pool := newFakePool()
	pool.datasets = []string{"tank/nd/win11"}
	agent := New("server-a", pool)

	var buf bytes.Buffer
	var lastWritten, lastEstimated int64
	err := agent.ExportImage(context.Background(), storage.ExportImageReq{
		ImageID: "win11",
		W:       &buf,
		OnProgress: func(written, estimated int64) {
			lastWritten, lastEstimated = written, estimated
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "STREAM:tank/nd/win11@ndexport-") {
		t.Fatalf("stream = %q", buf.String())
	}
	if lastWritten != int64(buf.Len()) || lastEstimated <= 0 {
		t.Fatalf("progress = %d/%d, buffer %d", lastWritten, lastEstimated, buf.Len())
	}
	if len(pool.snaps) != 0 {
		t.Fatalf("snapshots left behind: %#v", pool.snaps)
	}
}

func TestExportImageCleansUpSnapshotOnSendFailure(t *testing.T) {
	pool := newFakePool()
	pool.datasets = []string{"tank/nd/win11"}
	pool.failOn = "send tank/nd/win11@ndexport-"
	agent := New("server-a", pool)

	err := agent.ExportImage(context.Background(), storage.ExportImageReq{ImageID: "win11", W: io.Discard})
	if err == nil {
		t.Fatal("send failure not reported")
	}
	if len(pool.snaps) != 0 {
		t.Fatalf("snapshots left behind: %#v", pool.snaps)
	}
}

func TestExportImageRefusesMissingImage(t *testing.T) {
	pool := newFakePool()
	agent := New("server-a", pool)
	if err := agent.ExportImage(context.Background(), storage.ExportImageReq{ImageID: "ghost", W: io.Discard}); err == nil {
		t.Fatal("missing image accepted")
	}
	if len(pool.snaps) != 0 {
		t.Fatalf("snapshots left behind: %#v", pool.snaps)
	}
}

func TestExportImageSizeReadsReferenced(t *testing.T) {
	pool := newFakePool()
	pool.datasets = []string{"tank/nd/win11"}
	agent := New("server-a", pool)
	size, err := agent.ExportImageSize(context.Background(), "win11")
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("size = %d", size)
	}
	if _, err := agent.ExportImageSize(context.Background(), "ghost"); err == nil {
		t.Fatal("missing image accepted")
	}
}

// 回导导出流时会带着 ndexport- 临时快照，导入须在建基线前删掉它，结果与全新导入一致；
// 手工制作的流里其它快照保留不动。
func TestImportImageStripsExportSnapshotFromStream(t *testing.T) {
	pool := newFakePool()
	pool.receiveSnapshot = "ndexport-1755600000000000000"
	agent := New("server-a", pool)
	root, sourcePath := importImageFixture(t, "win11.zfs")

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows"}); err != nil {
		t.Fatal(err)
	}
	if pool.snaps["tank/nd/win11@ndexport-1755600000000000000"] {
		t.Fatalf("export snapshot survived the import: %#v", pool.snaps)
	}
	if !pool.snaps["tank/nd/win11@0"] || !pool.snaps["tank/nd/win11_default@0"] {
		t.Fatalf("baseline missing: %#v", pool.snaps)
	}
}

func TestImportImageKeepsForeignStreamSnapshots(t *testing.T) {
	pool := newFakePool()
	pool.receiveSnapshot = "migrate"
	agent := New("server-a", pool)
	root, sourcePath := importImageFixture(t, "win11.zfs")

	if _, err := agent.ImportImage(context.Background(), storage.ImportImageReq{Name: "win11", SourcePath: sourcePath, ImportDir: root, OSType: "windows"}); err != nil {
		t.Fatal(err)
	}
	if !pool.snaps["tank/nd/win11@migrate"] {
		t.Fatalf("hand-made stream snapshot removed: %#v", pool.snaps)
	}
}

// 用真实 zfs 客户端校验导出生成的命令语法。
func TestExportImageCommandGrammar(t *testing.T) {
	runner := &fakeRunner{recordIO: true, sendSizeOutput: "size\t1024\n"}
	agent := New("server-a", zfs.New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := agent.ExportImage(context.Background(), storage.ExportImageReq{ImageID: "win11", W: io.Discard, OnProgress: func(int64, int64) {}}); err != nil {
		t.Fatal(err)
	}
	args := runner.args()
	if len(args) != 4 {
		t.Fatalf("args = %#v", args)
	}
	if args[0][0] != "snapshot" || !strings.HasPrefix(args[0][1], "tank/nd/win11@ndexport-") {
		t.Fatalf("snapshot call = %#v", args[0])
	}
	snap := args[0][1][strings.Index(args[0][1], "@")+1:]
	if !reflect.DeepEqual(args[1], []string{"send", "-nP", "-w", "tank/nd/win11@" + snap}) {
		t.Fatalf("estimate call = %#v", args[1])
	}
	if !reflect.DeepEqual(args[2], []string{"send", "-w", "tank/nd/win11@" + snap}) {
		t.Fatalf("send call = %#v", args[2])
	}
	if !reflect.DeepEqual(args[3], []string{"destroy", "tank/nd/win11@" + snap}) {
		t.Fatalf("cleanup call = %#v", args[3])
	}
}
