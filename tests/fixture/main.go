// The smoke fixture exercises the real Līdza runtime inside release containers.
package main

import (
	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/db"
	"net/http"
	"os"
)

func main() {
	lidza.Run(lidza.App{Name: "deployment-smoke", Frontend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check-databases" {
			for _, key := range []string{"DATABASE_URL", "ANALYTICS_DATABASE_URL"} {
				pool, err := db.Open(r.Context(), db.Config{URL: os.Getenv(key)})
				if err != nil {
					http.Error(w, "database unavailable", 503)
					return
				}
				err = pool.Ping(r.Context())
				pool.Close()
				if err != nil {
					http.Error(w, "database unavailable", 503)
					return
				}
			}
			w.Write([]byte("ok"))
			return
		}
		w.Write([]byte(os.Getenv("RELEASE_TEXT")))
	})})
}
