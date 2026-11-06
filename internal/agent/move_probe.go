package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Move planning and stall diagnostics:
//
//   - POST /v1/move/probe          a short disk write and read measurement
//   - GET  /v1/move/apps/{id}/diagnostics  what this agent is doing for an
//     application's move right now (running operations, ClickHouse backups
//     and queries), bounded and free of values and credentials
//
// The probe writes and reads one temporary file under the agent's data
// directory, which holds move spools, staged uploads and backups, so it
// measures the disk a move uses. The read usually comes from the page cache
// and is an upper bound.

const (
	moveProbeDefaultBytes = 32 << 20
	moveProbeMaxBytes     = 256 << 20
	moveDiagnosticLines   = 40
)

// MoveProbe is one disk measurement.
type MoveProbe struct {
	Bytes      int64   `json:"bytes"`
	WriteBytes float64 `json:"write_bytes_per_second"`
	ReadBytes  float64 `json:"read_bytes_per_second"`
	Seconds    float64 `json:"seconds"`
	Path       string  `json:"path"`
}

// MoveDiagnostics lists what an agent does for a move right now.
type MoveDiagnostics struct {
	Lines []string `json:"lines"`
}

// moveProbeDisk measures dir; tests replace it.
var moveProbeDisk = func(ctx context.Context, dir string, size int64) (MoveProbe, error) {
	out := MoveProbe{Bytes: size, Path: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return out, errors.New("cannot prepare the probe directory")
	}
	if err := diskFits(dir, uint64(size), "the disk probe"); err != nil {
		return out, err
	}
	file, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return out, errors.New("cannot create the probe file")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	block := make([]byte, 1<<20)
	if _, err := rand.Read(block); err != nil {
		return out, err
	}
	started := time.Now()
	for written := int64(0); written < size; written += int64(len(block)) {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if _, err := file.Write(block[:min(int64(len(block)), size-written)]); err != nil {
			return out, fmt.Errorf("the probe could not write: %w", err)
		}
	}
	if err := file.Sync(); err != nil {
		return out, fmt.Errorf("the probe could not sync: %w", err)
	}
	write := time.Since(started)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	started = time.Now()
	if _, err := io.Copy(io.Discard, file); err != nil {
		return out, fmt.Errorf("the probe could not read: %w", err)
	}
	read := time.Since(started)
	out.WriteBytes = float64(size) / max(write.Seconds(), 0.001)
	out.ReadBytes = float64(size) / max(read.Seconds(), 0.001)
	out.Seconds = (write + read).Seconds()
	return out, nil
}

func (m *Manager) moveProbeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/move/probe", func(w http.ResponseWriter, r *http.Request) {
		size := int64(moveProbeDefaultBytes)
		if raw := r.URL.Query().Get("bytes"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1<<20 || n > moveProbeMaxBytes {
				Fail(w, 400, errors.New("bytes must be between 1 MiB and 256 MiB"))
				return
			}
			size = n
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		probe, err := moveProbeDisk(ctx, filepath.Join(m.cfg.DataDir, "move-probe"), size)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, probe)
	})
	mux.HandleFunc("GET /v1/move/apps/{id}/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid application id"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, m.moveDiagnostics(ctx, id, r.URL.Query().Get("move")))
	})
}

// moveDiagnostics collects the running operations of a move and, when the
// application has a managed ClickHouse database here, its latest backups and
// running queries.
func (m *Manager) moveDiagnostics(ctx context.Context, id, move string) MoveDiagnostics {
	out := MoveDiagnostics{Lines: []string{}}
	add := func(format string, args ...any) {
		if len(out.Lines) < moveDiagnosticLines {
			out.Lines = append(out.Lines, boundedErrorText(fmt.Sprintf(format, args...), 300))
		}
	}
	now := time.Now()
	progress := m.progress.prefixed("")
	keys := make([]string, 0, len(progress))
	for key := range progress {
		if strings.HasPrefix(key, "database/") || move != "" && strings.HasPrefix(key, "move/"+move+"/") {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		p := progress[key]
		add("operation %s: %s, %d of %d bytes, last change %s ago", key, p.Phase, p.DoneBytes, p.TotalBytes, now.Sub(p.UpdatedAt).Round(time.Second))
	}
	if len(keys) == 0 {
		add("no move or database operation is running on this agent")
	}
	m.mu.Lock()
	_, container := m.data.Apps[id]
	_, native := m.data.NativeHostData[id]
	transfers := m.moveTransfers.Load()
	m.mu.Unlock()
	add("move transfers in progress on this agent: %d", transfers)
	if !container && !native {
		return out
	}
	service, err := m.clickhouseServiceFor(ctx, id)
	if err != nil {
		return out
	}
	type backupRow struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error"`
		Start  string `json:"start_time"`
		Files  uint64 `json:"num_files"`
		Size   uint64 `json:"total_size"`
	}
	if rows, err := clickhouseRows[backupRow](ctx, service.client, "SELECT name, toString(status) AS status, error, toString(start_time) AS start_time, num_files, total_size FROM system.backups ORDER BY start_time DESC LIMIT 3"); err == nil {
		for _, row := range rows {
			add("ClickHouse backup %s: %s since %s, %d files, %d bytes%s", row.Name, row.Status, row.Start, row.Files, row.Size, moveDiagnosticError(service.client.scrub(row.Error)))
		}
	} else {
		add("ClickHouse system.backups unavailable: %s", service.client.scrub(err.Error()))
	}
	type processRow struct {
		Elapsed float64 `json:"elapsed"`
		Kind    string  `json:"kind"`
		Read    uint64  `json:"read_bytes"`
		Written uint64  `json:"written_bytes"`
	}
	// Only the statement's kind is listed: a query's text may hold values.
	database, err := chString(service.database)
	if err != nil {
		return out
	}
	if rows, err := clickhouseRows[processRow](ctx, service.client, "SELECT elapsed, upper(splitByChar(' ', trimLeft(query))[1]) AS kind, read_bytes, written_bytes FROM system.processes WHERE current_database = "+database+" OR position(query, "+database+") > 0 ORDER BY elapsed DESC LIMIT 5"); err == nil {
		for _, row := range rows {
			add("ClickHouse query %s running for %.0fs: %d bytes read, %d written", row.Kind, row.Elapsed, row.Read, row.Written)
		}
		if len(rows) == 0 {
			add("no ClickHouse query of %s is running", service.database)
		}
	}
	return out
}

func moveDiagnosticError(text string) string {
	if text == "" {
		return ""
	}
	return ": " + boundedErrorText(text, 160)
}
