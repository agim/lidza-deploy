package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
)

// Provision only the per-app auth signing secret. Master keys and provider
// credentials remain operator supplied. Never rotate a supplied signing secret.
func provisionAuthSecret(a App) (App, bool, error) {
	if a.Env["AUTH_SECRET"] != "" {
		return a, false, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return a, false, errors.New("could not generate application authentication secret")
	}
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["AUTH_SECRET"] = hex.EncodeToString(key)
	if err := a.Validate(); err != nil {
		return a, false, err
	}
	return a, true, nil
}
