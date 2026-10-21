// Platform-only acceptance fixture. Thura's application tests live in its repo.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func main() {
	role := os.Getenv("FIXTURE_ROLE")
	port := "3000"
	if role == "mail" {
		port = "12000"
	}
	if role == "office" {
		port = "9980"
	}
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		client := &http.Client{Timeout: time.Second}
		r, e := client.Get("http://127.0.0.1:" + port + "/health")
		if e != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		r.Body.Close()
		return
	}
	if role == "mail" {
		_ = os.MkdirAll("/var/lib/fixture", 0700)
		f, e := os.OpenFile("/var/lib/fixture/starts", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if e != nil {
			panic(e)
		}
		fmt.Fprintln(f, "start")
		f.Close()
	}
	var pool *pgxpool.Pool
	if role == "app" || role == "matrix" {
		var e error
		dsn := os.Getenv("DATABASE_URL")
		if role == "matrix" {
			dsn = os.Getenv("MATRIX_DATABASE_URL")
		}
		pool, e = pgxpool.New(context.Background(), dsn)
		if e != nil {
			panic(e)
		}
		defer pool.Close()
	}
	var selectionMu sync.Mutex
	type requiredState struct {
		Active, Desired string
		Revision        int
	}
	statePath := filepath.Join(os.Getenv("STORAGE_DIR"), "runtime.json")
	loadState := func() (requiredState, error) {
		value := requiredState{Active: os.Getenv("AI_RUNTIME"), Desired: os.Getenv("AI_RUNTIME")}
		if value.Active == "" {
			value.Active = "remote"
			value.Desired = "remote"
		}
		data, err := os.ReadFile(statePath)
		if os.IsNotExist(err) {
			return value, nil
		}
		if err != nil {
			return value, err
		}
		err = json.Unmarshal(data, &value)
		return value, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if role == "app" && (r.URL.Path == "/.well-known/deploy-runtime" || r.URL.Path == "/fixture/runtime") {
			selectionMu.Lock()
			defer selectionMu.Unlock()
			state, err := loadState()
			if err != nil {
				http.Error(w, "runtime state unavailable", 500)
				return
			}
			if r.URL.Path == "/.well-known/deploy-runtime" {
				if r.Method != "GET" || os.Getenv("DEPLOY_RUNTIME_TOKEN") == "" || r.Header.Get("Authorization") != "Bearer "+os.Getenv("DEPLOY_RUNTIME_TOKEN") {
					http.Error(w, "forbidden", 403)
					return
				}
				required := "remote"
				if state.Active == "local" || state.Desired == "local" {
					required = "local"
				}
				json.NewEncoder(w).Encode(map[string]any{"version": 1, "revision": fmt.Sprintf("r%d", state.Revision), "values": map[string]string{"AI_RUNTIME": required}})
				return
			}
			if r.Method != "POST" {
				http.Error(w, "method", 405)
				return
			}
			if mode := r.URL.Query().Get("desired"); mode == "local" || mode == "remote" {
				state.Desired = mode
			}
			if r.URL.Query().Get("publish") == "true" {
				state.Active = state.Desired
			}
			state.Revision++
			data, _ := json.Marshal(state)
			if err := os.WriteFile(statePath, data, 0600); err != nil {
				http.Error(w, "save failed", 500)
				return
			}
			fmt.Fprint(w, "saved")
			return
		}
		if role == "matrix" {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				http.Error(w, "matrix database unavailable", 503)
				return
			}
		}

		if role == "app" {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if pool.Ping(ctx) != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			c, e := net.DialTimeout("tcp", "127.0.0.1:12000", time.Second)
			if e != nil {
				http.Error(w, "mail unavailable", 503)
				return
			}
			c.Close()
			if os.Getenv("AI_RUNTIME") == "local" {
				model, err := net.DialTimeout("tcp", "local-ai:6379", time.Second)
				if err != nil {
					http.Error(w, "required model backend unavailable", 503)
					return
				}
				model.Close()
			}
			if r.URL.Path == "/persist" {
				path := filepath.Join(os.Getenv("STORAGE_DIR"), "fixture.txt")
				if r.Method == "POST" {
					_ = os.WriteFile(path, []byte("preserved"), 0600)
				}
				data, _ := os.ReadFile(path)
				w.Write(data)
				return
			}
		}
		if role == "app" && os.Getenv("FIXTURE_NEEDS_SETUP") == "true" {
			published := filepath.Join(os.Getenv("STORAGE_DIR"), "setup.complete")
			if r.Method == "POST" && r.URL.Path == "/setup/publish" {
				if err := os.WriteFile(published, []byte("active"), 0600); err != nil {
					http.Error(w, "publish failed", 500)
					return
				}
				fmt.Fprint(w, "published")
				return
			}
			if _, err := os.Stat(published); (err != nil || os.Getenv("FIXTURE_RESET_SETUP") == "true") && (r.URL.Path == "/readyz" || r.URL.Path == "/protected") {
				http.Error(w, "Server setup is required. Open /setup", 503)
				return
			}
		}
		if os.Getenv("FAIL_READY") == "true" {
			http.Error(w, "candidate failure", 503)
			return
		}
		fmt.Fprint(w, os.Getenv("RELEASE_TEXT")+role+":"+r.URL.Path)
	})
	if role == "app" && os.Getenv("FIXTURE_PUBLIC_PORTS") == "true" {
		go http.ListenAndServe(":25", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "public-tcp") }))
		go func() {
			conn, err := net.ListenPacket("udp", ":7882")
			if err != nil {
				panic(err)
			}
			defer conn.Close()
			buffer := make([]byte, 1024)
			for {
				n, addr, err := conn.ReadFrom(buffer)
				if err != nil {
					return
				}
				conn.WriteTo(buffer[:n], addr)
			}
		}()
	}
	if role == "app" {
		go http.ListenAndServe(":7880", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "rtc:"+r.URL.Path) }))
	}
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		panic(err)
	}
}
