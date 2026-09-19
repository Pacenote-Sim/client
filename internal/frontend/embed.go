// Package frontend is the window's one page, embedded so the exe is one file.
package frontend

import "embed"

// FS holds index.html. The Wails asset server serves it at /.
//
//go:embed index.html
var FS embed.FS
