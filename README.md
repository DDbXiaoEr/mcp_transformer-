# mcptransformer

把多个 stdio 的 MCP 服务器桥接为 **Streamable HTTP** 服务：启动 stdio 子进程作为上游 MCP 客户端，枚举其 tools / resources / resource templates / prompts，再通过 `StreamableHTTPServer` 暴露到同一个 HTTP 端口的多个 path 上。

## 功能

- 配置文件驱动：每个 stdio 服务器对应一个 HTTP endpoint。
- 自动转发 tools、resources、resource templates、prompts 调用。
- 单端口多 path 挂载，session 管理、SSE、CORS 由 `mcp-go` 提供。
- 优雅退出：`SIGINT`/`SIGTERM` 时关闭 HTTP 服务并结束所有子进程。
- 上游 stderr 转发到本进程日志，便于排障。

## 构建

```bash
make            # 构建 build/mcptransformer 与 build/listfiles，并把 config.yaml 复制到 build/
# 或
go build -o build/mcptransformer ./cmd/mcptransformer
```

构建产物与配置文件都输出到 `build/` 目录，`build/` 是一个可独立运行的自包含目录。

## 配置

`config.yaml`：

```yaml
listen: ":8080"          # 共享 HTTP 监听地址

servers:
  - name: filesystem            # 对外广告的 server 名
    endpoint: /mcp/filesystem   # Streamable HTTP 端点路径
    version: "1.0.0"            # 可选，默认 1.0.0
    command: npx                # stdio 服务器命令
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    env:                        # 可选，子进程环境变量
      LOG_LEVEL: debug

  - name: listfiles
    endpoint: /mcp/listfiles
    command: ./listfiles
    args: ["."]
```

## 运行

程序默认读取**工作目录**下的 `config.yaml`，也可用 `--config` 显式指定：

```bash
cd build && ./mcptransformer              # 默认读 ./config.yaml
./build/mcptransformer --config build/config.yaml
```

## 验证

```bash
# initialize 获取 session id
curl -s -i -X POST http://localhost:8080/mcp/listfiles \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'

# 带上 Mcp-Session-Id 调用工具
curl -s -X POST http://localhost:8080/mcp/listfiles \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Mcp-Session-Id: <session-id>' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_files","arguments":{}}}'
```

## 项目结构

```
.
├── cmd/
│   ├── mcptransformer/   # 桥接服务主程序
│   │   └── main.go
│   └── listfiles/        # 测试用 stdio MCP 服务器（列出目录文件）
│       └── main.go
├── config.yaml           # 示例配置（make 时复制到 build/config.yaml）
├── Makefile
├── go.mod / go.sum
├── AGENTS.md
└── README.md
```

`make build` 后 `build/` 目录结构：

```
build/
├── mcptransformer
├── listfiles
└── config.yaml
```

## 已知限制

- 上游能力在启动时快照注册，运行期新增/删除工具需重启本服务。
