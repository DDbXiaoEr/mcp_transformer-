package main

import (
	"context"
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
	Servers []ServerConfig `yaml:"servers"`
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

	mux := http.NewServeMux()
	var upstreams []*proxy
	seen := map[string]bool{}

	for _, sc := range cfg.Servers {
		if err := validateServer(&sc); err != nil {
			log.Fatalf("invalid server config %q: %v", sc.Name, err)
		}
		if seen[sc.Endpoint] {
			log.Fatalf("duplicate endpoint: %s", sc.Endpoint)
		}
		seen[sc.Endpoint] = true

		upstream, handler, err := buildProxy(ctx, sc)
		if err != nil {
			log.Printf("skip server %q (%s): %v", sc.Name, sc.Endpoint, err)
			continue
		}
		upstreams = append(upstreams, upstream)
		mux.Handle(sc.Endpoint, handler)
		log.Printf("mounted %q -> endpoint %s", sc.Name, sc.Endpoint)
	}

	addr := cfg.Listen
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	for _, u := range upstreams {
		u.close()
	}
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

func buildProxy(ctx context.Context, sc ServerConfig) (*proxy, *server.StreamableHTTPServer, error) {
	upstream, err := newProxy(ctx, sc)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*proxy, *server.StreamableHTTPServer, error) {
		upstream.close()
		return nil, nil, err
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
	return upstream, handler, nil
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

type namedWriter struct {
	name string
}

func (w *namedWriter) Write(p []byte) (int, error) {
	log.Printf("[%s/stderr] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
