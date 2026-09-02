# mcptransformer

[English](./README.en.md)

把多个 stdio 的 MCP 服务器桥接为 **Streamable HTTP** 服务：启动 stdio 子进程作为上游 MCP 客户端，枚举其 tools / resources / resource templates / prompts，再通过 `StreamableHTTPServer` 暴露到同一个 HTTP 端口的多个 path 上。

## 功能

- 配置文件驱动：每个 stdio 服务器对应一个 HTTP endpoint。
- 自动转发 tools、resources、resource templates、prompts 调用。
- 单端口多 path 挂载，session 管理、SSE、CORS 由 `mcp-go` 提供。
- 优雅退出：`SIGINT`/`SIGTERM` 时关闭 HTTP 服务并结束所有子进程。
- 上游 stderr 转发到本进程日志，便于排障。
- 管理 API：独立的状态查询与运行期控制（reload / stop / start）接口。

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
listen: ":8080"          # MCP 业务监听地址（默认 :8080）

admin:                   # 可选，管理 API（独立端口，缺省即下方默认值）
  listen: ":8081"               # 管理端口，默认 :8081；与 listen 相同时自动合并挂载
  statusPath: /__admin/status    # GET 查询上游状态
  controlPath: /__admin/control  # POST 控制 reload/stop/start

servers:
  - name: filesystem            # 对外广告的 server 名
    endpoint: /mcp/filesystem   # Streamable HTTP 端点路径
    version: "1.0.0"            # 可选，server 实现版本，默认 1.0.0
    protocolVersion: "2025-06-18" # 可选，端点固定服务的 MCP 协议版本（默认跟随客户端协商）
    command: npx                # stdio 服务器命令
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    env:                        # 可选，子进程环境变量
      LOG_LEVEL: debug

  - name: listfiles
    endpoint: /mcp/listfiles
    protocolVersion: "2025-06-18"
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

## 管理 API

> 管理接口默认独立监听 `:8081`（`admin.listen` 可配置），与 MCP 业务端口分离；
> 若 `admin.listen` 与 `listen` 相同，则自动合并挂载到同一端口。
> 路径可在 `admin` 配置段自定义（默认 `/__admin/status` 与 `/__admin/control`）。

### 状态查询

`GET /__admin/status` 返回每个上游服务器的运行状态：

```bash
curl -s http://localhost:8081/__admin/status
```

```json
{
  "servers": [
    {"name": "listfiles", "status": "running"}
  ]
}
```

`status` 取值：`running`（已连接）、`down`（进程退出、重连中）、`stopped`（已手动 stop）、`error`（启动失败）。

### 控制

`POST /__admin/control`，`action` 支持 `reload` / `stop` / `start`，`server` 指定目标（缺省作用于所有服务器）：

```bash
# 重连并重新枚举某个上游的能力
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"reload","server":"listfiles"}'

# 停止某个上游（endpoint 返回 503）
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"stop","server":"listfiles"}'

# 重新启动已停止的上游
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"start","server":"listfiles"}'

# 不带 server 则作用于全部
curl -s -X POST http://localhost:8081/__admin/control \
  -H 'Content-Type: application/json' \
  -d '{"action":"reload"}'
```

响应示例：

```json
{"results":[{"name":"listfiles","action":"reload","ok":true}]}
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

- 上游能力在 reload 时重新快照注册；`stop` 后需通过控制 API `start` 恢复。
