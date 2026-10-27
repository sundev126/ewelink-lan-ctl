// Package webui serves the embedded browser control panel.
package webui

import (
	"embed"
	"net/http"
)

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

//go:embed static
var static embed.FS

// Handler serves the control panel at "/" and its assets under "/assets/".
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", file("static/index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /assets/app.css", file("static/app.css", "text/css; charset=utf-8"))
	mux.Handle("GET /assets/app.js", file("static/app.js", "text/javascript; charset=utf-8"))
	return mux
}

func file(name, contentType string) http.HandlerFunc {
	content, err := static.ReadFile(name)
	if err != nil {
		panic("webui: missing embedded file " + name)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		header := w.Header()
		header.Set("Content-Type", contentType)
		header.Set("Cache-Control", "no-cache")
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(content)
	}
}
