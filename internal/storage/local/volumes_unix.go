//go:build unix

package local

import (
	"os"
	"syscall"
)

// physicalSize 返回文件实际占用的字节数；数据集开了压缩时比文件长度小。
func physicalSize(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}
