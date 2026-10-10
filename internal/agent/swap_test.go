package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSwapStatusAndExpiredOperation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	save := func(s SwapStatus) {
		t.Helper()
		b, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(m.cfg.DataDir, "swap-status.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save(SwapStatus{State: "queued", Message: "check queued", CheckedAt: time.Now().Unix()})
	if !m.upgradePending() {
		t.Fatal("concurrent update allowed during swap check")
	}
	save(SwapStatus{State: "queued", CheckedAt: time.Now().Add(-16 * time.Minute).Unix()})
	os.MkdirAll(filepath.Join(m.cfg.DataDir, "upgrade"), 0700)
	os.WriteFile(filepath.Join(m.cfg.DataDir, "upgrade/request"), []byte("swap-check\n"), 0600)
	m.expireSwapRequest()
	if m.upgradePending() || m.swapStatus().State != "failed" {
		t.Fatal("stale request blocked all future deployments")
	}
	os.WriteFile(filepath.Join(m.cfg.DataDir, "upgrade/request"), []byte("v0.2.18\n"), 0600)
	m.expireSwapRequest()
	if b, _ := os.ReadFile(filepath.Join(m.cfg.DataDir, "upgrade/request")); string(b) != "v0.2.18\n" {
		t.Fatal("expired check deleted an unrelated upgrade")
	}
	os.Remove(filepath.Join(m.cfg.DataDir, "upgrade/request"))
	save(SwapStatus{State: "ready", Message: "Existing active swap preserved.", CheckedAt: time.Now().Unix()})
	if m.swapStatus().Message != "Existing active swap preserved." || m.upgradePending() {
		t.Fatal("ready state lost")
	}
	m.queueSwapCheck()
	if _, err := os.Stat(filepath.Join(m.cfg.DataDir, "upgrade/request")); !os.IsNotExist(err) {
		t.Fatal("unsupported development agent queued root swap action")
	}
}
