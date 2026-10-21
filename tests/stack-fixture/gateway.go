package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza/pkg/gateway"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Platform-only fixture: actual mTLS listener and PostgreSQL-backed atomic
// replay claims. This is not an application protocol or vendor acceptance test.
func fixtureGateway(root http.Handler, pool *pgxpool.Pool) http.Handler {
	address := os.Getenv("DEPLOY_GATEWAY_ADDR")
	if address == "" {
		return root
	}
	decode := func(name string) []byte {
		data, err := base64.StdEncoding.DecodeString(os.Getenv(name))
		if err != nil {
			panic("invalid fixture gateway credential")
		}
		return data
	}
	cert, err := tls.X509KeyPair(decode("DEPLOY_GATEWAY_CERT"), decode("DEPLOY_GATEWAY_KEY"))
	if err != nil {
		panic("invalid fixture gateway certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(decode("DEPLOY_GATEWAY_CLIENT_CA")) {
		panic("invalid fixture gateway CA")
	}
	cfg, err := gateway.ServerTLS(cert, roots)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS fixture_gateway_nonces(nonce text PRIMARY KEY,expires timestamptz NOT NULL)`); err != nil {
		panic("fixture gateway nonce schema unavailable")
	}
	authorities := strings.Split(os.Getenv("DEPLOY_GATEWAY_AUTHORITIES"), ",")
	verify, err := gateway.Verify(gateway.Policy{Peers: strings.Split(os.Getenv("DEPLOY_GATEWAY_PEER_PINS"), ","), Delegates: strings.Split(os.Getenv("DEPLOY_GATEWAY_DELEGATE_PINS"), ","), Authorize: func(_ context.Context, authority string) error {
		if !slices.Contains(authorities, authority) {
			return fmt.Errorf("unknown fixture gateway authority")
		}
		return nil
	}, Claim: func(ctx context.Context, key string, ttl time.Duration) (bool, error) {
		tag, err := pool.Exec(ctx, `INSERT INTO fixture_gateway_nonces(nonce,expires) VALUES($1,now()+$2*interval '1 millisecond') ON CONFLICT(nonce) DO UPDATE SET expires=EXCLUDED.expires WHERE fixture_gateway_nonces.expires<now()`, key, ttl.Milliseconds())
		return tag.RowsAffected() == 1, err
	}})
	if err != nil {
		panic(err)
	}
	protected := verify(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gateway.RequireTLS13(r) {
			http.Error(w, "original TLS1.3 required", 403)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			http.Error(w, "body unavailable", 400)
			return
		}
		fmt.Fprint(w, "gateway-accepted:"+string(data))
	}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gateway-protected" {
			protected.ServeHTTP(w, r)
			return
		}
		root.ServeHTTP(w, r)
	})
	if os.Getenv("FIXTURE_GATEWAY_FAIL") != "true" {
		go func() {
			server := &http.Server{Addr: address, Handler: handler, TLSConfig: cfg, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
			if server.ListenAndServeTLS("", "") != nil {
				panic("fixture private TLS listener failed")
			}
		}()
	}
	return handler
}
