package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func appFixture(t *testing.T) App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return App{ID: 42, Slug: "deploy-fixture", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), WebhookSecret: "test-webhook-secret"}
}
func TestAppInstallationTokensNarrowScopeRenewAndFailClosed(t *testing.T) {
	app := appFixture(t)
	minted := 0
	revoked := false
	expired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Error("missing signed App JWT")
		}
		switch r.URL.Path {
		case "/app/installations/7":
			if revoked {
				http.Error(w, "revoked", http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42, "account": map[string]string{"login": "acme"}, "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
		case "/repos/acme/private/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42})
		case "/app/installations/7/access_tokens":
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Repositories) != 1 || body.Repositories[0] != "private" || body.Permissions["contents"] != "read" || len(body.Permissions) != 2 {
				t.Error("broad token permissions")
			}
			minted++
			expires := time.Now().Add(time.Hour)
			if expired {
				expires = time.Now().Add(-time.Minute)
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "ephemeral-token", "expires_at": expires})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := Client{API: server.URL}
	for i := 0; i < 2; i++ {
		if token, err := c.InstallationToken(context.Background(), app, 7, "acme/private"); err != nil || token != "ephemeral-token" {
			t.Fatal("mint failed", err)
		}
	}
	if minted != 2 {
		t.Fatal("a later checkout reused an old token")
	}
	expired = true
	if _, err := c.InstallationToken(context.Background(), app, 7, "acme/private"); err == nil {
		t.Fatal("expired credential accepted")
	}
	expired = false
	revoked = true
	if _, err := c.InstallationToken(context.Background(), app, 7, "acme/private"); err == nil {
		t.Fatal("revoked installation accepted")
	}
}

func TestAppJWTSignatureAndLifetime(t *testing.T) {
	app := appFixture(t)
	token, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid JWT structure")
	}
	block, _ := pem.Decode([]byte(app.PEM))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatal("invalid RS256 signature", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iss      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if claims.Iss != "42" || claims.Iat > now || claims.Iat < now-120 || claims.Exp <= now || claims.Exp > now+600 {
		t.Fatal("invalid GitHub issuer or JWT validity interval")
	}
}
