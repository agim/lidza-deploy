package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/gateway"
)

const ingressSnapshotLimit = 128 << 20

// NewIngressReader serves authenticated routing snapshots without becoming a
// second deployment manager. It never creates keys, starts jobs, reconciles
// containers, writes inventory, or runs host maintenance. Protected requests
// retain the shared, flock-protected replay journal used by the existing agent.
func NewIngressReader(parent context.Context, cfg Config) (*Manager, error) {
	if err := cfg.validateHostnames(); err != nil {
		return nil, err
	}
	if !cfg.IngressOnly || cfg.Native != nil {
		return nil, errors.New("ingress reader requires ingress_only configuration")
	}
	if !validateIngressListen(cfg.Listen) || !validateIngressListen(cfg.ProxyListen) {
		return nil, errors.New("independent ingress listeners must use explicit loopback addresses")
	}
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{cfg: cfg, ctx: ctx, cancel: cancel, hostCache: map[string]hostVerdict{}, hostSlots: make(chan struct{}, 8), gatewayClients: map[string]*gateway.Transport{}}
	key, err := credentials.Key(cfg.DataDir)
	if err != nil {
		cancel()
		return nil, errors.New("ingress requires the retained agent encryption key")
	}
	m.key = key
	if err := m.reloadIngressSnapshot(); err != nil {
		cancel()
		return nil, err
	}
	if err := m.initGateway(); err != nil {
		cancel()
		return nil, err
	}
	m.wg.Add(1)
	go m.ingressSnapshotLoop()
	return m, nil
}

func (m *Manager) reloadIngressSnapshot() error {
	file, err := os.Open(m.path())
	if err != nil {
		return errors.New("routing snapshot is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, ingressSnapshotLimit+1))
	if err != nil || len(data) > ingressSnapshotLimit {
		return errors.New("routing snapshot is unavailable or oversized")
	}
	var sealed string
	if json.Unmarshal(data, &sealed) != nil || sealed == "" {
		return errors.New("ingress requires an authenticated encrypted routing snapshot")
	}
	plain, err := credentials.Decrypt(m.key, sealed)
	if err != nil {
		return errors.New("routing snapshot authentication failed")
	}
	var next diskState
	if json.Unmarshal(plain, &next) != nil || next.Apps == nil {
		return errors.New("routing snapshot is invalid")
	}
	for id, app := range next.Apps {
		if id != app.ID || app.Validate() != nil {
			return errors.New("routing snapshot contains an invalid application")
		}
		if err := m.checkReservedDomains(app); err != nil {
			return err
		}
		if app.Current != nil && app.Current.Stack != nil && app.Current.Stack.Gateway != nil {
			// An automatic gateway is served by the full agent itself
			// (gateway_auto.go); this reader's proxy still refuses
			// protected paths without gateway evidence.
			if m.cfg.Gateway == nil && next.HostGateway == nil {
				return errors.New("protected releases require the retained host gateway configuration")
			}
			identity := app.Current.Stack.Gateway.Identity
			pki, found := next.GatewayPKI[identity]
			if !found || pki.validate() != nil {
				return errors.New("routing snapshot lacks a valid retained protected gateway identity")
			}
		}
	}
	if err := m.addNativeIngressRoutes(&next); err != nil {
		return err
	}
	m.mu.Lock()
	m.gatewayMu.Lock()
	for identity, transport := range m.gatewayClients {
		if previous, ok := m.gatewayKeys[identity]; !ok || previous != next.GatewayPKI[identity] {
			transport.CloseIdleConnections()
			delete(m.gatewayClients, identity)
		}
	}
	m.gatewayKeys = maps.Clone(next.GatewayPKI)
	m.data = next
	m.syncReleasedReservations()
	m.ingressError = nil
	m.gatewayMu.Unlock()
	m.mu.Unlock()
	return nil
}

func (m *Manager) ingressSnapshotLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last os.FileInfo
	for {
		info, err := os.Stat(m.path())
		if err != nil || last == nil || !os.SameFile(last, info) || !info.ModTime().Equal(last.ModTime()) || info.Size() != last.Size() {
			if err == nil {
				err = m.reloadIngressSnapshot()
			}
			if err != nil {
				m.mu.Lock()
				m.ingressError = errors.New("routing snapshot is unavailable")
				m.mu.Unlock()
				last = nil
			} else {
				last = info
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) ingressAvailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ingressError == nil
}

// IngressHealth exposes health and the existing loopback certificate permission
// check. It deliberately exposes no management, credential or route data.
func IngressHealth(m *Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		JSON(w, 200, map[string]string{"status": "ok", "mode": "ingress_only"})
	})
	mux.Handle("GET /tls/allow", tlsAllowHandler(m))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.ingressAvailable() {
			Fail(w, 503, errors.New("routing snapshot is unavailable"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func validateIngressListen(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535 && ip != nil && ip.IsLoopback()
}
