package web

import (
	"bytes"
	"embed"
	"net/http"
	"time"
)

//go:embed assets/app.css assets/htmx.min.js
var assetFS embed.FS

var assetModified = time.Now()

func serveAsset(w http.ResponseWriter, r *http.Request, route string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := assetFS.ReadFile(route)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch route {
	case "assets/app.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case "assets/htmx.min.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, route, assetModified, bytes.NewReader(data))
}
