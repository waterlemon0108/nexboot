package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

func Dist() fs.FS {
	out, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return out
}
