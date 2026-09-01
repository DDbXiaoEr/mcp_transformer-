# 项目结构

> 项目结构权威快照。开始改动前请先读本文件与 `AGENTS.md` 直接定位目标文件，
> 不要每次都全量探索仓库（避免反复 glob/grep 整个项目）。
> 当目录结构、文件职责或依赖发生变化时，请同步更新本文件。

## 目录树

```
.
├── AGENTS.md / AGENTS.en.md   # Agent 工作约定（构建验证、目录约定、探索约束）
├── README.md / README.en.md   # 项目说明与用法
├── PROJECT_STRUCTURE.md       # 本文件：项目结构快照
├── Makefile                   # 构建 / 测试 / vet / clean
├── go.mod / go.sum            # Go module 定义与依赖锁定
├── config.yaml                # 示例配置（make 时复制到 build/config.yaml）
├── configgen.html             # 独立静态 HTML 配置生成器页面（无构建依赖）
├── .gitignore                 # 忽略 build/ 等产物
└── cmd/
    ├── mcptransformer/
    │   └── main.go            # 桥接服务主程序
    └── listfiles/
        └── main.go            # 测试用 stdio MCP 服务器
```

## 各文件职责

### 根目录

- `Makefile` — 构建入口：`make` 构建 `build/mcptransformer` 与 `build/listfiles` 并复制
  `config.yaml`；`make vet` / `make test` / `make clean`。
- `go.mod` — module `mcptransformer`，go 1.25.5。直接依赖 `github.com/mark3labs/mcp-go v0.58.0`、
  `gopkg.in/yaml.v3 v3.0.1`。
- `config.yaml` — 示例配置，`servers` 每项对应一个 HTTP `endpoint`（字段说明见 `README.md`）。
- `configgen.html` — 静态 HTML 页面，用于可视化生成 `config.yaml`，不参与构建/运行。

### cmd/mcptransformer/main.go — 桥接服务主程序

- `Config` / `AdminConfig` / `ServerConfig` — YAML 配置结构体。
- `main` — 解析配置 → 校验 server 与 admin 路径 → 逐个 `managedServer.start` → 挂业务 `mcpMux`
  与 `adminMux`（独立 `listen` / `admin.listen` 端口，同地址时合并）→ `ListenAndServe` → 优雅退出。
- `loadConfig` / `validateServer` / `adminPaths` / `validateAdminPath` / `envList` — 配置读取、
  字段校验、admin 路径默认值与格式校验、env map 展开为 `k=v` 列表。
- `proxy` — 上游 stdio 客户端封装：持有可替换的 `*client.Client`，`monitor` goroutine 监听
  stderr EOF 检测进程退出并自动重连（指数退避）。`status` 报告 running/down/stopped。
- `newProxy` / `connect` / `current` / `isClosed` / `status` / `close` / `monitor` / `sleepOrDone` —
  创建、连接、取当前客户端、关闭标记、状态、监控重连、退避等待。
- `proxy.ListTools` / `ListResources` / `ListResourceTemplates` / `ListPrompts` /
  `CallTool` / `ReadResource` / `GetPrompt` — 转发到当前上游客户端。
- `runtime` / `buildRuntime` — 一套可整体替换的“上游 proxy + StreamableHTTPServer handler”单元。
- `managedServer` — 生命周期封装：持有可替换的 `runtime`，实现 `http.Handler` 以便 mux 稳定挂载；
  `start` / `reload` / `stop` 控制重建/替换/拆除，`status` 报告状态。
- `manager` — 维护全部 managedServer（按 name 索引），暴露 `handleStatus`（GET）与
  `handleControl`（POST：reload/stop/start）两个管理 HTTP API。
- `registerTools` / `registerResources` / `registerPrompts` — 枚举并注册上游能力，
  handler 转发到 proxy。
- `writeJSON` / `namedWriter` — JSON 响应助手 / 将上游 stderr 写入本进程日志。

### cmd/listfiles/main.go — 测试用 stdio MCP 服务器

- `fileEntry` — 文件条目结构（名称/大小/权限/是否目录/修改时间）。
- `main` — 注册 `list_files` 工具（可带 `path` 参数），`server.ServeStdio` 启动。
