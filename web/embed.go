// Package web содержит статические файлы дашборда, встроенные в бинарник.
package web

import "embed"

// Static — файлы дашборда (корень — каталог static).
//
//go:embed static
var Static embed.FS
