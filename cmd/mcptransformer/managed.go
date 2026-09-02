package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// managedServer owns a single upstream server across its lifecycle. It holds a
// swappable runtime (proxy + HTTP handler) and implements http.Handler so the
// mux can keep a stable registration while the underlying handler is swapped
// out by reload/stop/start operations.
type managedServer struct {
	cfg ServerConfig
	ctx context.Context

	mu      sync.RWMutex
	runtime *runtime
	lastErr error
}

func newManagedServer(ctx context.Context, sc ServerConfig) *managedServer {
	return &managedServer{cfg: sc, ctx: ctx}
}

func (m *managedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	rt := m.runtime
	m.mu.RUnlock()
	if rt == nil {
		http.Error(w, "upstream MCP server is stopped", http.StatusServiceUnavailable)
		return
	}
	rt.handler.ServeHTTP(w, r)
}

// start builds a fresh runtime and swaps it in, but only when the server is
// currently stopped. Use reload to force a rebuild of a running server.
func (m *managedServer) start() error {
	m.mu.RLock()
	running := m.runtime != nil
	m.mu.RUnlock()
	if running {
		return fmt.Errorf("server %q is already running", m.cfg.Name)
	}
	return m.reload()
}

// reload rebuilds the upstream connection and re-enumerates capabilities,
// then atomically swaps in the new handler and tears down the old one.
func (m *managedServer) reload() error {
	rt, err := buildRuntime(m.ctx, m.cfg)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	old := m.runtime
	m.runtime = rt
	m.lastErr = nil
	m.mu.Unlock()

	if old != nil {
		old.proxy.close()
	}
	return nil
}

func (m *managedServer) stop() {
	m.mu.Lock()
	old := m.runtime
	m.runtime = nil
	m.lastErr = nil
	m.mu.Unlock()

	if old != nil {
		old.proxy.close()
	}
}

type serverStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (m *managedServer) status() serverStatus {
	m.mu.RLock()
	rt := m.runtime
	lastErr := m.lastErr
	m.mu.RUnlock()

	st := serverStatus{Name: m.cfg.Name}
	if rt == nil {
		st.Status = "stopped"
		if lastErr != nil {
			st.Status = "error"
		}
	} else {
		st.Status = rt.proxy.status()
	}
	if lastErr != nil {
		st.Error = lastErr.Error()
	}
	return st
}
