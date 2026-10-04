package agent

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyEnvironmentMigratesToEncryptedState(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir}
	legacy := diskState{Apps: map[string]App{"one": testApp("one")}}
	if err := state.Save(filepath.Join(dir, "state.json"), legacy); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	data, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "runtime-secret") || strings.Contains(string(data), "APP_SECRET") {
		t.Fatal("plaintext environment retained")
	}
	info, _ := os.Stat(m.path())
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe state permissions")
	}
	key, err := credentials.Key(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err = json.Unmarshal(data, &sealed); err != nil {
		t.Fatal(err)
	}
	plain, err := credentials.Decrypt(key, sealed)
	if err != nil || !strings.Contains(string(plain), "runtime-secret") {
		t.Fatal("migration lost secret", err)
	}
	reopened, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	reopened.key = make([]byte, 32)
	if err = reopened.load(); err == nil {
		t.Fatal("wrong key accepted")
	}
}
