// Package web — статические файлы интерфейса, встроенные в бинарник.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
