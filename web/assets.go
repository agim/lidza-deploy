package web

import (
	"embed"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza/pkg/devserver"
	"io/fs"
	"net/http"
	"strings"
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

// ConsoleBase is the path every control panel view lives under.
const ConsoleBase = "/console"

// ConsoleHome is the panel's landing view.
const ConsoleHome = ConsoleBase + "/applications"

// IsConsolePath reports whether path is a control panel view (served by console.html).
func IsConsolePath(path string) bool {
	return path == ConsoleBase || strings.HasPrefix(path, ConsoleBase+"/")
}

func staticHandler(sub fs.FS, version string) http.Handler {
	files := devserver.Static(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Lidza-Deploy-Version", version)
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.Redirect(w, r, ConsoleHome, http.StatusSeeOther)
			return
		}
		if IsConsolePath(r.URL.Path) {
			// Every panel view is a client-side route of the one console page,
			// so deep links and reloads load it with the page's own headers.
			page := r.Clone(r.Context())
			page.URL.Path, page.URL.RawPath = "/console.html", ""
			files.ServeHTTP(w, page)
			return
		}
		files.ServeHTTP(w, r)
	})
}
