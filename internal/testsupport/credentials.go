// Package testsupport holds fixtures shared by Deploy's test packages.
package testsupport

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/agim/lidza/pkg/credentials"
)

// Run supplies a fresh, package-local key when lidza test deliberately disables
// operator credentials. Never enable key-file reads or reuse an operator key.
func Run(m *testing.M) int {
	if os.Getenv(credentials.EnvKeyOff) == "1" && os.Getenv(credentials.EnvMasterKey) == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			fmt.Fprintln(os.Stderr, "test credential generation failed:", err)
			return 1
		}
		if err := os.Setenv(credentials.EnvMasterKey, hex.EncodeToString(key)); err != nil {
			fmt.Fprintln(os.Stderr, "test credential setup failed:", err)
			return 1
		}
	}
	return m.Run()
}

// Key installs a new fixture key after a test intentionally clears its settings.
func Key(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentials.EnvMasterKey, hex.EncodeToString(key))
}
