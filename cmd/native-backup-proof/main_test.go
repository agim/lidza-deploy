package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
	"github.com/agim/lidza/pkg/credentials"
)

func TestProofPrivacyChecksumAndConditionalWrite(t *testing.T) {
	for _, mode := range []string{"success", "public", "existing", "raced", "corrupt", "short", "long", "wrong-checksum", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			archive := []byte("fixture-only PGDMP complete restored snapshot")
			digest := sha256.Sum256(archive)
			x := expectation{Bucket: "private", Region: "nyc3", Prefix: "saved", Key: "native-proof/pilot/" + strings.Repeat("a", 32) + ".dump", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(archive))}
			var mu sync.Mutex
			objects := map[string][]byte{}
			if mode == "existing" {
				objects["/private/saved/"+x.Key] = []byte("existing backup")
			}
			secretPuts, probes, deleted, conditional := 0, 0, 0, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				authorized := r.Header.Get("Authorization") != "" || r.URL.Query().Get("X-Amz-Signature") != ""
				data, exists := objects[r.URL.Path]
				if !authorized {
					if mode == "public" && exists {
						w.Header().Set("Content-Length", fmt.Sprint(len(data)))
						w.WriteHeader(200)
					} else {
						w.WriteHeader(403)
					}
					return
				}
				switch r.Method {
				case "HEAD":
					if !exists {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				case "PUT":
					body, _ := io.ReadAll(r.Body)
					if strings.HasPrefix(string(body), "lidza privacy probe ") {
						probes++
						objects[r.URL.Path] = body
						return
					}
					secretPuts++
					conditional = r.Header.Get("If-None-Match") == "*"
					if mode == "raced" {
						objects[r.URL.Path] = []byte("concurrent backup")
						w.WriteHeader(412)
						return
					}
					if mode == "redirect" {
						w.Header().Set("Location", "/unexpected")
						w.WriteHeader(307)
						return
					}
					objects[r.URL.Path] = body
				case "DELETE":
					deleted++
					delete(objects, r.URL.Path)
				case "GET":
					if !exists {
						w.WriteHeader(404)
						return
					}
					if mode == "corrupt" {
						data = bytes.Repeat([]byte("x"), len(data))
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					w.Write(data)
				default:
					w.WriteHeader(405)
				}
			}))
			defer server.Close()
			cfg := storage.Config{Provider: "s3", Endpoint: server.URL, Bucket: x.Bucket, Region: x.Region, Prefix: x.Prefix, AccessKey: "fixture-access", SecretKey: "fixture-secret", Timeout: time.Second}
			input := archive
			switch mode {
			case "short":
				input = archive[:len(archive)-1]
			case "long":
				input = append(bytes.Clone(archive), 'x')
			case "wrong-checksum":
				input = bytes.Repeat([]byte("x"), len(archive))
			}
			root := t.TempDir()
			r, err := prove(context.Background(), cfg, x, bytes.NewReader(input), root)
			if mode == "success" {
				if err != nil || !r.CompleteReadback || !r.Privacy.Private || r.SHA256 != x.SHA256 || r.UploadSHA256 != x.SHA256 || r.ReadbackSHA256 != x.SHA256 || r.ConfigurationChanged || r.CredentialsExported {
					t.Fatalf("receipt=%+v err=%v", r, err)
				}
			} else if err == nil {
				t.Fatal("unsafe proof accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "short" || mode == "long" || mode == "wrong-checksum" || mode == "existing" {
				if probes != 0 || secretPuts != 0 || deleted != 0 {
					t.Fatal("unverified input/existing key touched storage")
				}
			}
			if mode == "public" && (probes != 1 || deleted != 1 || secretPuts != 0) {
				t.Fatal("public storage received DB bytes")
			}
			if secretPuts > 0 && (!conditional || probes != 1 || deleted != 1) {
				t.Fatal("upload missed exact-key probe or conditional write")
			}
			if mode == "raced" && string(objects["/private/saved/"+x.Key]) != "concurrent backup" {
				t.Fatal("concurrent object overwritten")
			}
			if mode == "existing" && string(objects["/private/saved/"+x.Key]) != "existing backup" {
				t.Fatal("existing object modified")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("private spool was not cleaned")
			}
		})
	}
}

func TestProfileReadsOnlySealedMatchingState(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "config"), 0700)
	key := bytes.Repeat([]byte{17}, 32)
	os.WriteFile(filepath.Join(dir, "config/master.key"), []byte(hex.EncodeToString(key)), 0600)
	x := expectation{Endpoint: "https://storage.example", Bucket: "private", Region: "region", Prefix: "saved", Key: "native-proof/pilot/" + strings.Repeat("a", 32) + ".dump", SHA256: strings.Repeat("b", 64), Size: 42}
	cfg := storage.Config{Provider: "s3", Endpoint: x.Endpoint, Bucket: x.Bucket, Region: x.Region, Prefix: x.Prefix, AccessKey: "fixture-access", SecretKey: "fixture-secret"}
	plain, _ := json.Marshal(map[string]any{"backup_storage": cfg})
	sealed, _ := credentials.Encrypt(key, plain)
	raw, _ := json.Marshal(sealed)
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, raw, 0600)
	t.Setenv("LIDZA_MASTER_KEY", strings.Repeat("f", 64))
	got, err := readProfile(dir, x)
	if err != nil || got.SecretKey != cfg.SecretKey {
		t.Fatal("explicit retained key/profile not used", err)
	}
	if err = validateExpectation(x); err != nil {
		t.Fatal(err)
	}
	wrong := x
	wrong.Bucket = "other"
	if _, err = readProfile(dir, wrong); err == nil {
		t.Fatal("foreign destination accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("state changed")
	}
	os.WriteFile(path, plain, 0600)
	if _, err = readProfile(dir, x); err == nil {
		t.Fatal("plaintext fallback accepted")
	}
	os.WriteFile(path, raw, 0644)
	os.Chmod(path, 0644)
	if _, err = readProfile(dir, x); err == nil {
		t.Fatal("unprotected state accepted")
	}
	wrong = x
	wrong.Key = "databases/pilot/predeployment.dump"
	if validateExpectation(wrong) == nil {
		t.Fatal("non-proof/overwrite key accepted")
	}
}
