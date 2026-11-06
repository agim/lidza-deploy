package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/agim/lidza/pkg/report"
)

// Console output of the hosted runtimes, read from Docker consoles and
// native systemd journals alike. Līdza (Go) applications log structured
// JSON; Rails logs through Ruby's Logger::Formatter or the plain tagged
// formatter; Node prints uncaught exceptions as "Error: ..." followed by
// "at" frames, and pino/winston log JSON. One scan serves every source.

var (
	// "F, [2026-11-05T10:00:00.123456 #1234] FATAL -- : message" (Ruby Logger::Formatter).
	rubyLoggerPrefix = regexp.MustCompile(`^[A-Z], \[[^\]]{0,64}\] +(DEBUG|INFO|WARN|ERROR|FATAL|UNKNOWN|ANY) -- [^:]{0,64}: `)
	railsStarted     = regexp.MustCompile(`^Started ([A-Z]{3,8}) "([^"]*)" for `)
	railsCompleted   = regexp.MustCompile(`^Completed (\d{3}) ?([-A-Za-z' ]{0,64}?)(?: in \d+(?:\.\d+)?ms.*)?$`)
	railsException   = regexp.MustCompile(`^([A-Z][A-Za-z0-9_]*(?:::[A-Z][A-Za-z0-9_]*)*) \((.*)\):$`)
	railsJobFailure  = regexp.MustCompile(`^Error performing [A-Za-z0-9_:]+ \(Job ID: [^)]{0,128}\)`)
	railsRouting     = regexp.MustCompile(`^ActionController::RoutingError \(No route matches \[([A-Z]{3,8})\] "([^"]*)"\)`)
	railsBacktrace   = regexp.MustCompile(`^(?:\S+\.rb:\d+:in |from \S+:\d+|\S*/gems/\S+:\d+)`)
	pumaFailure      = regexp.MustCompile(`^(?:\[\d+\] )?! \S`)
	nodeException    = regexp.MustCompile(`^([A-Z][A-Za-z0-9_$]*(?:Error|Exception|Rejection)|Error|Exception)(?: \[[A-Z0-9_]+\])?: (.+)$`)
	nodeFrame        = regexp.MustCompile(`^\s+at \S`)
	accessLogRequest = regexp.MustCompile(`"(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) ([^" ]+) HTTP/[0-9.]+" (\d{3})\b`)
	plainRequestLine = regexp.MustCompile(`(?:^|\s)(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) (/[^\s"]*) (\d{3})(?:\s|$)`)
	requestIDTag     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{16,128}$`)
)

// consoleLine is a console line with its logger framing removed.
type consoleLine struct {
	text      string // payload without a Ruby severity prefix and leading log tags
	severity  string // ERROR, FATAL, ... from a Logger::Formatter prefix or a [level] tag
	requestID string // the request id tag of ActiveSupport::TaggedLogging, when present
}

// normalizeConsoleLine strips the Ruby Logger::Formatter prefix and leading
// "[tag]" groups. A level tag becomes the severity; a tag that looks like a
// request id (long, with digits) becomes the request id. Structured JSON
// lines are never touched by this (they start with "{").
func normalizeConsoleLine(payload string) consoleLine {
	line := consoleLine{text: payload}
	if m := rubyLoggerPrefix.FindStringSubmatch(payload); m != nil {
		line.severity = m[1]
		line.text = payload[len(m[0]):]
	}
	for strings.HasPrefix(line.text, "[") {
		end := strings.IndexByte(line.text, ']')
		if end <= 1 || end > 130 || strings.ContainsRune(line.text[1:end], '[') || end+1 < len(line.text) && line.text[end+1] != ' ' {
			break
		}
		tag := line.text[1:end]
		switch upper := strings.ToUpper(tag); upper {
		case "ERROR", "FATAL", "PANIC", "CRITICAL", "WARN", "WARNING", "INFO", "DEBUG", "TRACE":
			if line.severity == "" {
				line.severity = upper
			}
		default:
			if line.requestID == "" && requestIDTag.MatchString(tag) && strings.ContainsAny(tag, "0123456789") {
				line.requestID = tag
			}
		}
		line.text = strings.TrimPrefix(line.text[end+1:], " ")
	}
	return line
}

type startedRequest struct{ method, path string }

// consoleScan classifies the lines of one console read in order. It keeps
// the "Started METHOD path" context of Rails requests to attach routes to
// their later failures and probes, and remembers the runtime family of the
// last failure so that its stack frames can follow it.
type consoleScan struct {
	started  map[string]startedRequest // by request id; "" for untagged logs
	lastKind string
	lastAge  int
}

func newConsoleScan() *consoleScan { return &consoleScan{started: map[string]startedRequest{}} }

func (s *consoleScan) found(e report.Error, kind string) report.Error {
	s.lastKind, s.lastAge = kind, 0
	return e
}

// classify returns an application failure, a suspected probe, or nothing.
func (s *consoleScan) classify(payload string) (report.Error, *Probe, bool) {
	s.lastAge++
	if strings.HasPrefix(payload, "{") {
		if e, ok := jsonRecord(payload); ok {
			return s.found(e, "json"), nil, true
		}
		return securityRecord(payload)
	}
	line := normalizeConsoleLine(payload)
	text := strings.TrimSpace(line.text)
	e := report.Error{Source: "server", RequestID: line.requestID}
	switch {
	case text == "":
		return report.Error{}, nil, false
	case railsStarted.MatchString(text):
		m := railsStarted.FindStringSubmatch(text)
		if len(s.started) >= 256 {
			clear(s.started)
		}
		s.started[line.requestID] = startedRequest{m[1], strings.SplitN(m[2], "?", 2)[0]}
		return report.Error{}, nil, false
	case railsRouting.MatchString(text):
		// Routing errors are ordinary 404s for diagnosis; only scanner paths are reported.
		m := railsRouting.FindStringSubmatch(text)
		return classifyProbe(m[1], m[2], 404, line.requestID)
	case railsException.MatchString(text):
		e.Message = strings.TrimSuffix(text, ":")
		if request, ok := s.started[line.requestID]; ok {
			e.Method, e.Route = request.method, request.path
		}
		return s.found(e, "rails"), nil, true
	case railsJobFailure.MatchString(text):
		e.Message = strings.TrimSuffix(text, ":")
		return s.found(e, "rails"), nil, true
	case railsCompleted.MatchString(text):
		m := railsCompleted.FindStringSubmatch(text)
		status, _ := strconv.Atoi(m[1])
		request := s.started[line.requestID]
		if status >= 500 {
			e.Message = strings.TrimSpace("HTTP " + m[1] + " " + strings.TrimSpace(m[2]))
			e.Method, e.Route = request.method, request.path
			return s.found(e, "rails"), nil, true
		}
		if request.path != "" {
			return classifyProbe(request.method, request.path, status, line.requestID)
		}
		return report.Error{}, nil, false
	case pumaFailure.MatchString(text) || strings.HasPrefix(text, "Puma caught this error:") || strings.HasPrefix(text, "bundler: failed to load command"):
		e.Message = text
		return s.found(e, "rails"), nil, true
	case nodeException.MatchString(text):
		e.Message = text
		return s.found(e, "node"), nil, true
	case strings.HasPrefix(text, "npm ERR! "):
		e.Message = text
		return s.found(e, "node"), nil, true
	}
	upper := strings.ToUpper(text)
	switch {
	case line.severity == "ERROR" || line.severity == "FATAL" || line.severity == "PANIC" || line.severity == "CRITICAL":
		e.Message = text
		return s.found(e, "text"), nil, true
	case strings.HasPrefix(upper, "PANIC:"):
		e.Message = text
		return s.found(e, "go"), nil, true
	case strings.HasPrefix(upper, "FATAL ERROR:") || strings.HasPrefix(upper, "ERROR:") || strings.Contains(upper, "LEVEL=ERROR") || strings.Contains(upper, " ERROR:") || strings.Contains(upper, " FATAL ERROR:"):
		e.Message = text
		return s.found(e, "text"), nil, true
	}
	if m := accessLogRequest.FindStringSubmatch(text); m != nil {
		status, _ := strconv.Atoi(m[3])
		return classifyProbe(m[1], m[2], status, line.requestID)
	}
	if m := plainRequestLine.FindStringSubmatch(text); m != nil {
		status, _ := strconv.Atoi(m[3])
		return classifyProbe(m[1], m[2], status, line.requestID)
	}
	return report.Error{}, nil, false
}

// continuation returns the stack frame text when payload extends the last
// failure: Go panic goroutine dumps, Node "at" frames, Ruby backtraces.
func (s *consoleScan) continuation(payload string) (string, bool) {
	if s.lastAge > 200 {
		return "", false
	}
	switch s.lastKind {
	case "go":
		if strings.HasPrefix(payload, "goroutine ") || strings.HasPrefix(payload, "\t") || strings.HasSuffix(payload, ")") {
			return payload, true
		}
	case "node":
		if text := strings.TrimRight(normalizeConsoleLine(payload).text, " \t\r"); nodeFrame.MatchString(text) {
			return text, true
		}
	case "rails":
		if text := strings.TrimSpace(normalizeConsoleLine(payload).text); railsBacktrace.MatchString(text) {
			return text, true
		}
	}
	return "", false
}

// jsonRecord reads a structured log line: slog/zerolog/winston levels,
// pino's numeric levels and error objects, and HTTP status 500 and above.
func jsonRecord(line string) (report.Error, bool) {
	e := report.Error{Source: "server"}
	var data map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &data) != nil || data == nil {
		return e, false
	}
	text := func(key string) string { var s string; json.Unmarshal(data[key], &s); return s }
	level := strings.ToUpper(text("level"))
	if level == "" {
		// pino: 50 is error, 60 is fatal.
		var numeric int
		if json.Unmarshal(data["level"], &numeric) == nil && numeric >= 50 {
			level = "ERROR"
		}
	}
	if level == "" {
		level = strings.ToUpper(text("severity"))
	}
	var status int
	json.Unmarshal(data["status"], &status)
	if level != "ERROR" && level != "FATAL" && level != "PANIC" && level != "CRITICAL" && status < 500 {
		return e, false
	}
	e.Message = text("msg")
	if e.Message == "" {
		e.Message = text("message")
	}
	detail := text("error")
	e.Stack = text("stack")
	if detail == "" {
		// pino serializes errors as objects under err (or error).
		for _, key := range []string{"err", "error"} {
			var wrapped struct{ Type, Message, Stack string }
			if json.Unmarshal(data[key], &wrapped) == nil && wrapped.Message != "" {
				detail = wrapped.Message
				if wrapped.Type != "" {
					detail = wrapped.Type + ": " + wrapped.Message
				}
				if e.Stack == "" {
					e.Stack = wrapped.Stack
				}
				break
			}
		}
	}
	if detail != "" {
		if e.Message == "" {
			e.Message = detail
		} else {
			e.Message += ": " + detail
		}
	}
	if status >= 500 && level != "ERROR" {
		e.Message = "HTTP " + strconv.Itoa(status) + " " + e.Message
	}
	e.Route = text("route")
	if e.Route == "" {
		e.Route = strings.SplitN(text("path"), "?", 2)[0]
	}
	e.Method = text("method")
	e.RequestID = text("request_id")
	return e, e.Message != ""
}
