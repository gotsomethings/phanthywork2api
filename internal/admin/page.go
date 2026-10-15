package admin

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed admin.html
var pageFS embed.FS

// page 托管控制面板页面（纯静态，不含密钥）。
func (a *Admin) page(w http.ResponseWriter, r *http.Request) {
	raw, err := fs.ReadFile(pageFS, "admin.html")
	if err != nil {
		http.Error(w, "admin page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}
