// A tiny real HTTP application checks the agent's private service DNS,
// credentials and network attachment without needing unrelated providers.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func query(r *http.Request, sql string) (string, error) {
	u, err := url.Parse(os.Getenv("CLICKHOUSE_URL"))
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("database", os.Getenv("CLICKHOUSE_DATABASE"))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), "POST", u.String(), strings.NewReader(sql))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(os.Getenv("CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD"))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ClickHouse status %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), err
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		commands := []string{"SELECT 1"}
		if r.URL.Path == "/write" {
			commands = []string{"CREATE TABLE IF NOT EXISTS retained (v String) ENGINE=MergeTree ORDER BY tuple()", "INSERT INTO retained VALUES ('persisted')", "SELECT v FROM retained LIMIT 1"}
		}
		if r.URL.Path == "/read" {
			commands = []string{"SELECT v FROM retained LIMIT 1"}
		}
		var result string
		for _, sql := range commands {
			var err error
			result, err = query(r, sql)
			if err != nil {
				http.Error(w, "telemetry unavailable", 503)
				return
			}
		}
		fmt.Fprint(w, result)
	})
	if err := http.ListenAndServe(":3000", nil); err != nil {
		panic(err)
	}
}
