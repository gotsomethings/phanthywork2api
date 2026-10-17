package webui

// webui 内嵌 cli2api 前端构建产物（internal/webui/static），
// 使控制台外观与 cli2api 完全一致。

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// Handler 返回静态资源处理器。
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// IndexHTML 返回 SPA 入口页面。
func IndexHTML() ([]byte, error) {
	return fs.ReadFile(assets, "static/index.html")
}

// SPAFile 判断路径是否为内嵌静态资源。
func SPAFile(p string) bool {
	for _, prefix := range []string{"/assets/", "/favicon", "/apple-touch-icon", "/og-card", "/site.webmanifest"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
