package agent

import (
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
	"os"
)

// Read legacy private JSON once; NewManager atomically replaces it with sealed
// state before serving requests. Never fall back when a sealed file cannot open.
func (m *Manager) load() error {
	decode := func(raw []byte) error {
		if err := json.Unmarshal(raw, &m.data); err != nil {
			return err
		}
		m.syncReleasedReservations()
		return nil
	}
	data, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var sealed string
	if err := json.Unmarshal(data, &sealed); err == nil {
		plain, err := credentials.Decrypt(m.key, sealed)
		if err != nil {
			return err
		}
		return decode(plain)
	}
	return decode(data)
}
func (m *Manager) save() error {
	if m.cfg.IngressOnly {
		return errors.New("independent ingress cannot write deployment inventory")
	}
	plain, err := json.Marshal(m.data)
	if err != nil {
		return err
	}
	sealed, err := credentials.Encrypt(m.key, plain)
	if err != nil {
		return err
	}
	return state.Save(m.path(), sealed)
}
