package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type ConsoleJobRequest struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Command string `json:"command"`
}

type ConsoleJobView struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	Output     string  `json:"output"`
	NextOffset int     `json:"next_offset"`
	OutputSize int     `json:"output_size"`
	Truncated  bool    `json:"truncated"`
	Duration   float64 `json:"duration_seconds"`
	ExitCode   int     `json:"exit_code"`
	Error      string  `json:"error,omitempty"`
}

type consoleJob struct {
	evicted                   bool
	mu                        sync.Mutex
	id, owner, state, failure string
	digest                    [32]byte
	started, finished         time.Time
	output                    cappedConsoleBuffer
	result                    ConsoleResult
}

func (j *consoleJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.output.Write(p)
}

func (j *consoleJob) view(offset int) ConsoleJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	size := j.output.Len()
	offset = min(max(0, offset), size)
	end := min(size, offset+64<<10)
	for end < size && end > offset && !utf8.RuneStart(j.output.Bytes()[end]) {
		end--
	}
	if end == size && end > offset && j.state == "running" {
		start := end - 1
		for start > offset && !utf8.RuneStart(j.output.Bytes()[start]) {
			start--
		}
		if !utf8.FullRune(j.output.Bytes()[start:end]) {
			end = start
		}
	}
	failure := j.failure
	if j.evicted {
		failure = "Output expired from console history; command was not repeated."
	}
	duration := j.result.Duration
	if j.state == "running" {
		duration = time.Since(j.started).Seconds()
	}
	return ConsoleJobView{ID: j.id, State: j.state, Output: string(j.output.Bytes()[offset:end]), NextOffset: end, OutputSize: size, Truncated: j.output.truncated, Duration: duration, ExitCode: j.result.ExitCode, Error: failure}
}

var consoleJobID = regexp.MustCompile(`^[a-zA-Z0-9-]{16,64}$`)

func (m *Manager) startConsoleJob(input ConsoleJobRequest) (*consoleJob, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("hosting agent must run as root to use the server console")
	}
	return m.queueConsoleJob(input, runConsoleOutput)
}

func (m *Manager) queueConsoleJob(input ConsoleJobRequest, execute func(context.Context, string, io.Writer) (ConsoleResult, error)) (*consoleJob, error) {
	if !consoleJobID.MatchString(input.ID) || input.Owner == "" || len(input.Owner) > 256 {
		return nil, errors.New("invalid console job identity")
	}
	if strings.TrimSpace(input.Command) == "" || len(input.Command) > consoleMaxInput || strings.ContainsRune(input.Command, 0) {
		return nil, errors.New("command must contain 1 to 65536 bytes")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.consoleJobs == nil {
		m.consoleJobs = map[string]*consoleJob{}
	}
	digest := sha256.Sum256([]byte(input.Command))
	if existing := m.consoleJobs[input.ID]; existing != nil {
		if existing.owner != input.Owner || existing.digest != digest {
			return nil, errors.New("console job ID already used")
		}
		return existing, nil // Safe retry after a lost acknowledgement; never execute twice.
	}
	if m.ctx.Err() != nil || m.upgradePending() {
		return nil, errors.New("hosting agent stopping or upgrading")
	}
	for id, j := range m.consoleJobs {
		j.mu.Lock()
		running, expired := j.state == "running", !j.finished.IsZero() && time.Since(j.finished) > 30*time.Minute
		j.mu.Unlock()
		if running {
			return nil, errors.New("a console command is already running on this server")
		}
		if expired {
			delete(m.consoleJobs, id)
		}
	}
	if len(m.consoleJobs) >= 256 {
		return nil, errors.New("console history is full; retry after older results expire (30 minutes)")
	}
	// Bound retained output without evicting a recently submitted job's retry key.
	// Keep at most eight outputs, but retain retry identities for 30 minutes.
	var retained int
	var oldest *consoleJob
	for _, job := range m.consoleJobs {
		if !job.evicted {
			retained++
			if oldest == nil || job.started.Before(oldest.started) {
				oldest = job
			}
		}
	}
	if retained >= 8 && oldest != nil {
		oldest.mu.Lock()
		oldest.output = cappedConsoleBuffer{}
		oldest.evicted = true
		oldest.mu.Unlock()
	}

	j := &consoleJob{id: input.ID, owner: input.Owner, digest: digest, state: "running", started: time.Now()}
	m.consoleJobs[input.ID] = j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		result, err := execute(m.ctx, input.Command, j)
		j.mu.Lock()
		defer j.mu.Unlock()
		j.result = result
		j.finished = time.Now()
		j.state = "finished"
		if err != nil {
			j.state = "interrupted"
			j.failure = "Command interrupted: " + err.Error()
		}
	}()
	return j, nil
}

func (m *Manager) consoleJobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/console/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var input ConsoleJobRequest
		if err := Decode(w, r, &input); err != nil {
			Fail(w, 400, err)
			return
		}
		j, err := m.startConsoleJob(input)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, j.view(0))
	})
	mux.HandleFunc("GET /v1/console/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		m.mu.Lock()
		j := m.consoleJobs[r.PathValue("job")]
		m.mu.Unlock()
		expired := false
		if j != nil {
			j.mu.Lock()
			expired = j.evicted || !j.finished.IsZero() && time.Since(j.finished) > 30*time.Minute
			j.mu.Unlock()
		}
		if j == nil || j.owner != r.URL.Query().Get("owner") || expired {
			Fail(w, 404, errors.New("console result unavailable; agent may have restarted or result expired. Check command outcome before rerunning"))
			return
		}
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset < 0 {
			Fail(w, 400, errors.New("invalid output offset"))
			return
		}
		JSON(w, 200, j.view(offset))
	})
}
