package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A minimal ClickHouse HTTP interface client. The agent talks to the
// servers it manages over HTTP only: the host server on loopback and a
// managed container on its private bridge address. Credentials travel in
// the X-ClickHouse-User/X-ClickHouse-Key headers, never in a URL or an
// argument, and are scrubbed from every error text.

type clickhouseClient struct {
	base     string // http://127.0.0.1:8123
	user     string
	password string
	client   *http.Client
}

func (c clickhouseClient) http() *http.Client {
	if c.client != nil {
		return c.client
	}
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// request sends sql in the body, or in the query string when body carries
// data (INSERT ... FORMAT Native).
func (c clickhouseClient) request(ctx context.Context, sql string, body io.Reader, settings url.Values) (*http.Response, error) {
	params := url.Values{}
	for key, values := range settings {
		params[key] = values
	}
	if body == nil {
		body = strings.NewReader(sql)
	} else {
		params.Set("query", sql)
	}
	target := strings.TrimRight(c.base, "/") + "/"
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, errors.New("invalid ClickHouse address")
	}
	req.Header.Set("X-ClickHouse-User", c.user)
	req.Header.Set("X-ClickHouse-Key", c.password)
	res, err := c.http().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("ClickHouse is unreachable")
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, &clickhouseError{status: res.StatusCode, detail: c.scrub(string(detail))}
	}
	return res, nil
}

type clickhouseError struct {
	status int
	detail string
}

var clickhouseErrorCode = regexp.MustCompile(`Code: (\d+)`)

func (e *clickhouseError) Error() string {
	detail := strings.TrimSpace(e.detail)
	if detail == "" {
		return fmt.Sprintf("ClickHouse answered HTTP %d", e.status)
	}
	return "ClickHouse: " + boundedErrorText(detail, 600)
}

// code is ClickHouse's numeric error code, 0 when absent.
func (e *clickhouseError) code() int {
	match := clickhouseErrorCode.FindStringSubmatch(e.detail)
	if match == nil {
		return 0
	}
	n, _ := strconv.Atoi(match[1])
	return n
}

func (c clickhouseClient) scrub(text string) string {
	if c.password != "" {
		text = strings.ReplaceAll(text, c.password, "[redacted]")
	}
	return text
}

// exec runs one statement and waits for its end, so a failure is reported
// as an HTTP error rather than inside a partly sent response.
func (c clickhouseClient) exec(ctx context.Context, sql string) error {
	res, err := c.request(ctx, sql, nil, url.Values{"wait_end_of_query": {"1"}})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return nil
}

// query returns a small result as text.
func (c clickhouseClient) query(ctx context.Context, sql string) (string, error) {
	res, err := c.request(ctx, sql, nil, url.Values{"wait_end_of_query": {"1"}})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return "", errors.New("the ClickHouse answer could not be read")
	}
	return strings.TrimSpace(string(out)), nil
}

// rows decodes a JSONEachRow result into out, one value per row.
func clickhouseRows[T any](ctx context.Context, c clickhouseClient, sql string) ([]T, error) {
	text, err := c.query(ctx, sql+" FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers = 0")
	if err != nil {
		return nil, err
	}
	rows := []T{}
	decoder := json.NewDecoder(strings.NewReader(text))
	for decoder.More() {
		var row T
		if err := decoder.Decode(&row); err != nil {
			return nil, errors.New("ClickHouse returned an unexpected row")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// stream returns a large result (SELECT ... FORMAT Native) as a stream.
func (c clickhouseClient) stream(ctx context.Context, sql string) (io.ReadCloser, error) {
	res, err := c.request(ctx, sql, nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// insert streams data into an INSERT ... FORMAT statement.
func (c clickhouseClient) insert(ctx context.Context, sql string, data io.Reader) error {
	res, err := c.request(ctx, sql, data, url.Values{"wait_end_of_query": {"1"}})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return nil
}

func (c clickhouseClient) version(ctx context.Context) (string, error) {
	version, err := c.query(ctx, "SELECT version()")
	if err != nil {
		return "", err
	}
	if !clickhouseVersionPattern.MatchString(version) {
		return "", errors.New("ClickHouse reported an unexpected version")
	}
	return version, nil
}

// waitReady polls until the server answers an authenticated query.
func (c clickhouseClient) waitReady(ctx context.Context, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		version, err := c.version(probe)
		cancel()
		if err == nil {
			return version, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("ClickHouse did not become ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---- identifiers, literals and versions -------------------------------------------

var (
	clickhouseVersionPattern = regexp.MustCompile(`^\d{1,3}\.\d{1,2}(\.\d+){0,2}$`)
	clickhouseSeriesPattern  = regexp.MustCompile(`^\d{1,3}\.\d{1,2}$`)
	clickhouseNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// chIdent quotes an identifier with backticks. Names come from the agent
// (app_<id>) or from a server's own catalog; NUL cannot be represented.
func chIdent(name string) (string, error) {
	if name == "" || len(name) > 255 || strings.ContainsRune(name, 0) {
		return "", errors.New("invalid ClickHouse identifier")
	}
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`", nil
}

// chString quotes a string literal.
func chString(value string) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", errors.New("invalid ClickHouse literal")
	}
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(value) + "'", nil
}

func mustCHIdent(name string) string {
	quoted, err := chIdent(name)
	if err != nil {
		panic(err)
	}
	return quoted
}

func mustCHString(value string) string {
	quoted, err := chString(value)
	if err != nil {
		panic(err)
	}
	return quoted
}

// clickhouseSeries is a version's major.minor release series (26.8).
func clickhouseSeries(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	series := parts[0] + "." + parts[1]
	if !clickhouseSeriesPattern.MatchString(series) {
		return ""
	}
	return series
}

// compareClickHouseSeries orders two series numerically (26.10 > 26.8).
func compareClickHouseSeries(a, b string) int {
	pa, pb := strings.SplitN(a, ".", 2), strings.SplitN(b, ".", 2)
	for i := range 2 {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ClickHouseMoveFormat picks how a database moves between two servers:
// BACKUP/RESTORE when the destination's series is the source's or newer
// (ClickHouse reads backups of older releases), otherwise a per-table
// Native export that any release can read.
func ClickHouseMoveFormat(sourceVersion, targetVersion string) string {
	source, target := clickhouseSeries(sourceVersion), clickhouseSeries(targetVersion)
	if source != "" && target != "" && compareClickHouseSeries(target, source) >= 0 {
		return clickhouseFormatBackup
	}
	return clickhouseFormatNative
}
