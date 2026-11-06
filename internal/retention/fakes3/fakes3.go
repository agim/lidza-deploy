// Package fakes3 is an in-memory, path-style S3 endpoint for tests: PUT,
// HEAD, DELETE and ListObjectsV2 with pagination. It checks nothing about
// signatures.
package fakes3

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object is one stored object.
type Object struct {
	Size     int64
	Modified time.Time
}

// Server is a running fake bucket.
type Server struct {
	*httptest.Server
	Bucket string
	// PageSize bounds each list response, to exercise continuation.
	PageSize int

	mu         sync.Mutex
	objects    map[string]Object
	deleted    []string
	failDelete map[string]bool
}

// New starts a fake bucket; close it with Close.
func New(bucket string) *Server {
	s := &Server{Bucket: bucket, PageSize: 1000, objects: map[string]Object{}, failDelete: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Put stores an object directly; key is the full bucket key.
func (s *Server) Put(key string, size int64, modified time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = Object{Size: size, Modified: modified.UTC()}
}

// FailDelete makes deleting key fail with 403.
func (s *Server) FailDelete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failDelete[key] = true
}

// Keys returns the stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Deleted returns the keys deleted so far, in order.
func (s *Server) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deleted)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+s.Bucket)
	if !ok {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(rest, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case r.Method == http.MethodPut && key != "":
		n, _ := io.Copy(io.Discard, r.Body)
		s.objects[key] = Object{Size: n, Modified: time.Now().UTC()}
		w.Header().Set("ETag", `"fake"`)
	case r.Method == http.MethodHead && key != "":
		o, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
		w.Header().Set("Last-Modified", o.Modified.Format(http.TimeFormat))
	case r.Method == http.MethodDelete && key != "":
		if s.failDelete[key] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		delete(s.objects, key)
		s.deleted = append(s.deleted, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type listContent struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
}

type listResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Contents              []listContent `xml:"Contents"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	limit := s.PageSize
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil && n > 0 && n < limit {
		limit = n
	}
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > q.Get("continuation-token") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out listResult
	for i, k := range keys {
		if i == limit {
			out.IsTruncated = true
			out.NextContinuationToken = keys[i-1]
			break
		}
		o := s.objects[k]
		out.Contents = append(out.Contents, listContent{Key: k, Size: o.Size, LastModified: o.Modified.Format(time.RFC3339), ETag: `"fake"`})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}
