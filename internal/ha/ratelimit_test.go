package ha

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// 复制流与客户机 iSCSI 流量共用上行，不限速时全量同步会把客户机饿到卡住；限速把大流量控制在配置的份额内。
func TestRateLimitedWriterPacesLargeWrites(t *testing.T) {
	var sink bytes.Buffer
	// 1 MB/s：写 300KB 应约 0.3 秒，绝不能瞬间完成。
	w := NewRateLimitedWriter(&sink, 1)
	start := time.Now()
	n, err := io.Copy(w, bytes.NewReader(make([]byte, 300*1024)))
	if err != nil || n != 300*1024 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("300KB at 1MB/s finished in %v — not throttled", elapsed)
	}
	if sink.Len() != 300*1024 {
		t.Fatalf("sink=%d", sink.Len())
	}
}

// 零表示不限：原样返回 writer，选择不限速的站点热路径上没有节奏计算。
func TestRateLimitedWriterZeroIsUnlimited(t *testing.T) {
	var sink bytes.Buffer
	w := NewRateLimitedWriter(&sink, 0)
	if w != io.Writer(&sink) {
		t.Fatal("zero limit must return the writer unchanged")
	}
}
