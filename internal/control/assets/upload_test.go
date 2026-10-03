package assets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
)

func newUploads(t *testing.T, node string) (*UploadService, string) {
	t.Helper()
	dir := t.TempDir()
	return &UploadService{
		ImportDir: func(context.Context) (string, error) { return dir, nil },
		NodeID:    node,
		FreeSpace: func(string) (int64, error) { return 1 << 40, nil },
	}, dir
}

// 上传中断后，会话记得已收字节，浏览器可从断点续传，最终文件内容完整一致。
func TestUploadResumesFromWhereItStopped(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")
	body := []byte("0123456789abcdefghij")

	s, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs", SizeBytes: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if s.Received != 0 || s.UploadID == "" {
		t.Fatalf("新会话 = %+v", s)
	}

	// 传前 8 字节，然后“断线”
	got, err := svc.Append(ctx, s.UploadID, 0, strings.NewReader(string(body[:8])))
	if err != nil || got.Received != 8 {
		t.Fatalf("第一段：%+v %v", got, err)
	}

	// 浏览器回来先问「你收到哪了」
	st, err := svc.Status(ctx, s.UploadID)
	if err != nil || st.Received != 8 {
		t.Fatalf("续传前查询：%+v %v", st, err)
	}

	// 从断点续传剩下的
	got, err = svc.Append(ctx, s.UploadID, 8, strings.NewReader(string(body[8:])))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.Received != int64(len(body)) {
		t.Fatalf("补完之后：%+v", got)
	}

	// 完整的文件落到导入目录里，内容一字不差
	final := filepath.Join(dir, "win11.zfs")
	b, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("成品不在导入目录：%v", err)
	}
	if string(b) != string(body) {
		t.Fatalf("内容对不上：%q", string(b))
	}
	if got.Path != final {
		t.Fatalf("返回的路径 %q 与实际 %q 不符", got.Path, final)
	}
}

// 偏移越过已收位置时拒绝并告知正确起点，不能把分片拼错位置留下空洞。
func TestUploadRejectsAMisalignedChunkAndSaysWhereItIs(t *testing.T) {
	ctx := context.Background()
	svc, _ := newUploads(t, "nodeA")
	s, _ := svc.Begin(ctx, BeginUploadRequest{FileName: "a.zfs", SizeBytes: 100})
	if _, err := svc.Append(ctx, s.UploadID, 0, strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Append(ctx, s.UploadID, 50, strings.NewReader("xxxx"))
	if err == nil {
		t.Fatal("偏移跳空必须拒绝，否则文件中间是个洞")
	}
	if !strings.Contains(err.Error(), "10") {
		t.Fatalf("错误里要带上当前已收字节数，浏览器才知道从哪续：%v", err)
	}
	// 重复发同一段（网络重试）不该把文件写长
	if _, err := svc.Append(ctx, s.UploadID, 0, strings.NewReader("0123456789")); err != nil {
		t.Fatalf("重发已收过的分片应当被容忍：%v", err)
	}
	st, _ := svc.Status(ctx, s.UploadID)
	if st.Received != 10 {
		t.Fatalf("重发之后 received = %d，应仍是 10", st.Received)
	}
}

// 主备切换后会话留在旧主上，新主应明确说明主机已切换，而不是回含糊的 404。
func TestUploadOnAnotherNodeSaysTheActiveMoved(t *testing.T) {
	ctx := context.Background()
	svc, _ := newUploads(t, "nodeA")
	s, _ := svc.Begin(ctx, BeginUploadRequest{FileName: "a.zfs", SizeBytes: 10})

	moved, _ := newUploads(t, "nodeB") // 切换后由 nodeB 接手
	_, err := moved.Append(ctx, s.UploadID, 0, strings.NewReader("0123456789"))
	if err == nil {
		t.Fatal("会话属于另一台，必须拒绝")
	}
	if !strings.Contains(err.Error(), "主机") {
		t.Fatalf("要说清是主机换了，而不是「会话不存在」：%v", err)
	}
	if _, err := moved.Status(ctx, s.UploadID); err == nil {
		t.Fatal("查询同样要拒绝")
	}
}

// 建会话时就拒绝不支持的后缀、路径穿越和空间不足，不拖到传完才拒。
func TestUploadValidatesNameAndTypeUpFront(t *testing.T) {
	ctx := context.Background()
	svc, _ := newUploads(t, "nodeA")
	for _, c := range []struct{ name, file string }{
		{"不支持的后缀", "notes.txt"},
		{"没有后缀", "win11"},
		{"路径穿越", "../../etc/passwd.zfs"},
		{"空名字", ""},
	} {
		if _, err := svc.Begin(ctx, BeginUploadRequest{FileName: c.file, SizeBytes: 10}); err == nil {
			t.Fatalf("%s（%q）应当在建会话时就被拒", c.name, c.file)
		}
	}
	// 空间不够也要当场说，而不是传到一半磁盘满
	tight := &UploadService{
		ImportDir: func(context.Context) (string, error) { return t.TempDir(), nil },
		NodeID:    "nodeA",
		FreeSpace: func(string) (int64, error) { return 1 << 20, nil },
	}
	if _, err := tight.Begin(ctx, BeginUploadRequest{FileName: "big.zfs", SizeBytes: 1 << 30}); err == nil {
		t.Fatal("空间不够应当在建会话时就拒绝")
	}
}

// 刷新页面或服务重启后重新选同一个文件，Begin 应认领盘上的断点而不是新开会话。
func TestBeginResumesAnUnfinishedUploadOfTheSameFile(t *testing.T) {
	ctx := context.Background()
	svc, _ := newUploads(t, "nodeA")

	first, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000, LastModified: 1724400000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(ctx, first.UploadID, 0, strings.NewReader(strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}

	// 页面刷新了，重新选同一个文件
	again, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000, LastModified: 1724400000})
	if err != nil {
		t.Fatal(err)
	}
	if again.UploadID != first.UploadID {
		t.Fatalf("又开了一个新会话：%q（原来是 %q）", again.UploadID, first.UploadID)
	}
	if again.Received != 400 {
		t.Fatalf("received = %d，想要 400——断点没被认出来", again.Received)
	}
}

// 同名同大小但修改时间不同时不能续传，否则拼出长度正确、内容错乱的文件。
func TestBeginDoesNotResumeADifferentFileWithTheSameName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newUploads(t, "nodeA")

	first, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000, LastModified: 1724400000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(ctx, first.UploadID, 0, strings.NewReader(strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000, LastModified: 1724499999})
	if err != nil {
		t.Fatal(err)
	}
	if again.UploadID == first.UploadID || again.Received != 0 {
		t.Fatalf("接到了别人的半截文件上：id=%q received=%d", again.UploadID, again.Received)
	}
}

// 旧版本分片没有记修改时间，新前端报上真实时间时应只按名字和大小认领断点。
func TestBeginResumesLegacyPartWhoseIdentityWasNeverRecorded(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")

	// 老版本留下的：元数据里没有 last_modified
	first, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(ctx, first.UploadID, 0, strings.NewReader(strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(dir, ".uploads", first.UploadID+".json"),
		map[string]any{"file_name": "win11.zfs.gz", "size_bytes": 1000, "node_id": "nodeA"}); err != nil {
		t.Fatal(err)
	}

	// 新前端选同一个文件，报上了真实的改动时间
	again, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 1000, LastModified: 1724400000})
	if err != nil {
		t.Fatal(err)
	}
	if again.UploadID != first.UploadID || again.Received != 400 {
		t.Fatalf("老分片续不上：id=%q received=%d", again.UploadID, again.Received)
	}
}

// 续传的空间预检按「还差多少」算：已落盘部分占着导入目录，按全长比会误拒；
// 真不够时仍要拒，全新上传照旧按全长判。
func TestBeginCountsWhatIsAlreadyUploadedAgainstFreeSpace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	free := int64(10000) // 开局空间充足
	svc := &UploadService{
		ImportDir: func(context.Context) (string, error) { return dir, nil },
		NodeID:    "nodeA",
		FreeSpace: func(string) (int64, error) { return free, nil },
	}

	// 传一半：8000 的文件先落 6000（此时剩余空间也跟着变小）
	first, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 8000, LastModified: 42})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(ctx, first.UploadID, 0, strings.NewReader(strings.Repeat("x", 6000))); err != nil {
		t.Fatal(err)
	}

	// 现在只剩 2500 可用，而文件全长 8000 —— 但还差的只有 2000，够。
	free = 2500
	again, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 8000, LastModified: 42})
	if err != nil {
		t.Fatalf("还差 2000、可用 2500，不该被判放不下：%v", err)
	}
	if again.UploadID != first.UploadID || again.Received != 6000 {
		t.Fatalf("续传没接上：id=%q received=%d", again.UploadID, again.Received)
	}

	// 真的不够时仍要当场拒绝：还差 2000，只剩 1500。
	free = 1500
	if _, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs.gz", SizeBytes: 8000, LastModified: 42}); err == nil {
		t.Fatal("剩余空间连缺口都不够时应当拒绝")
	}

	// 全新上传（没有断点）照旧按全长判。
	free = 1000
	if _, err := svc.Begin(ctx, BeginUploadRequest{FileName: "other.zfs.gz", SizeBytes: 8000, LastModified: 7}); err == nil {
		t.Fatal("全新上传空间不够应当拒绝")
	}
}

// 导入目录里已有同名文件（可能正以「导入后删除源文件」在导入）时，开始上传就要点名拒绝，不能传完再静默覆盖它。
func TestBeginRefusesWhenTheImportDirAlreadyHasThatFile(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")
	existing := filepath.Join(dir, "win11.zfs")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs", SizeBytes: 3})
	if !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "win11.zfs") {
		t.Fatalf("同名文件已存在时应点名拒绝，得到 %v", err)
	}
}

// 上传期间才出现的同名文件也不能被覆盖：成品改名避让，并返回实际落点。
func TestFinishNeverOverwritesAFileThatAppearedDuringUpload(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")
	s, err := svc.Begin(ctx, BeginUploadRequest{FileName: "win11.zfs", SizeBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "win11.zfs")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Append(ctx, s.UploadID, 0, strings.NewReader("new"))
	if err != nil || !got.Complete {
		t.Fatalf("append: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "old" {
		t.Fatalf("原有文件被覆盖：%q", b)
	}
	if got.Path == existing || filepath.Dir(got.Path) != dir || !strings.HasSuffix(got.Path, ".zfs") {
		t.Fatalf("成品落点 %q 不对", got.Path)
	}
	if b, _ := os.ReadFile(got.Path); string(b) != "new" {
		t.Fatalf("成品内容 %q", b)
	}
}

// 过期按最后一次写入算：开始于 30 小时前、仍在续传的大文件不能被清掉；真正停了 24 小时以上的才清。
func TestReapGoesByTheLastWriteNotTheStart(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")
	now := time.Now()
	svc.Now = func() time.Time { return now.Add(-30 * time.Hour) }
	s, err := svc.Begin(ctx, BeginUploadRequest{FileName: "big.zfs", SizeBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	svc.Now = func() time.Time { return now }
	if _, err := svc.Append(ctx, s.UploadID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if n := svc.Reap(ctx, 24*time.Hour); n != 0 {
		t.Fatalf("刚写过的会话被清掉了 %d 个", n)
	}
	part := filepath.Join(dir, uploadsSubdir, s.UploadID+".part")
	old := now.Add(-25 * time.Hour)
	if err := os.Chtimes(part, old, old); err != nil {
		t.Fatal(err)
	}
	if n := svc.Reap(ctx, 24*time.Hour); n != 1 {
		t.Fatalf("停了 25 小时的会话应被清掉，清了 %d 个", n)
	}
}

// 正在写入的会话（持有会话锁）不能被清，否则写到一半分片没了。
func TestReapSkipsASessionBeingWritten(t *testing.T) {
	ctx := context.Background()
	svc, dir := newUploads(t, "nodeA")
	old := time.Now().Add(-48 * time.Hour)
	svc.Now = func() time.Time { return old }
	s, err := svc.Begin(ctx, BeginUploadRequest{FileName: "big.zfs", SizeBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	svc.Now = nil
	part := filepath.Join(dir, uploadsSubdir, s.UploadID+".part")
	if err := os.Chtimes(part, old, old); err != nil {
		t.Fatal(err)
	}
	unlock := svc.lock(s.UploadID)
	n := svc.Reap(ctx, 24*time.Hour)
	unlock()
	if n != 0 {
		t.Fatalf("写入中的会话被清掉了")
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("分片没了：%v", err)
	}
}
