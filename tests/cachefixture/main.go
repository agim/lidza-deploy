// A production Līdza app whose readiness depends on a real cache service.
package main

import (
	"context"
	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/cache"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/packs/storage"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "storage-task" {
		s, err := storage.New(storage.Config{Provider: os.Getenv("STORAGE_PROVIDER"), Dir: os.Getenv("STORAGE_DIR")})
		if err != nil {
			panic(err)
		}
		r, _, err := s.Get(context.Background(), "retained.txt")
		if err != nil {
			panic(err)
		}
		defer r.Close()
		io.Copy(os.Stdout, r)
		return
	}
	lidza.Run(lidza.App{Name: "cache-fixture", Packs: []lidza.Pack{db.Pack(), auth.Pack(), mail.Pack(), cache.Pack(), storage.Pack()}, Frontend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/storage-") {
			s := storage.From(r.Context())
			if r.URL.Path == "/storage-write" {
				if _, err := s.Put(r.Context(), "retained.txt", strings.NewReader("persisted"), storage.PutOptions{}); err != nil {
					http.Error(w, "storage write failed", 503)
					return
				}
			}
			reader, _, err := s.Get(r.Context(), "retained.txt")
			if err != nil {
				http.Error(w, "storage read failed", 503)
				return
			}
			defer reader.Close()
			io.Copy(w, reader)
			return
		}
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
