package assets

import "syscall"

// statfsFree 返回 dir 所在文件系统的剩余字节数；导出到目录受它限制，而不是池空间。
func statfsFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
