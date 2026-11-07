package control

import (
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// A control panel that is not the active one serves only a notice: the
// standby copy a move installed on its destination, and the old panel
// once it handed over. Neither boots the application, so neither runs
// ticks, jobs or updates, nor calls an agent. /readyz answers so the
// upgrade helper's health check passes on both hosts.

const standbyPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%TITLE%</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0f0c;color:#d7e0d9;font:16px/1.5 system-ui,sans-serif}main{max-width:34rem;padding:2rem}h1{font-size:1.4rem;color:#7ee2a8;margin:0 0 .75rem}p{margin:.5rem 0;color:#aab6ad}</style></head>
<body><main><h1>%TITLE%</h1>%BODY%</main></body></html>`

func noticeHandler(title, body string) http.Handler {
	page := []byte(strings.NewReplacer("%TITLE%", html.EscapeString(title), "%BODY%", body).Replace(standbyPage))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/readyz" || r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("ok\n"))
			return
		}
		w.Header().Set("Retry-After", "120")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			agent.Fail(w, http.StatusServiceUnavailable, panelMovingError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(page)
	})
}

// StandbyHandler is the page of a standby control panel.
func StandbyHandler() http.Handler {
	return noticeHandler("This control panel is standing by", "<p>A copy of the control panel is being moved to this server. It takes over when the move's cutover runs on the current panel.</p><p>Until then, use the current control panel.</p>")
}

// MovedHandler is the page of a panel that handed over; movedTo is the
// panel's public URL (unchanged), now served by another server.
func MovedHandler(movedTo string) http.Handler {
	return noticeHandler("This control panel has moved", "<p>The control panel now runs on another server at the same address, <strong>"+html.EscapeString(movedTo)+"</strong>.</p><p>Your DNS resolver still points to the old server. Try again in a few minutes, or flush your DNS cache.</p>")
}

// MovedTo reads the moved marker of a data directory; ok is false when the
// panel did not move away.
func MovedTo(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, agent.ControlMovedFile))
	if err != nil {
		return "", false
	}
	var marker struct {
		MovedTo string `json:"moved_to"`
	}
	_ = json.Unmarshal(raw, &marker)
	return marker.MovedTo, true
}
