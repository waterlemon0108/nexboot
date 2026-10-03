package zfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func newTestClient(runner Runner) *Client {
	return New("tank", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 一轮复制先对整个目录容器打一个递归快照，让所有镜像、配置和库副本处于同一 txg。
func TestSnapshotRecursiveFreezesTheWholeContainer(t *testing.T) {
	runner := &fakeRunner{}
	client := newTestClient(runner)

	if err := client.SnapshotRecursive(context.Background(), "tank/nd", "rep-1"); err != nil {
		t.Fatal(err)
	}
	want := fakeCall{name: "zfs", args: []string{"snapshot", "-r", "tank/nd@rep-1"}}
	if !reflect.DeepEqual(runner.calls, []fakeCall{want}) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestSnapshotRecursiveRejectsMissingArguments(t *testing.T) {
	client := newTestClient(&fakeRunner{})
	if err := client.SnapshotRecursive(context.Background(), "", "rep-1"); err == nil {
		t.Fatal("expected error for empty root")
	}
	if err := client.SnapshotRecursive(context.Background(), "tank/nd", ""); err == nil {
		t.Fatal("expected error for empty snapshot")
	}
}

// 首次同步无基点，发完整复制流；之后在两个容器快照间增量并带全部中间快照（-I），不漏还原点。
func TestSendReplicationFullAndIncremental(t *testing.T) {
	runner := &pipeRunner{}
	client := newTestClient(runner)
	var sink bytes.Buffer

	if err := client.SendReplication(context.Background(), "tank/nd", "", "rep-1", &sink); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-R", "-w", "tank/nd@rep-1"}}) {
		t.Fatalf("full send calls = %#v", runner.streamCalls)
	}

	runner = &pipeRunner{}
	client = newTestClient(runner)
	if err := client.SendReplication(context.Background(), "tank/nd", "rep-1", "rep-2", &sink); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-R", "-w", "-I", "tank/nd@rep-1", "tank/nd@rep-2"}}) {
		t.Fatalf("incremental send calls = %#v", runner.streamCalls)
	}
}

func TestSendReplicationRejectsMissingArguments(t *testing.T) {
	client := newTestClient(&pipeRunner{})
	if err := client.SendReplication(context.Background(), "", "", "rep-1", io.Discard); err == nil {
		t.Fatal("expected error for empty root")
	}
	if err := client.SendReplication(context.Background(), "tank/nd", "", "", io.Discard); err == nil {
		t.Fatal("expected error for empty target snapshot")
	}
	if err := client.SendReplication(context.Background(), "tank/nd", "", "rep-1", nil); err == nil {
		t.Fatal("expected error for nil sink")
	}
}

// 接收中断会在目标上留下续传 token，下一次从它继续而不是从头发流。
func TestSendResumeContinuesFromToken(t *testing.T) {
	runner := &pipeRunner{}
	client := newTestClient(runner)

	if err := client.SendResume(context.Background(), "1-abc-def", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-t", "1-abc-def"}}) {
		t.Fatalf("resume send calls = %#v", runner.streamCalls)
	}
	if err := client.SendResume(context.Background(), "", io.Discard); err == nil {
		t.Fatal("expected error for empty token")
	}
}

// 接收端不能挂载收到的内容（-u：备机目录不在服务中），须跟随发送端回滚（-F），并能续传中断的流（-s）。
func TestReceiveReplicationArgv(t *testing.T) {
	runner := &pipeRunner{}
	client := newTestClient(runner)

	if err := client.ReceiveReplication(context.Background(), "tank/nd", strings.NewReader("stream")); err != nil {
		t.Fatal(err)
	}
	// -x mountpoint：mountpoint 是带发送端池名的绝对路径，收下会让副本落在别台池名的目录下；
	// 本机再改回则每轮复制又被写回，反复卸载重挂打断接收，复制停摆。见 ReceiveReplication。
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", "tank/nd"}}) {
		t.Fatalf("recv calls = %#v", runner.streamCalls)
	}
	if got := string(runner.streamData[0]); got != "stream" {
		t.Fatalf("recv stdin = %q", got)
	}
	if err := client.ReceiveReplication(context.Background(), "", strings.NewReader("")); err == nil {
		t.Fatal("expected error for empty root")
	}
}

// 没有可续传时 ZFS 报 "-"，调用方拿到空 token 而不是字面的短横。
func TestResumeTokenReadsProperty(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank/nd\t1-abc-def\n")}
	ds, token, err := newTestClient(runner).ResumeToken(context.Background(), "tank/nd")
	if err != nil || ds != "tank/nd" || token != "1-abc-def" {
		t.Fatalf("ds=%q token=%q err=%v", ds, token, err)
	}
	runner = &fakeRunner{out: []byte("tank/nd\t-\ntank/nd/a\t-\n")}
	if ds, token, err = newTestClient(runner).ResumeToken(context.Background(), "tank/nd"); err != nil || ds != "" || token != "" {
		t.Fatalf("没有令牌时应为空：ds=%q token=%q err=%v", ds, token, err)
	}
}

// 两节点按 GUID 而非名字比对持有内容：rename 和 promote 会改名字，不改 GUID。
func TestListGUIDsParsesDatasetsSnapshotsAndOrigins(t *testing.T) {
	out := strings.Join([]string{
		"tank/nd\t111\t-",
		"tank/nd@rep-1\t112\t-",
		"tank/nd/win11\t200\t-",
		"tank/nd/win11@0\t201\t-",
		"tank/nd/win11@rep-1\t202\t-",
		"tank/nd/win11_default\t300\ttank/nd/win11@0",
		"tank/nd/win11_default@0\t301\t-",
		"malformed line",
		"",
	}, "\n")
	runner := &fakeRunner{out: []byte(out)}
	client := newTestClient(runner)

	inv, err := client.ListGUIDs(context.Background(), "tank/nd")
	if err != nil {
		t.Fatal(err)
	}
	want := fakeCall{name: "zfs", args: []string{"list", "-H", "-p", "-r", "-t", "all", "-o", "name,guid,origin,creation", "tank/nd"}}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if len(inv.Entries) != 7 {
		t.Fatalf("entries = %d: %#v", len(inv.Entries), inv.Entries)
	}
	if got := inv.Entries[5]; got.Name != "tank/nd/win11_default" || got.GUID != "300" || got.Origin != "tank/nd/win11@0" {
		t.Fatalf("clone entry = %#v", got)
	}
	if got := inv.Entries[1]; got.Origin != "" {
		t.Fatalf("origin '-' should read as empty, got %q", got.Origin)
	}
	if !inv.HasSnapshot("tank/nd@rep-1") {
		t.Fatal("expected container snapshot to be present")
	}
	if inv.HasSnapshot("tank/nd@rep-9") {
		t.Fatal("unexpected snapshot")
	}
}

// 收发两端取都持有的最新容器快照作增量基点；按 GUID 比较，重建的同名快照不算共同基点。
func TestGUIDInventoryCommonSnapshot(t *testing.T) {
	sender := GUIDInventory{Entries: []GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "1"},
		{Name: "tank/nd@rep-2", GUID: "2"},
		{Name: "tank/nd@rep-3", GUID: "3"},
	}}
	receiver := GUIDInventory{Entries: []GUIDEntry{
		{Name: "tank/nd@rep-1", GUID: "1"},
		{Name: "tank/nd@rep-2", GUID: "99"}, // 同名重建
	}}
	if got := sender.LatestCommonSnapshot(receiver, "tank/nd", "tank/nd"); got != "rep-1" {
		t.Fatalf("common snapshot = %q, want rep-1", got)
	}
	empty := GUIDInventory{}
	if got := sender.LatestCommonSnapshot(empty, "tank/nd", "tank/nd"); got != "" {
		t.Fatalf("common snapshot with empty receiver = %q, want empty", got)
	}
}

// 本机备份比较数据池容器与备份池下的副本：快照名相同、根不同，按名字加 GUID 匹配。
func TestLatestCommonSnapshotAcrossRoots(t *testing.T) {
	sender := GUIDInventory{Entries: []GUIDEntry{
		{Name: "tank/nd@ndbackup-1", GUID: "1"},
		{Name: "tank/nd@ndbackup-2", GUID: "2"},
	}}
	receiver := GUIDInventory{Entries: []GUIDEntry{
		{Name: "backup/ndiskless/nd@ndbackup-1", GUID: "1"},
	}}
	if got := sender.LatestCommonSnapshot(receiver, "tank/nd", "backup/ndiskless/nd"); got != "ndbackup-1" {
		t.Fatalf("cross-root common = %q, want ndbackup-1", got)
	}
}

// 发送端的旧轮次标记只保留最新 K 个；只动复制标记，还原点永不清理。
func TestPruneSnapshotsKeepsNewestK(t *testing.T) {
	list := strings.Join([]string{
		"tank/nd@rep-1\t10",
		"tank/nd@rep-3\t30",
		"tank/nd@keepme\t35",
		"tank/nd@rep-2\t20",
		"tank/nd@rep-4\t40",
	}, "\n")
	runner := &sequenceRunner{outs: [][]byte{[]byte(list)}}
	client := newTestClient(runner)

	if err := client.PruneSnapshots(context.Background(), "tank/nd", "rep-", 2); err != nil {
		t.Fatal(err)
	}
	wantList := fakeCall{name: "zfs", args: []string{"list", "-H", "-p", "-d", "1", "-t", "snapshot", "-o", "name,createtxg", "tank/nd"}}
	if !reflect.DeepEqual(runner.calls[0], wantList) {
		t.Fatalf("list call = %#v", runner.calls[0])
	}
	var destroyed []string
	for _, call := range runner.calls[1:] {
		if len(call.args) < 3 || call.args[0] != "destroy" || call.args[1] != "-r" {
			t.Fatalf("unexpected call %#v", call)
		}
		destroyed = append(destroyed, call.args[2])
	}
	if !reflect.DeepEqual(destroyed, []string{"tank/nd@rep-1", "tank/nd@rep-2"}) {
		t.Fatalf("destroyed = %v", destroyed)
	}
}

func TestPruneSnapshotsRejectsNonPositiveKeep(t *testing.T) {
	client := newTestClient(&fakeRunner{})
	if err := client.PruneSnapshots(context.Background(), "tank/nd", "rep-", 0); err == nil {
		t.Fatal("expected error for keep <= 0")
	}
}

// Replicate 是本机备份用的同机 send|recv 管道；接收去掉 mountpoint，以免与在用数据集争同一目录。
func TestReplicatePipesSendIntoLocalReceive(t *testing.T) {
	runner := &pipeRunner{}
	client := newTestClient(runner)

	if err := client.Replicate(context.Background(), "tank/nd", "backup/ndiskless/nd", "", "ndbackup-1"); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-R", "-w", "tank/nd@ndbackup-1"}}) {
		t.Fatalf("send calls = %#v", runner.streamCalls)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", "backup/ndiskless/nd"}}) {
		t.Fatalf("recv calls = %#v", runner.streamCalls)
	}

	runner = &pipeRunner{}
	client = newTestClient(runner)
	if err := client.Replicate(context.Background(), "tank/nd", "backup/ndiskless/nd", "ndbackup-1", "ndbackup-2"); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"send", "-R", "-w", "-I", "tank/nd@ndbackup-1", "tank/nd@ndbackup-2"}}) {
		t.Fatalf("incremental send calls = %#v", runner.streamCalls)
	}
}

// 目录挪开后必须递归让出挂载点：zfs rename 不带走 nd/db 的显式挂载点，
// 否则旧的那份继续占着 /<池>/nd/db、盖住新收下的副本。
func TestDetachMountsReleasesTheSubtreeRecursively(t *testing.T) {
	runner := &sequenceRunner{}
	client := newTestClient(runner)
	if err := client.DetachMounts(context.Background(), "tank/nd-rebuilding-7"); err != nil {
		t.Fatal(err)
	}
	want := fakeCall{name: "zfs", args: []string{"inherit", "-r", "mountpoint", "tank/nd-rebuilding-7"}}
	if !containsCall(runner.calls, want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

// 改属性不等于卸载：mountpoint 已是 none 的容器 inherit 后不会被卸，陈旧挂载留在原处，
// 新库副本挂不上（EIO）。所以必须由深到浅显式卸载。
func TestDetachMountsActuallyUnmountsAndNotJustRepointsTheProperty(t *testing.T) {
	runner := &sequenceRunner{outs: [][]byte{
		[]byte("tank/nd-rebuilding-7\ntank/nd-rebuilding-7/db\ntank/nd-rebuilding-7/vmdk\n"),
	}}
	client := newTestClient(runner)
	if err := client.DetachMounts(context.Background(), "tank/nd-rebuilding-7"); err != nil {
		t.Fatal(err)
	}
	for _, ds := range []string{
		"tank/nd-rebuilding-7/db", "tank/nd-rebuilding-7/vmdk", "tank/nd-rebuilding-7",
	} {
		if !containsCall(runner.calls, fakeCall{name: "zfs", args: []string{"unmount", "-f", ds}}) {
			t.Fatalf("%s 没有被卸载。calls = %#v", ds, runner.calls)
		}
	}
	// 由深到浅：父级须排在子集之后，否则设备忙卸不掉。
	iOf := func(ds string) int {
		for i, c := range runner.calls {
			if len(c.args) == 3 && c.args[0] == "unmount" && c.args[2] == ds {
				return i
			}
		}
		return -1
	}
	if iOf("tank/nd-rebuilding-7") < iOf("tank/nd-rebuilding-7/db") {
		t.Fatalf("父级必须最后卸，calls = %#v", runner.calls)
	}
}

// 卸载失败不能中断：没挂载的数据集 `zfs unmount` 本就报错，当致命错误会让重建停在半路。
func TestDetachMountsToleratesDatasetsThatAreNotMounted(t *testing.T) {
	runner := &sequenceRunner{
		outs:  [][]byte{[]byte("tank/nd-rebuilding-7\n")},
		err:   errors.New("cannot unmount 'tank/nd-rebuilding-7': not currently mounted"),
		errAt: 2, // 第 1 次是 list，第 2 次是 unmount
	}
	client := newTestClient(runner)
	if err := client.DetachMounts(context.Background(), "tank/nd-rebuilding-7"); err != nil {
		t.Fatalf("没挂载不是错误：%v", err)
	}
	if !containsCall(runner.calls, fakeCall{
		name: "zfs", args: []string{"inherit", "-r", "mountpoint", "tank/nd-rebuilding-7"}}) {
		t.Fatalf("卸载失败之后仍要让出属性，calls = %#v", runner.calls)
	}
}

func containsCall(calls []fakeCall, want fakeCall) bool {
	for _, c := range calls {
		if reflect.DeepEqual(c, want) {
			return true
		}
	}
	return false
}

// 备机最后确认收到的标记不能删，否则备机回来时与写入者没有共同快照，只能整份重建目录。
func TestPruneSnapshotsKeepsPinnedMarkers(t *testing.T) {
	list := strings.Join([]string{
		"tank/nd@rep-1\t10",
		"tank/nd@rep-2\t20",
		"tank/nd@rep-3\t30",
		"tank/nd@rep-4\t40",
	}, "\n")
	runner := &sequenceRunner{outs: [][]byte{[]byte(list)}}
	if err := newTestClient(runner).PruneSnapshotsExcept(context.Background(), "tank/nd", "rep-", 1, []string{"rep-2"}); err != nil {
		t.Fatal(err)
	}
	var destroyed []string
	for _, call := range runner.calls[1:] {
		destroyed = append(destroyed, call.args[2])
	}
	if !reflect.DeepEqual(destroyed, []string{"tank/nd@rep-1", "tank/nd@rep-3"}) {
		t.Fatalf("destroyed = %v，rep-2 有备机钉着、rep-4 是最新的，都该留下", destroyed)
	}
}

// 逐数据集追赶的收发不带 -R：有共同快照从它增量，新配置以克隆源为起点（对端收成克隆），都没有就整份。
func TestSendDatasetShapes(t *testing.T) {
	for _, tc := range []struct {
		name               string
		from, origin, want string
	}{
		{"增量", "tank/nd/cfg@1", "", "send -w -I tank/nd/cfg@1 tank/nd/cfg@rep-2"},
		{"克隆", "", "tank/nd/img@0", "send -w -i tank/nd/img@0 tank/nd/cfg@rep-2"},
		{"整份", "", "", "send -w tank/nd/cfg@rep-2"},
	} {
		runner := &pipeRunner{}
		if err := newTestClient(runner).SendDataset(context.Background(), "tank/nd/cfg", tc.from, tc.origin, "rep-2", io.Discard); err != nil {
			t.Fatal(err)
		}
		if len(runner.streamCalls) != 1 || strings.Join(runner.streamCalls[0].args, " ") != tc.want {
			t.Fatalf("%s: calls = %#v", tc.name, runner.streamCalls)
		}
	}
}

// 逐数据集收流也须可续传（-s），否则大镜像中途被打断就得整份重传。
func TestReceiveDatasetArgv(t *testing.T) {
	runner := &pipeRunner{}
	if err := newTestClient(runner).ReceiveDataset(context.Background(), "data/nd/cfg", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"recv", "-F", "-u", "-s", "-x", "mountpoint", "data/nd/cfg"}}) {
		t.Fatalf("calls = %#v", runner.streamCalls)
	}
}

// 新镜像接收绝不能带 -F，否则会回滚并替换同名的已有数据集。
func TestReceiveStreamNeverForces(t *testing.T) {
	runner := &pipeRunner{}
	if err := newTestClient(runner).ReceiveStream(context.Background(), "data/nd/vmdk2", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if !runner.hasStreamCall(fakeCall{name: "zfs", args: []string{"recv", "-u", "-x", "mountpoint", "data/nd/vmdk2"}}) {
		t.Fatalf("calls = %#v", runner.streamCalls)
	}
}

// 递归收流中断时 token 在子数据集上：要递归查并指出是哪个数据集；根上有 token 时优先根。
func TestResumeTokenFindsAChildDataset(t *testing.T) {
	runner := &fakeRunner{out: []byte("tank/nd\t-\ntank/nd/image\t-\ntank/nd/windows-vmdk\t1-child\n")}
	client := newTestClient(runner)
	ds, token, err := client.ResumeToken(context.Background(), "tank/nd")
	if err != nil || ds != "tank/nd/windows-vmdk" || token != "1-child" {
		t.Fatalf("ds=%q token=%q err=%v", ds, token, err)
	}
	want := fakeCall{name: "zfs", args: []string{"get", "-H", "-r", "-t", "filesystem,volume", "-o", "name,value", "receive_resume_token", "tank/nd"}}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	runner = &fakeRunner{out: []byte("tank/nd\t1-root\ntank/nd/a\t1-child\n")}
	if ds, token, _ = newTestClient(runner).ResumeToken(context.Background(), "tank/nd"); ds != "tank/nd" || token != "1-root" {
		t.Fatalf("根上有令牌时应优先根：ds=%q token=%q", ds, token)
	}
}
