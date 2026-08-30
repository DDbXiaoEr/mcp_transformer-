package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	Name     string            `yaml:"name"`
	Endpoint string            `yaml:"endpoint"`
	Version  string            `yaml:"version"`
	Command  string            `yaml:"command"`
	Args     []string          `yaml:"args"`
	Env      map[string]string `yaml:"env"`
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
	var upstreams []*client.Client
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
		_ = u.Close()
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

func buildProxy(ctx context.Context, sc ServerConfig) (*client.Client, *server.StreamableHTTPServer, error) {
	upstream, err := client.NewStdioMCPClient(sc.Command, envList(sc.Env), sc.Args...)
	if err != nil {
		return nil, nil, fmt.Errorf("start stdio client: %w", err)
	}
	fail := func(err error) (*client.Client, *server.StreamableHTTPServer, error) {
		_ = upstream.Close()
		return nil, nil, err
	}

	if _, err := upstream.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo: mcp.Implementation{Name: "mcptransformer", Version: "1.0.0"},
		},
	}); err != nil {
		return fail(fmt.Errorf("initialize: %w", err))
	}

	go pipeStderr(sc.Name, upstream)

	version := sc.Version
	if version == "" {
		version = "1.0.0"
	}

	s := server.NewMCPServer(sc.Name, version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, false),
		server.WithPromptCapabilities(true),
	)

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

func registerTools(ctx context.Context, upstream *client.Client, s *server.MCPServer) error {
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

func registerResources(ctx context.Context, upstream *client.Client, s *server.MCPServer) error {
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

func registerPrompts(ctx context.Context, upstream *client.Client, s *server.MCPServer) error {
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

func pipeStderr(name string, c *client.Client) {
	r, ok := client.GetStderr(c)
	if !ok {
		return
	}
	_, _ = io.Copy(&namedWriter{name: name}, r)
}

type namedWriter struct {
	name string
}

func (w *namedWriter) Write(p []byte) (int, error) {
	log.Printf("[%s/stderr] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
