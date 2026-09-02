package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

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

type namedWriter struct {
	name string
}

func (w *namedWriter) Write(p []byte) (int, error) {
	log.Printf("[%s/stderr] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
