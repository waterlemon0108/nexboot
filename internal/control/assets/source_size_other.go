//go:build !unix

package assets

import "os"

func sourceDataSize(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	return info.Size(), true
}
