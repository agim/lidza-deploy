package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
	"golang.org/x/sys/unix"
)

// The agent is a host-local service, not a replicated framework application.
// A separate encrypted, fsynced journal plus flock shares claims across verifier
// instances on this host and retains them through restarts. It never expands
// deployment history or silently falls back to independent in-memory claims.
func (m *Manager) claimGatewayNonce(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if len(key) > 80 || ttl <= 0 || ttl > time.Minute {
		return false, errors.New("invalid gateway claim")
	}
	lock, err := os.OpenFile(filepath.Join(m.cfg.DataDir, "gateway-nonces.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path := filepath.Join(m.cfg.DataDir, "gateway-nonces.json")
	claims := map[string]int64{}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Size() > 2<<20) {
		return false, errors.New("gateway replay journal is invalid or oversized")
	} else if err != nil && !os.IsNotExist(err) {
		return false, errors.New("gateway replay journal unavailable")
	}
	var sealed string
	if err := state.Load(path, &sealed); err != nil {
		return false, errors.New("gateway replay journal unavailable")
	}
	if sealed != "" {
		plain, err := credentials.Decrypt(m.key, sealed)
		if err != nil {
			return false, errors.New("gateway replay journal could not be authenticated")
		}
		if json.Unmarshal(plain, &claims) != nil || claims == nil {
			return false, errors.New("gateway replay journal is invalid")
		}
	}
	now := time.Now()
	for k, until := range claims {
		if until <= now.UnixMilli() {
			delete(claims, k)
		}
	}
	if _, exists := claims[key]; exists {
		return false, nil
	}
	if len(claims) >= 8192 {
		return false, errors.New("gateway replay journal capacity reached")
	}
	claims[key] = now.Add(ttl).UnixMilli()
	plain, err := json.Marshal(claims)
	if err != nil {
		return false, err
	}
	sealed, err = credentials.Encrypt(m.key, plain)
	if err != nil {
		return false, err
	}
	if err := state.Save(path, sealed); err != nil {
		return false, err
	}
	return true, nil
}
