package proof

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

func UI() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ui/") {
			copy := r.Clone(r.Context())
			copy.URL.Path = strings.TrimPrefix(r.URL.Path, "/ui")
			files.ServeHTTP(w, copy)
			return
		}
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/checks/") {
			http.NotFound(w, r)
			return
		}
		b, e := fs.ReadFile(sub, "index.html")
		if e != nil {
			http.Error(w, "UI unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
}
