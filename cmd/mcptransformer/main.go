package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen  string         `yaml:"listen"`
	Admin   AdminConfig    `yaml:"admin"`
	Servers []ServerConfig `yaml:"servers"`
}

type AdminConfig struct {
	Listen      string `yaml:"listen"`
	StatusPath  string `yaml:"statusPath"`
	ControlPath string `yaml:"controlPath"`
}

type ServerConfig struct {
	Name            string            `yaml:"name"`
	Endpoint        string            `yaml:"endpoint"`
	Version         string            `yaml:"version"`
	ProtocolVersion string            `yaml:"protocolVersion"`
	Command         string            `yaml:"command"`
	Args            []string          `yaml:"args"`
	Env             map[string]string `yaml:"env"`
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file (defaults to config.yaml in the working directory)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	seen := map[string]bool{}
	for _, sc := range cfg.Servers {
		if err := validateServer(&sc); err != nil {
			log.Fatalf("invalid server config %q: %v", sc.Name, err)
		}
		if seen[sc.Endpoint] {
			log.Fatalf("duplicate endpoint: %s", sc.Endpoint)
		}
		seen[sc.Endpoint] = true
	}

	statusPath, controlPath := adminPaths(cfg.Admin)
	if err := validateAdminPath(statusPath, "admin.statusPath"); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if err := validateAdminPath(controlPath, "admin.controlPath"); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if statusPath == controlPath {
		log.Fatalf("admin.statusPath and admin.controlPath must differ")
	}

	mgr := newManager(ctx, cfg.Servers)

	mcpMux := http.NewServeMux()
	for _, name := range mgr.names() {
		ms, _ := mgr.get(name)
		mcpMux.Handle(ms.cfg.Endpoint, ms)
		if err := ms.start(); err != nil {
			log.Printf("skip server %q (%s): %v", ms.cfg.Name, ms.cfg.Endpoint, err)
			continue
		}
		log.Printf("mounted %q -> endpoint %s", ms.cfg.Name, ms.cfg.Endpoint)
	}

	adminMux := http.NewServeMux()
	adminMux.HandleFunc(statusPath, mgr.handleStatus)
	adminMux.HandleFunc(controlPath, mgr.handleControl)
	log.Printf("admin status API -> GET %s", statusPath)
	log.Printf("admin control API -> POST %s", controlPath)

	mcpAddr := cfg.Listen
	if mcpAddr == "" {
		mcpAddr = defaultListen
	}
	adminAddr := cfg.Admin.Listen
	if adminAddr == "" {
		adminAddr = defaultAdminListen
	}

	var servers []*http.Server
	if adminAddr == mcpAddr {
		// 同一地址时合并挂载，避免端口冲突。
		mcpMux.HandleFunc(statusPath, mgr.handleStatus)
		mcpMux.HandleFunc(controlPath, mgr.handleControl)
		servers = []*http.Server{{Addr: mcpAddr, Handler: mcpMux}}
		log.Printf("listening on %s (MCP + admin)", mcpAddr)
	} else {
		servers = []*http.Server{
			{Addr: mcpAddr, Handler: mcpMux},
			{Addr: adminAddr, Handler: adminMux},
		}
		log.Printf("listening on %s (MCP)", mcpAddr)
		log.Printf("listening on %s (admin)", adminAddr)
	}

	for _, srv := range servers {
		go func(srv *http.Server) {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("http server: %v", err)
			}
		}(srv)
	}

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}

	mgr.closeAll()
	log.Println("bye")
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no servers configured")
	}
	return &cfg, nil
}

func validateServer(sc *ServerConfig) error {
	if sc.Name == "" {
		return fmt.Errorf("name is required")
	}
	if sc.Command == "" {
		return fmt.Errorf("command is required")
	}
	if sc.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if !strings.HasPrefix(sc.Endpoint, "/") {
		return fmt.Errorf("endpoint must start with '/'")
	}
	if sc.ProtocolVersion != "" && !slices.Contains(mcp.ValidProtocolVersions, sc.ProtocolVersion) {
		return fmt.Errorf("unsupported protocolVersion %q (valid: %s)", sc.ProtocolVersion, strings.Join(mcp.ValidProtocolVersions, ", "))
	}
	return nil
}

const (
	defaultStatusPath  = "/__admin/status"
	defaultControlPath = "/__admin/control"
	defaultListen      = ":8080"
	defaultAdminListen = ":8081"
)

func adminPaths(a AdminConfig) (string, string) {
	statusPath := a.StatusPath
	if statusPath == "" {
		statusPath = defaultStatusPath
	}
	controlPath := a.ControlPath
	if controlPath == "" {
		controlPath = defaultControlPath
	}
	return statusPath, controlPath
}

func validateAdminPath(path, field string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%s must start with '/'", field)
	}
	return nil
}

func envList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

const (
	reconnectDelay    = 500 * time.Millisecond
	maxReconnectDelay = 5 * time.Second
	initializeTimeout = 10 * time.Second
)

var errUpstreamDown = errors.New("upstream MCP server is unavailable")

// proxy wraps a stdio MCP client whose underlying process may exit at any
// time. It owns a swappable *client.Client and a monitor goroutine that
// detects process death (stderr EOF) and transparently reconnects.
type proxy struct {
	name string
	cfg  ServerConfig

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.RWMutex
	client *client.Client
	closed bool

	wg sync.WaitGroup
}

func newProxy(ctx context.Context, sc ServerConfig) (*proxy, error) {
	pctx, cancel := context.WithCancel(ctx)
	p := &proxy{
		name:   sc.Name,
		cfg:    sc,
		ctx:    pctx,
		cancel: cancel,
	}
	if err := p.connect(pctx); err != nil {
		cancel()
		return nil, err
	}
	p.wg.Add(1)
	go p.monitor()
	return p, nil
}

// connect spawns a fresh stdio client and performs the MCP handshake,
// swapping it in as the current client on success.
func (p *proxy) connect(ctx context.Context) error {
	c, err := client.NewStdioMCPClient(p.cfg.Command, envList(p.cfg.Env), p.cfg.Args...)
	if err != nil {
		return fmt.Errorf("start stdio client: %w", err)
	}
	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	if _, err := c.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo: mcp.Implementation{Name: "mcptransformer", Version: "1.0.0"},
		},
	}); err != nil {
		_ = c.Close()
		return fmt.Errorf("initialize: %w", err)
	}
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
	return nil
}

func (p *proxy) current() *client.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.client
}

func (p *proxy) isClosed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.closed
}

// status reports the current connection state of the upstream process.
func (p *proxy) status() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return "stopped"
	}
	if p.client == nil {
		return "down"
	}
	return "running"
}

// monitor watches the current client's stderr. When the upstream process
// dies, stderr hits EOF, io.Copy returns, and we reconnect with backoff.
func (p *proxy) monitor() {
	defer p.wg.Done()
	delay := reconnectDelay
	for {
		c := p.current()
		if c == nil {
			return
		}
		stderr, ok := client.GetStderr(c)
		if !ok {
			return
		}
		_, _ = io.Copy(&namedWriter{name: p.name}, stderr)

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.client = nil
		p.mu.Unlock()
		_ = c.Close()

		if p.ctx.Err() != nil {
			return
		}
		log.Printf("[%s] upstream exited, reconnecting", p.name)
		for {
			if p.ctx.Err() != nil {
				return
			}
			if err := p.connect(p.ctx); err != nil {
				log.Printf("[%s] reconnect failed: %v (retrying in %s)", p.name, err, delay)
				if !sleepOrDone(p.ctx, delay) {
					return
				}
				if delay < maxReconnectDelay {
					delay *= 2
				}
				continue
			}
			delay = reconnectDelay
			log.Printf("[%s] reconnected", p.name)
			break
		}
	}
}

func (p *proxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	c := p.client
	p.client = nil
	p.mu.Unlock()

	p.cancel()
	if c != nil {
		_ = c.Close()
	}
	p.wg.Wait()
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *proxy) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.ListTools(ctx, req)
}

func (p *proxy) ListResources(ctx context.Context, req mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.ListResources(ctx, req)
}

func (p *proxy) ListResourceTemplates(ctx context.Context, req mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.ListResourceTemplates(ctx, req)
}

func (p *proxy) ListPrompts(ctx context.Context, req mcp.ListPromptsRequest) (*mcp.ListPromptsResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.ListPrompts(ctx, req)
}

func (p *proxy) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.CallTool(ctx, req)
}

func (p *proxy) ReadResource(ctx context.Context, req mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.ReadResource(ctx, req)
}

func (p *proxy) GetPrompt(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	c := p.current()
	if c == nil {
		return nil, errUpstreamDown
	}
	return c.GetPrompt(ctx, req)
}

// runtime is the fully-built, swappable serving unit for one upstream server:
// the stdio proxy plus the StreamableHTTPServer handler registered with it.
type runtime struct {
	proxy   *proxy
	handler http.Handler
}

func buildRuntime(ctx context.Context, sc ServerConfig) (*runtime, error) {
	upstream, err := newProxy(ctx, sc)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*runtime, error) {
		upstream.close()
		return nil, err
	}

	version := sc.Version
	if version == "" {
		version = "1.0.0"
	}

	opts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, false),
		server.WithPromptCapabilities(true),
	}

	if sc.ProtocolVersion != "" {
		hooks := &server.Hooks{}
		hooks.AddAfterInitialize(func(ctx context.Context, id any, msg *mcp.InitializeRequest, result *mcp.InitializeResult) {
			result.ProtocolVersion = sc.ProtocolVersion
		})
		opts = append(opts, server.WithHooks(hooks))
	}

	s := server.NewMCPServer(sc.Name, version, opts...)

	if err := registerTools(ctx, upstream, s); err != nil {
		return fail(err)
	}
	if err := registerResources(ctx, upstream, s); err != nil {
		return fail(err)
	}
	if err := registerPrompts(ctx, upstream, s); err != nil {
		return fail(err)
	}

	handler := server.NewStreamableHTTPServer(s, server.WithEndpointPath(sc.Endpoint))
	return &runtime{proxy: upstream, handler: handler}, nil
}

func registerTools(ctx context.Context, upstream *proxy, s *server.MCPServer) error {
	result, err := upstream.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	for _, tool := range result.Tools {
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return upstream.CallTool(ctx, mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      req.Params.Name,
					Arguments: req.Params.Arguments,
				},
			})
		})
	}
	log.Printf("  registered %d tool(s)", len(result.Tools))
	return nil
}

func registerResources(ctx context.Context, upstream *proxy, s *server.MCPServer) error {
	resources, err := upstream.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		log.Printf("  list resources skipped: %v", err)
	} else {
		for _, resource := range resources.Resources {
			s.AddResource(resource, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				res, err := upstream.ReadResource(ctx, mcp.ReadResourceRequest{Params: req.Params})
				if err != nil {
					return nil, err
				}
				return res.Contents, nil
			})
		}
		log.Printf("  registered %d resource(s)", len(resources.Resources))
	}

	templates, err := upstream.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		log.Printf("  list resource templates skipped: %v", err)
		return nil
	}
	for _, template := range templates.ResourceTemplates {
		s.AddResourceTemplate(template, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			res, err := upstream.ReadResource(ctx, mcp.ReadResourceRequest{Params: req.Params})
			if err != nil {
				return nil, err
			}
			return res.Contents, nil
		})
	}
	log.Printf("  registered %d resource template(s)", len(templates.ResourceTemplates))
	return nil
}

func registerPrompts(ctx context.Context, upstream *proxy, s *server.MCPServer) error {
	result, err := upstream.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		log.Printf("  list prompts skipped: %v", err)
		return nil
	}
	for _, prompt := range result.Prompts {
		s.AddPrompt(prompt, func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return upstream.GetPrompt(ctx, mcp.GetPromptRequest{Params: req.Params})
		})
	}
	log.Printf("  registered %d prompt(s)", len(result.Prompts))
	return nil
}

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
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Command  string   `json:"command"`
	Args     []string `json:"args,omitempty"`
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
}

func (m *managedServer) status() serverStatus {
	m.mu.RLock()
	rt := m.runtime
	lastErr := m.lastErr
	m.mu.RUnlock()

	st := serverStatus{
		Name:     m.cfg.Name,
		Endpoint: m.cfg.Endpoint,
		Command:  m.cfg.Command,
		Args:     m.cfg.Args,
	}
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

type namedWriter struct {
	name string
}

func (w *namedWriter) Write(p []byte) (int, error) {
	log.Printf("[%s/stderr] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
