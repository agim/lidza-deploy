package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCachePreflightRetainsResourceFailureInDeploymentDiagnostics(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	c := CacheResource{Mode: "local", Ready: true, LocalPassword: "private-cache-password", URL: "redis://:private-cache-password@managed-cache:6379/0"}
	a := testApp("cache-budget-error")
	a.Env["CACHE_URL"] = c.URL
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.data.Caches[a.ID] = c
	m.mu.Unlock()
	m.cacheProvision = func(context.Context, CacheResource) error {
		return cacheFailure("host CPU/RAM budget", errors.New("RAM budgets reserve 10000 MiB but this host has 8000 MiB"), c)
	}
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), diagnosticKey{}, func(line string) { logs.WriteString(line) })
	_, err := m.preflightRuntime(ctx, a, cacheManifest(t, `["lidza/cache"]`))
	if err == nil || !strings.Contains(err.Error(), "RAM budgets reserve") || !strings.Contains(logs.String(), "Server → Resources") || strings.Contains(logs.String(), c.LocalPassword) {
		t.Fatal("cache preflight lost the cause or exposed credentials", err, logs.String())
	}
}

func TestCacheFailuresExposeBudgetReasonAndRedactCredentials(t *testing.T) {
	c := CacheResource{LocalPassword: "private-password-value", URL: "redis://:private-password-value@cache.example.com:6379/0"}
	budget := publicCacheFailure(cacheFailure("host CPU/RAM budget", errors.New("RAM budgets reserve 10000 MiB but this host has 8000 MiB"), c))
	if !strings.Contains(budget.Error(), "10000 MiB") || !strings.Contains(budget.Error(), "Server → Resources") {
		t.Fatal("resource cause or repair location hidden", budget)
	}
	service := publicCacheFailure(cacheFailure("Valkey provisioning/readiness", errors.New("connection failed "+c.URL+" password="+c.LocalPassword), c))
	if strings.Contains(service.Error(), c.URL) || strings.Contains(service.Error(), c.LocalPassword) || !strings.Contains(service.Error(), "Valkey provisioning/readiness") {
		t.Fatal("unsafe or missing service diagnostic", service)
	}
	unknown := publicCacheFailure(errors.New("unclassified private-password-value"))
	if strings.Contains(unknown.Error(), "private-password-value") {
		t.Fatal("unclassified error exposed")
	}
}
