package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

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
