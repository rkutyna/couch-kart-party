package main

import (
	"embed"
	"io/fs"
	"os"
)

//go:embed web
var embeddedWeb embed.FS

// webFS returns the static client files: from disk when dir is set (handy while
// editing the client), otherwise the copy embedded in the binary.
func webFS(dir string) (fs.FS, error) {
	if dir != "" {
		return os.DirFS(dir), nil
	}
	return fs.Sub(embeddedWeb, "web")
}
