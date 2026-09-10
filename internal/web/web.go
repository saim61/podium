// Package web serves the demo page from inside the binary.
package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the demo page and its assets.
//
// Embedded rather than mounted from disk, so the image is self-contained and the page can never
// be a different version than the API it talks to.
func Handler() (http.Handler, error) {
	static, err := fs.Sub(files, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	return http.FileServerFS(static), nil
}
