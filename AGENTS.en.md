# AGENTS.md

[中文](./AGENTS.md)

## Project overview

`mcptransformer` is a Go program that bridges multiple stdio MCP servers into a Streamable HTTP service. It depends on `github.com/mark3labs/mcp-go` and `gopkg.in/yaml.v3`.

## Build & verify

```bash
make            # builds build/mcptransformer and build/listfiles, and copies config.yaml into build/
make vet        # go vet
make test       # go test ./...
make clean
```

After changing code, always run `go build ./...` (or `make`) to confirm compilation and run `go vet ./...`.

## Exploration constraints

- The project structure is captured in `PROJECT_STRUCTURE.md`. Before making changes, read that file and this document to locate the target file; do not re-explore the entire repository on every task (avoid repeated whole-repo glob/grep).
- Only re-explore and update `PROJECT_STRUCTURE.md` when the directory layout, file responsibilities, or dependencies change.

## Directory conventions

- `cmd/mcptransformer/` — the bridge service main program, split by responsibility into multiple files of the same package (all `package main`):
  - `main.go` — entry point: loads/validates config, mounts business and admin muxes, graceful shutdown.
  - `config.go` — config structs, loading and validation, default path/port constants.
  - `proxy.go` — upstream stdio client wrapper (death detection + exponential-backoff reconnect) and forwarding methods.
  - `runtime.go` — a fully swappable unit for one upstream: proxy + StreamableHTTPServer handler, and capability enumeration registration.
  - `managed.go` — `managedServer` lifecycle wrapper (start/reload/stop/status).
  - `admin.go` — `manager` plus the status/control admin HTTP API and JSON helpers.
- `cmd/listfiles/main.go` — test stdio MCP server exposing the `list_files` tool to list directory files.
- `config.yaml` — example config; each `servers` entry maps to one HTTP `endpoint`.
- Binaries and config are output to `build/`, which is ignored by `.gitignore`.
- Default config path: `config.yaml` in the working directory (overridable via `--config`).

## Key implementation notes

- Upstream connection: `client.NewStdioMCPClient(command, envList, args...)` → `Initialize(...)`.
- Capability enumeration: `ListTools` / `ListResources` / `ListResourceTemplates` / `ListPrompts`, registering the returned `mcp.Tool`/`Resource`/`ResourceTemplate`/`Prompt` directly onto `server.NewMCPServer` (same types, no schema rebuilding).
- Forwarding handlers: tool→`CallTool`, resource/template→`ReadResource`, prompt→`GetPrompt`.
- HTTP exposure: `server.NewStreamableHTTPServer(s, server.WithEndpointPath(endpoint))`, mounted on a shared `http.ServeMux`.
- Protocol version: each endpoint can configure `protocolVersion` (`ServerConfig.ProtocolVersion`) to pin the MCP protocol version it serves; implemented by overriding `result.ProtocolVersion` via `server.WithHooks` `AddAfterInitialize`. When unset, mcp-go's default client-version negotiation is preserved.
- When resource/template/prompt enumeration fails, only log a warning and skip (upstream may not support it), without failing the whole server; a failed tools enumeration skips that server.

## Test stdio server

`cmd/listfiles` is a minimal stdio MCP server for end-to-end bridge verification. Usage:

```bash
./build/listfiles /some/dir
```

After configuring it in `config.yaml`, run `initialize` → `tools/list` → `tools/call list_files` against its endpoint with curl to verify the forwarding chain.
