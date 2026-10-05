package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agim/lidza/packs/analytics"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const appErrorLimit = 500

// AppErrors reads the framework's existing error store, never app tables or
// credentials supplied by the browser. Retention remains owned by analytics.
type AppErrors struct {
	Status    string                  `json:"status"`
	Errors    []analytics.StoredError `json:"errors"`
	Limit     int                     `json:"limit"`
	Shared    bool                    `json:"shared,omitempty"`
	Truncated bool                    `json:"truncated,omitempty"`
}

func (m *Manager) appErrors(ctx context.Context, id string) (AppErrors, error) {
	out := AppErrors{Status: "ready", Errors: []analytics.StoredError{}, Limit: appErrorLimit}
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	env := maps.Clone(a.Env)
	d := m.data.Databases[a.Bindings["DATABASE_URL"]]
	for otherID, other := range m.data.Apps {
		if otherID != id && !other.Retiring && sameErrorDatabase(env["DATABASE_URL"], other.Env["DATABASE_URL"]) {
			out.Shared = true
			break
		}
	}
	m.mu.Unlock()
	if !ok || a.Retiring {
		return out, errors.New("application is unavailable")
	}
	if env["DATABASE_URL"] == "" {
		out.Status = "needs_database"
		return out, nil
	}
	select {
	case m.databaseSlots <- struct{}{}:
		defer func() { <-m.databaseSlots }()
	default:
		return out, errors.New("database operations are busy; retry shortly")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(env["DATABASE_URL"])
	if err != nil {
		return out, errors.New("application database connection is invalid")
	}
	// Managed PostgreSQL has no published port. On supported Linux hosts the
	// agent reaches its private bridge address using the app's own database role.
	if d.Mode == "local" && d.URL == env["DATABASE_URL"] {
		if !d.Ready {
			return out, errors.New("application database is not ready")
		}
		address, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "`+d.Network+`"}}{{.IPAddress}}{{end}}`, d.Network)
		if err != nil || net.ParseIP(address) == nil {
			return out, errors.New("application database network is unavailable")
		}
		cfg.ConnConfig.Host = address
		cfg.ConnConfig.Fallbacks = nil
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return out, errors.New("application error store could not be opened")
	}
	defer pool.Close()
	rows, err := analytics.Recent(ctx, pool, appErrorLimit)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "42P01" {
			out.Status = "needs_analytics"
			return out, nil
		}
		return out, errors.New("application error store is unavailable; check database access and analytics migrations")
	}
	secrets := outputSecrets(env)
	encodedBytes := 0
	for _, e := range rows {
		// Queries, user identifiers and user agents can contain personal data or
		// arbitrary tokens. The dashboard needs route/request correlation only.
		e.URL, e.UserID = nil, nil
		e.Message = boundedErrorText(scrubOutput(e.Message, secrets), 2048)
		if e.Source != "server" && e.Source != "client" {
			e.Source = "other"
		}
		e.ID = boundedErrorText(e.ID, 64)
		e.Fingerprint = boundedErrorText(e.Fingerprint, 64)
		for _, item := range []struct {
			field *string
			limit int
		}{{e.Stack, 4096}, {e.Route, 512}, {e.Method, 16}, {e.RequestID, 128}} {
			field := item.field
			if field != nil {
				*field = boundedErrorText(scrubOutput(*field, secrets), item.limit)
			}
		}
		encoded, _ := json.Marshal(e)
		encodedBytes += len(encoded)
		if encodedBytes > 2<<20 {
			out.Truncated = true
			break
		}
		out.Errors = append(out.Errors, e)
	}
	return out, nil
}

func boundedErrorText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…"
}

func (m *Manager) errorsRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := m.appErrors(r.Context(), r.PathValue("id"))
	if err != nil {
		Fail(w, http.StatusServiceUnavailable, err)
		return
	}
	JSON(w, http.StatusOK, out)
}

// Framework records are scoped by database, not by hosting app ID.
func sameErrorDatabase(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	a, errA := url.Parse(left)
	b, errB := url.Parse(right)
	if errA != nil || errB != nil || a.Hostname() == "" || b.Hostname() == "" {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() == "" {
			return "5432"
		}
		return u.Port()
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b) && a.Path == b.Path
}
