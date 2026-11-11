//go:build !unix

package local

import "os"

// physicalSize 在拿不到块数的平台上退回文件长度。
func physicalSize(info os.FileInfo) int64 { return info.Size() }
