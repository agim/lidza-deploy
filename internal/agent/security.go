package agent

import (
	"encoding/json"
	"github.com/agim/lidza/pkg/report"
	"net/url"
	"strconv"
	"strings"
)

// Probe is diagnostic classification, never proof of successful exploitation.
// Client attribution is deliberately absent until trusted proxy telemetry exists.
type Probe struct {
	Category string `json:"category"`
	Status   int    `json:"status"`
	Outcome  string `json:"outcome"`
}

func securityRecord(line string) (report.Error, *Probe, bool) {
	var row struct {
		Message   string `json:"msg"`
		Path      string `json:"path"`
		Method    string `json:"method"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal([]byte(line), &row) != nil || row.Message != "request" || row.Status < 100 || row.Status >= 500 || row.Method == "" || len(row.Path) > 4096 {
		return report.Error{}, nil, false
	}
	path := strings.SplitN(row.Path, "?", 2)[0]
	decoded := strings.ToLower(path)
	for i := 0; i < 2; i++ {
		v, err := url.PathUnescape(decoded)
		if err != nil {
			break
		}
		decoded = v
	}
	category := ""
	switch {
	case strings.Contains(decoded, "../") || strings.Contains(decoded, `..\`):
		category = "path-traversal"
	case strings.Contains(decoded, "/.env") || strings.Contains(decoded, "/proc/self/environ") || strings.Contains(decoded, "/phpinfo.php") || strings.Contains(decoded, "/.git/"):
		category = "secret-file"
	case strings.HasSuffix(decoded, "/fs/exec") || strings.HasSuffix(decoded, "/rds/execute") || strings.Contains(decoded, "/node-load-method/"):
		category = "execution-probe"
	case decoded == "/api/actuator" || strings.HasSuffix(decoded, "/swagger.zip") || strings.HasSuffix(decoded, "/openapi.zip"):
		category = "endpoint-discovery"
	default:
		return report.Error{}, nil, false
	}
	outcome := "rejected"
	if row.Status >= 200 && row.Status < 300 {
		outcome = "review-response"
	} else if row.Status >= 300 && row.Status < 400 {
		outcome = "redirected"
	}
	return report.Error{Source: "server", Message: "Suspected " + category + " probe: HTTP " + strconv.Itoa(row.Status), Route: path, Method: row.Method, RequestID: row.RequestID}, &Probe{category, row.Status, outcome}, true
}
