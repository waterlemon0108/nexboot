package ha

import (
	"io"
	"time"
)

// 复制流与所有运行中客户机的 iSCSI 流量共用上行，不限速时一次全量同步就能占满网卡、
// 让整间教室卡住。日常增量只有还原点大小，感觉不到限速；它约束的是两类可预期的大流量：
// 新加入节点的首次全量同步和 promote 后的重建。

// NewRateLimitedWriter 以小片节奏把写入限制在约 mbPerSec MB/s（最简单的令牌桶）。零或负数表示不限，原样返回 w。
func NewRateLimitedWriter(w io.Writer, mbPerSec int) io.Writer {
	if mbPerSec <= 0 {
		return w
	}
	return &rateLimitedWriter{w: w, bytesPerSec: float64(mbPerSec) * 1024 * 1024, last: time.Now()}
}

type rateLimitedWriter struct {
	w           io.Writer
	bytesPerSec float64
	budget      float64 // 无需休眠还能发送的字节数
	last        time.Time
}

// maxSlice 让单次休眠保持很短，能尽快察觉取消（HTTP 连接关闭），节奏也更平滑。
const maxSlice = 256 * 1024

func (r *rateLimitedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		now := time.Now()
		r.budget += now.Sub(r.last).Seconds() * r.bytesPerSec
		r.last = now
		if cap := r.bytesPerSec / 4; r.budget > cap {
			r.budget = cap // 空闲的流不能攒出突发额度
		}
		if r.budget < 1 {
			time.Sleep(time.Duration((1 - r.budget) / r.bytesPerSec * float64(time.Second)))
			continue
		}
		n := len(p)
		if float64(n) > r.budget {
			n = int(r.budget)
		}
		if n > maxSlice {
			n = maxSlice
		}
		wrote, err := r.w.Write(p[:n])
		written += wrote
		r.budget -= float64(wrote)
		if err != nil {
			return written, err
		}
		p = p[wrote:]
	}
	return written, nil
}
