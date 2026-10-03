//go:build unix

package assets

import (
	"os"
	"syscall"
)

// sourceDataSize 返回源文件实际占用的字节数：取文件长度与已分配块中较小者，
// 稀疏 raw 镜像按实际数据计。读不到文件时返回 false。
func sourceDataSize(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	size := info.Size()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if allocated := int64(st.Blocks) * 512; allocated < size {
			size = allocated
		}
	}
	return size, true
}
