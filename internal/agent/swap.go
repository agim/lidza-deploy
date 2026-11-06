package agent

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"time"
)

type SwapStatus struct {
	State     string `json:"state"`
	Message   string `json:"message"`
	CheckedAt int64  `json:"checked_at"`
}

func (m *Manager) swapStatus() SwapStatus {
	var s SwapStatus
	if root, err := os.OpenRoot(m.cfg.DataDir); err == nil {
		defer root.Close()
		_ = readSSHFile(root, "swap-status.json", &s)
	}
	if (s.State == "queued" || s.State == "applying") && s.CheckedAt > 0 && time.Since(time.Unix(s.CheckedAt, 0)) > 15*time.Minute {
		s.State = "failed"
		s.Message = "Automatic swap check timed out; inspect lidza-agent-upgrade.service or rerun the installer."
	}
	return s
}

// Existing agents upgraded by the older CLI acquire the new helper after their
// restart. Wait for that capability rather than sending an unsupported command.
func (m *Manager) swapLoop() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		m.queueSwapCheck()
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (m *Manager) queueSwapCheck() {
	if runtime.GOOS != "linux" || m.cfg.DataDir != "/var/lib/lidza-agent" {
		return
	}
	helper, err := os.ReadFile("/usr/local/libexec/lidza-agent-upgrade")
	if err != nil || !strings.Contains(string(helper), "# lidza-swap-v1") {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireSwapRequest()
	if m.upgradePending() {
		return
	}
	s := m.swapStatus()
	if s.CheckedAt > 0 && time.Since(time.Unix(s.CheckedAt, 0)) < 24*time.Hour {
		return
	}
	root, err := os.OpenRoot(m.cfg.DataDir)
	if err != nil {
		return
	}
	defer root.Close()
	if root.MkdirAll("upgrade", 0700) != nil {
		return
	}
	// Mark acceptance to avoid retry storms if a root unit is unavailable.
	pending, _ := json.Marshal(SwapStatus{State: "queued", Message: "Automatic swap check queued", CheckedAt: time.Now().Unix()})
	if writeSSHFile(root, "swap-status.json", pending, 0600) != nil {
		return
	}
	if err = writeSSHFile(root, "upgrade/request", []byte("swap-check\n"), 0600); err != nil {
		root.Remove("swap-status.json")
	}
}

// A stopped path unit must not leave an automatic check blocking every manual
// deployment indefinitely. Only our exact expired request may be removed.
func (m *Manager) expireSwapRequest() {
	s := m.swapStatus()
	if s.State != "failed" || s.CheckedAt <= 0 || time.Since(time.Unix(s.CheckedAt, 0)) <= 15*time.Minute {
		return
	}
	root, err := os.OpenRoot(m.cfg.DataDir)
	if err != nil {
		return
	}
	defer root.Close()
	f, err := root.Open("upgrade/request")
	if err != nil {
		return
	}
	var b [32]byte
	n, e := f.Read(b[:])
	f.Close()
	if e == nil && string(b[:n]) == "swap-check\n" {
		_ = root.Remove("upgrade/request")
	}
}
