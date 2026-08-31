# AGENTS.md

[English](./AGENTS.en.md)

## 项目概述

`mcptransformer` 是一个 Go 程序，把多个 stdio 的 MCP 服务器桥接为 Streamable HTTP 服务。依赖 `github.com/mark3labs/mcp-go` 与 `gopkg.in/yaml.v3`。

## 构建与验证

```bash
make            # 构建 build/mcptransformer 与 build/listfiles，并复制 config.yaml 到 build/
make vet        # go vet
make test       # go test ./...
make clean
```

修改代码后必须运行 `go build ./...` 或 `make` 确认编译通过，并运行 `go vet ./...`。

## 目录约定

- `cmd/mcptransformer/main.go` — 桥接服务主程序（配置解析、stdio 客户端、转发注册、StreamableHTTP 服务、优雅退出）。
- `cmd/listfiles/main.go` — 测试用 stdio MCP 服务器，暴露 `list_files` 工具列出目录文件。
- `config.yaml` — 示例配置，每条 `servers` 项对应一个 HTTP `endpoint`。
- 二进制与配置输出到 `build/`，已被 `.gitignore` 忽略。
- 默认配置路径：工作目录下的 `config.yaml`（`--config` 可覆盖）。

## 关键实现要点

- 上游连接：`client.NewStdioMCPClient(command, envList, args...)` → `Initialize(...)`。
- 能力枚举：`ListTools` / `ListResources` / `ListResourceTemplates` / `ListPrompts`，把返回的 `mcp.Tool`/`Resource`/`ResourceTemplate`/`Prompt` 直接注册到 `server.NewMCPServer`（类型同源，无需重建 schema）。
- 转发 handler：tool→`CallTool`，resource/template→`ReadResource`，prompt→`GetPrompt`。
- HTTP 暴露：`server.NewStreamableHTTPServer(s, server.WithEndpointPath(endpoint))`，挂到共享 `http.ServeMux`。
- 协议版本：每个 endpoint 可配置 `protocolVersion`（`ServerConfig.ProtocolVersion`）来固定对外服务的 MCP 协议版本；实现方式是通过 `server.WithHooks` 的 `AddAfterInitialize` 钩子覆盖 `result.ProtocolVersion`。未配置时保持 mcp-go 默认的客户端版本协商。
- 资源/模板/提示词枚举失败时仅记录告警并跳过（上游可能不支持），不会导致整条 server 失败；tools 枚举失败则跳过该 server。

## 测试 stdio 服务器

`cmd/listfiles` 是一个最小 stdio MCP 服务器，用于端到端验证桥接。用法：

```bash
./build/listfiles /some/dir
```

通过 `config.yaml` 配置后，用 curl 对其 endpoint 做 `initialize` → `tools/list` → `tools/call list_files` 即可验证转发链路。
