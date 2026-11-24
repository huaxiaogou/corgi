package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var rawDist embed.FS

// DistFS returns the compiled Vite frontend rooted at dist/.
func DistFS() fs.FS {
	sub, err := fs.Sub(rawDist, "dist")
	if err != nil {
		return rawDist
	}
	return sub
}
