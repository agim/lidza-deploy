package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

func validateCheckoutCredential(in DeployRequest) error {
	if in.CredentialURL == "" {
		if in.CredentialTicket != "" || in.CredentialServer != "" {
			return errors.New("incomplete checkout authorization")
		}
		return nil
	}
	u, err := url.Parse(in.CredentialURL)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "/api/agent/checkout-token" || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) || len(in.CredentialTicket) < 32 || len(in.CredentialTicket) > 512 || !idPattern.MatchString(in.CredentialServer) || in.Token != "" {
		return errors.New("invalid checkout authorization")
	}
	return nil
}
func (m *Manager) checkoutCredential(ctx context.Context, in DeployRequest) (string, error) {
	if err := validateCheckoutCredential(in); err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"ticket": in.CredentialTicket})
	r, err := http.NewRequestWithContext(ctx, "POST", in.CredentialURL, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("invalid checkout authorization")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	r.Header.Set("X-Lidza-Server", in.CredentialServer)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(r)
	if err != nil {
		return "", errors.New("control panel unavailable for GitHub checkout authorization")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", errors.New("GitHub checkout authorization refused; check installation access and redeploy")
	}
	var out struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&out) != nil || out.Token == "" || len(out.Token) > 4096 {
		return "", errors.New("invalid checkout credential response")
	}
	return out.Token, nil
}
