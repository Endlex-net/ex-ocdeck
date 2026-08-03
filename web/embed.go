// Package web 通过 go:embed 将前端构建产物（web/dist）内嵌进二进制（design.md §14）。
//
// 构建顺序约束：go:embed 在编译期固化 web/dist 内容，因此必须先执行前端构建
// （`pnpm --dir web build`，产物输出到 web/dist）再执行 `go build`；
// web/dist 在 .gitignore 中（构建期生成，不入库），缺失 dist 时 go build 会编译失败。
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var embedded embed.FS

// DistFS 返回内嵌前端产物的 fs.FS，根映射到 dist 目录（路径不带 "dist/" 前缀）。
// 调用方据此 http.FileServer 同源服务静态资源（design.md §14：根路由 + SPA fallback）。
func DistFS() (fs.FS, error) {
	return fs.Sub(embedded, "dist")
}