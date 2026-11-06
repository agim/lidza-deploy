package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAndIngressRolesCannotBeCombined(t *testing.T) {
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("x", 64))
	file := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(file, []byte(`{"ingress_only":true,"listen":"127.0.0.1:9095","proxy_listen":"127.0.0.1:18085","native":{"root":"/home/deploy","user":"deploy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(file); err == nil {
		t.Fatal("combined native/ingress roles accepted")
	}
	cfg := Config{IngressOnly: true, Native: &NativeHostConfig{Root: t.TempDir(), User: "agim"}}
	if reader, err := NewIngressReader(context.Background(), cfg); err == nil {
		reader.Close()
		t.Fatal("native host initialized as an ingress reader")
	}
}
