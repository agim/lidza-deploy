package web

import (
	"embed"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza/pkg/devserver"
	"io/fs"
	"net/http"
)

//go:embed static/*
var assets embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return staticHandler(sub, buildinfo.Version)
}

func staticHandler(sub fs.FS, version string) http.Handler {
	files := devserver.Static(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Lidza-Deploy-Version", version)
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.Redirect(w, r, "/console.html", http.StatusSeeOther)
			return
		}
		files.ServeHTTP(w, r)
	})
}
