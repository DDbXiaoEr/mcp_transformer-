# mcptransformer

[中文](./README.md)

Bridges multiple stdio MCP servers into a **Streamable HTTP** service: launches each stdio subprocess as an upstream MCP client, enumerates its tools / resources / resource templates / prompts, then exposes them via `StreamableHTTPServer` on multiple paths of a single HTTP port.

## Features

- Config-driven: each stdio server maps to one HTTP endpoint.
- Auto-forwards tools, resources, resource templates, and prompts.
- Single port, multiple paths; session management, SSE, and CORS provided by `mcp-go`.
- Graceful shutdown: on `SIGINT`/`SIGTERM` closes the HTTP server and terminates all subprocesses.
- Upstream stderr is forwarded to the process log for easy debugging.
- Admin API: separate status query and runtime control (reload / stop / start) endpoints.

## Build

```bash
make            # builds build/mcptransformer and build/listfiles, and copies config.yaml into build/
# or
go build -o build/mcptransformer ./cmd/mcptransformer
```

Build artifacts and the config file are output to `build/`, which is a self-contained runnable directory.

## Configuration

`config.yaml`:

```yaml
listen: ":8080"          # MCP business listen address (default :8080)

admin:                   # optional, admin API (separate port, defaults shown below)
  listen: ":8081"               # admin port, default :8081; merges onto the same mux when equal to listen
  statusPath: /__admin/status    # GET upstream status
  controlPath: /__admin/control  # POST reload/stop/start

servers:
  - name: filesystem            # advertised server name
    endpoint: /mcp/filesystem   # Streamable HTTP endpoint path
    version: "1.0.0"            # optional, server implementation version, default 1.0.0
    protocolVersion: "2025-06-18" # optional, MCP protocol version this endpoint serves (default: negotiate with client)
    command: npx                # stdio server command
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    env:                        # optional, subprocess environment variables
      LOG_LEVEL: debug

  - name: listfiles
    endpoint: /mcp/listfiles
    protocolVersion: "2025-06-18"
    command: ./listfiles
    args: ["."]
```

## Run

The program reads `config.yaml` from the **working directory** by default; use `--config` to specify a path explicitly:

```bash
cd build && ./mcptransformer              # reads ./config.yaml by default
./build/mcptransformer --config build/config.yaml
```

## Verify

```bash
# initialize to obtain a session id
curl -s -i -X POST http://localhost:8080/mcp/listfiles \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'

# call a tool with the Mcp-Session-Id header
curl -s -X POST http://localhost:8080/mcp/listfiles \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Mcp-Session-Id: <session-id>' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_files","arguments":{}}}'
```

## Admin API

> The admin API listens on a separate port `:8081` by default (`admin.listen` is configurable),
> isolated from the MCP business port; when `admin.listen` equals `listen`, both are mounted on
> the same port. Paths can be customized via the `admin` config section (defaults `/__admin/status`
> and `/__admin/control`).

### Status

`GET /__admin/status` returns the running status of every upstream server:

```bash
curl -s http://localhost:8081/__admin/status
```

```json
{
  "servers": [
    {"name": "listfiles", "endpoint": "/mcp/listfiles", "command": "./listfiles", "status": "running"}
  ]
}
```

`status` values: `running` (connected), `down` (process exited, reconnecting), `stopped` (stopped via control), `error` (failed to start).

### Control

`POST /__admin/control`; `action` is one of `reload` / `stop` / `start`, and `server` targets a specific server (defaults to all):

```bash
# reconnect and re-enumerate one upstream's capabilities
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"reload","server":"listfiles"}'

# stop one upstream (its endpoint then returns 503)
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"stop","server":"listfiles"}'

# restart a stopped upstream
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"start","server":"listfiles"}'

# omit server to act on all servers
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"reload"}'
```

Example response:

```json
{"results":[{"name":"listfiles","action":"reload","ok":true}]}
```

## Project structure

```
.
├── cmd/
│   ├── mcptransformer/   # bridge service main program
│   │   └── main.go
│   └── listfiles/        # test stdio MCP server (lists directory files)
│       └── main.go
├── config.yaml           # example config (copied to build/config.yaml on make)
├── Makefile
├── go.mod / go.sum
├── AGENTS.md
└── README.md
```

After `make build`, the `build/` directory contains:

```
build/
├── mcptransformer
├── listfiles
└── config.yaml
```

## Known limitations

- Upstream capabilities are re-snapshotted on `reload`; after `stop`, use the control API `start` to recover.
