// A production Līdza app whose readiness depends on a real cache service.
package main

import (
	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/cache"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/mail"
	"net/http"
)

func main() {
	lidza.Run(lidza.App{Name: "cache-fixture", Packs: []lidza.Pack{db.Pack(), auth.Pack(), mail.Pack(), cache.Pack()}, Frontend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := cache.From(r.Context())
		if r.URL.Path == "/write" {
			if err := c.Set(r.Context(), "retained", "persisted", 0); err != nil {
				http.Error(w, "cache write failed", 503)
				return
			}
		}
		var value string
		if err := c.Get(r.Context(), "retained", &value); err != nil {
			http.Error(w, "cache read failed", 503)
			return
		}
		w.Write([]byte(value))
	})})
}
