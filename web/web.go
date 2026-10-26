// Package web embeds the board's templates and static files, and the multi-tenant hub's pages.
package web

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed templates static hub
var FS embed.FS

// Pages lists the page templates (everything but layout.html and brand.html).
func Pages() ([]string, error) {
	entries, err := fs.ReadDir(FS, "templates")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".html") && n != "layout.html" && n != "brand.html" {
			out = append(out, n)
		}
	}
	return out, nil
}
