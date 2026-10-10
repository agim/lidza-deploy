package agent

import (
	"net/url"
	"strings"
	"testing"
)

func FuzzAppEnvironment(f *testing.F) {
	f.Add("DATABASE_URL", "postgres://user:password@database/app")
	f.Add("LIDZA_ADDR", "0.0.0.0:3000")
	f.Add("APP_SECRET", "value\x00injected")
	f.Fuzz(func(t *testing.T, key, value string) {
		a := testApp("fuzz")
		a.Env = map[string]string{key: value}
		if a.Validate() == nil {
			if !envPattern.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") {
				t.Fatal("accepted unsafe Docker environment")
			}
			if strings.HasPrefix(key, "LIDZA_") && key != "LIDZA_MASTER_KEY" {
				t.Fatal("accepted agent-owned environment")
			}
		}
	})
}

func FuzzCacheConnection(f *testing.F) {
	f.Add("redis://:password@cache.example.com:6379/0")
	f.Add("redis://:password@127.0.0.1:6379/0")
	f.Add("rediss://:password@cache.example.com:6379/0\n")
	f.Fuzz(func(t *testing.T, address string) {
		err := (CacheRequest{Mode: "external", URL: address}).Validate()
		if err != nil {
			return
		}
		u, err := url.Parse(address)
		if err != nil || u.User == nil || u.Hostname() == "" || strings.ContainsAny(address, "\r\n\x00") {
			t.Fatal("accepted unsafe cache address")
		}
		if password, ok := u.User.Password(); !ok || password == "" {
			t.Fatal("accepted unauthenticated cache")
		}
	})
}
