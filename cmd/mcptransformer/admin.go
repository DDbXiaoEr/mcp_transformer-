package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// manager tracks all managed servers and exposes the status/control HTTP APIs.
type manager struct {
	mu      sync.RWMutex
	servers map[string]*managedServer
	order   []string
}

func newManager(ctx context.Context, servers []ServerConfig) *manager {
	m := &manager{servers: map[string]*managedServer{}}
	for _, sc := range servers {
		m.servers[sc.Name] = newManagedServer(ctx, sc)
		m.order = append(m.order, sc.Name)
	}
	return m
}

func (m *manager) names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

func (m *manager) get(name string) (*managedServer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ms, ok := m.servers[name]
	return ms, ok
}

func (m *manager) closeAll() {
	for _, name := range m.names() {
		if ms, ok := m.get(name); ok {
			ms.stop()
		}
	}
}

func (m *manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	names := m.names()
	statuses := make([]serverStatus, 0, len(names))
	for _, name := range names {
		ms, _ := m.get(name)
		statuses = append(statuses, ms.status())
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": statuses})
}

type controlRequest struct {
	Action string `json:"action"`
	Server string `json:"server"`
}

type controlResult struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func (m *manager) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return
	}

	switch req.Action {
	case "reload", "stop", "start":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown action %q (valid: reload, stop, start)", req.Action)})
		return
	}

	var targets []string
	if req.Server == "" {
		targets = m.names()
	} else {
		if _, ok := m.get(req.Server); !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "server not found: " + req.Server})
			return
		}
		targets = []string{req.Server}
	}

	results := make([]controlResult, 0, len(targets))
	for _, name := range targets {
		ms, _ := m.get(name)
		var err error
		switch req.Action {
		case "reload":
			err = ms.reload()
		case "stop":
			ms.stop()
		case "start":
			err = ms.start()
		}
		res := controlResult{Name: name, Action: req.Action, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
