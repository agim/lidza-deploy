package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
)

// Called only for new app identities. Never backfill a running installation:
// its original encryption key may protect data not visible in the checkout.
func provisionMasterKey(a App) (App, error) {
	if a.Env["LIDZA_MASTER_KEY"] != "" {
		return a, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return a, errors.New("could not generate application master key")
	}
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["LIDZA_MASTER_KEY"] = hex.EncodeToString(key)
	a.EnvSources = maps.Clone(a.EnvSources)
	if a.EnvSources == nil {
		a.EnvSources = map[string]string{}
	}
	a.EnvSources["LIDZA_MASTER_KEY"] = "built-in"
	return a, nil
}
