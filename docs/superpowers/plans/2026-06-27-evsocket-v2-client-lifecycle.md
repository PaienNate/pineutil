# `evsocket/v2` 客户端状态机与保活完善 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完善 `evsocket/v2` 的客户端 facade，使其具备清晰状态机、主动 Ping 保活、只读握手头访问，以及与现有外部 FSM / retry 兼容的客户端行为语义。

**Architecture:** 保持 `SocketInstance` / `WebsocketWrapper` facade 不变，继续使用 `gws` 作为底层 transport。客户端重连仍由外部调用方控制，`evsocket/v2` 只负责单次连接建立、连接存活检测、断线发现、统一回调分发和稳定 facade 语义。内部引入轻量状态机和 heartbeat 配置，不实现自动重试调度。

**Tech Stack:** Go 1.23, `github.com/lxzan/gws`, `github.com/looplab/fsm`, `testing`, `httptest`, `gorilla/websocket`, `go test -race`

## Global Constraints

- 保持 `evsocket/v2` 的 facade-only 设计，不重新暴露原始 websocket `Conn`。
- 客户端自动重连策略不内建，仍由上层 FSM / retry 逻辑控制。
- 正式保留客户端 API：`NewClient`、`Connect`、`Reconnect`、`Dial`。
- 正式保留服务端 facade：`SocketInstance`、`On(...)`、`New(...)`、`EmitTo(...)`、`Broadcast(...)`、`Shutdown()`。
- 增加只读握手头访问能力，使用 `GetRequestHeader(key string) string`。
- 客户端默认使用主动 Ping 保活，并对入站有效帧刷新 deadline。
- 所有新增行为必须先有 failing test，再写实现。
- 所有公开文档、示例、注释使用中文。
- 验证顺序：先定向测试，再 `go test ./evsocket/v2 -race -count=1`，最后 `go test ./... -count=1`。

---

## File Map

- Modify: `evsocket/v2/socketio.go`
- Modify: `evsocket/v2/socketio_test.go`
- Modify: `evsocket/v2/README.md`
- Modify: `evsocket/v2/MIGRATION.md`
- Modify: `evsocket/v2/example_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

## Planned API / Behavior Changes

### 客户端 facade 正式 API
- `func (sm *SocketInstance) NewClient(rawURL string, options ClientOptions) *WebsocketWrapper`
- `func (sm *SocketInstance) Dial(rawURL string, options ClientOptions) (*WebsocketWrapper, error)`
- `func (kws *WebsocketWrapper) Connect() error`
- `func (kws *WebsocketWrapper) Reconnect() error`

### 新增只读握手头访问
- `func (kws *WebsocketWrapper) GetRequestHeader(key string) string`

### ClientOptions 需要明确支持
- `RequestHeader http.Header`
- `Subprotocols []string`
- `UseCompression bool`
- `TLSConfig *tls.Config`
- `HandshakeTimeout time.Duration`
- `PingInterval time.Duration`
- `PingWait time.Duration`
- `NewDialer func() (gws.Dialer, error)`

### 客户端状态机
- `idle`
- `connecting`
- `connected`
- `reconnecting`
- `closing`
- `closed`

### 客户端保活模型
- `OnOpen`：设置 deadline = `now + PingWait`
- 启动主动 Ping ticker：每隔 `PingInterval` 发送 `Ping`
- 收到 `Ping` / `Pong` / `Message` 时刷新 deadline
- `OnPing` 自动 `WritePong(nil)`
- 超时后进入断线路径并触发 `OnDisconnected`

### Task 1: 先把客户端状态机语义用测试钉死

**Files:**
- Modify: `evsocket/v2/socketio_test.go`
- Test: `evsocket/v2/socketio.go`

**Interfaces:**
- Consumes: `NewClient`, `Connect`, `Reconnect`, `Dial`, `OnConnectError`, `OnConnectFailed`, `OnDisconnected`
- Produces: 客户端状态语义回归测试，覆盖连接/重连/重入/关闭路径

- [ ] **Step 1: 写连接状态语义 failing tests**

在 `evsocket/v2/socketio_test.go` 新增测试，覆盖：
- `Connect()` 成功后 facade 为 alive
- 已连接状态下再次 `Connect()` 不重复建立 transport
- `Reconnect()` 成功后 facade 对象不变、UUID 不变
- 连接中并发 `Connect()` / `Reconnect()` 会被拒绝或安全串行化（按最终设计选一条并固定）

- [ ] **Step 2: 写回调语义 failing tests**

新增测试，固定：
- 首次连接失败：触发 `OnConnectError` + `OnConnectFailed`
- 重连失败：只触发 `OnConnectError`
- 断线只触发一次 `OnDisconnected`

- [ ] **Step 3: 运行定向测试确认先失败**

Run: `go test ./evsocket/v2 -run 'TestSocketInstanceDial|TestSocketInstanceNewClient|TestReconnect|TestConnect' -count=1`

Expected: 至少有一部分关于重入、状态语义或并发连接的测试失败。

- [ ] **Step 4: 实现最小状态机**

在 `evsocket/v2/socketio.go` 中：
- 引入内部客户端状态表示
- 给 `Connect()` / `Reconnect()` 增加状态检查
- 防止并发重入
- 保持 facade 对象与 UUID 稳定

- [ ] **Step 5: 运行定向测试确认通过**

Run: `go test ./evsocket/v2 -run 'TestSocketInstanceDial|TestSocketInstanceNewClient|TestReconnect|TestConnect' -count=1`

Expected: 状态机与回调语义测试通过。

### Task 2: 实现主动 Ping 保活与 deadline 刷新

**Files:**
- Modify: `evsocket/v2/socketio.go`
- Modify: `evsocket/v2/socketio_test.go`

**Interfaces:**
- Consumes: `ClientOptions.PingInterval`, `ClientOptions.PingWait`, `transportHandler.OnOpen`, `OnPing`, `OnPong`, `OnMessage`
- Produces: 客户端 heartbeat / deadline 机制和断线发现逻辑

- [ ] **Step 1: 写 heartbeat / deadline failing tests**

在 `evsocket/v2/socketio_test.go` 新增测试，覆盖：
- client 建立后主动发 `Ping`
- server 返回 `Pong` 后 client 仍保持 alive
- 收到普通消息会刷新 deadline
- 当服务端不再响应且超过 `PingWait` 后，client 触发 `OnDisconnected`

- [ ] **Step 2: 运行 heartbeat 测试确认失败**

Run: `go test ./evsocket/v2 -run 'TestClientHeartbeat|TestClientDeadline|TestClientDisconnectOnTimeout' -count=1`

Expected: 至少关于主动 Ping 或超时断开的测试失败。

- [ ] **Step 3: 在 ClientOptions 中加入正式 heartbeat 配置**

在 `evsocket/v2/socketio.go` 中正式定义：
- `PingInterval`
- `PingWait`

并设置默认值（建议）：
- `HandshakeTimeout = 10s`
- `PingInterval = 15s`
- `PingWait = 45s`

- [ ] **Step 4: 实现 heartbeat runtime**

在 `evsocket/v2/socketio.go` 中实现：
- `OnOpen` 设置首次 deadline
- 启动主动 Ping ticker
- `OnPing` 刷新 deadline 并 `WritePong(nil)`
- `OnPong` 刷新 deadline
- `OnMessage` 刷新 deadline
- timeout 触发关闭并走统一 `OnDisconnected`

- [ ] **Step 5: 跑 heartbeat 定向测试**

Run: `go test ./evsocket/v2 -run 'TestClientHeartbeat|TestClientDeadline|TestClientDisconnectOnTimeout' -count=1`

Expected: 主动 Ping、deadline 刷新、超时断开测试全部通过。

- [ ] **Step 6: 跑 race 测试**

Run: `go test ./evsocket/v2 -race -count=1`

Expected: heartbeat 引入后无 race、无 goroutine 泄漏、无重复断线回调。

### Task 3: 增加只读握手头访问能力

**Files:**
- Modify: `evsocket/v2/socketio.go`
- Modify: `evsocket/v2/socketio_test.go`

**Interfaces:**
- Consumes: server `New(...)` / client `Dial(...)` 握手路径
- Produces: `GetRequestHeader(key string) string`

- [ ] **Step 1: 写 failing tests**

在 `evsocket/v2/socketio_test.go` 新增测试：
- 服务端反向连接场景：回调里通过 `GetRequestHeader("Authorization")` 能读到请求头
- 客户端正向连接场景：facade 侧也能读取发送时配置的 `RequestHeader`

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./evsocket/v2 -run 'TestGetRequestHeader' -count=1`

Expected: 由于当前没有 `GetRequestHeader` API，测试失败。

- [ ] **Step 3: 实现只读握手头存储**

在 `evsocket/v2/socketio.go` 中：
- 将 server/client 握手头保存为 facade 内部只读副本
- 增加：
  - `func (kws *WebsocketWrapper) GetRequestHeader(key string) string`

要求：
- 不返回底层对象
- 不允许业务直接修改内部 header
- 对大小写和多值行为保持 `http.Header` 默认语义

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./evsocket/v2 -run 'TestGetRequestHeader' -count=1`

Expected: 正向和反向握手头读取测试通过。

### Task 4: 把当前 OneBot 风格调用路径需要的能力补成完整契约

**Files:**
- Modify: `evsocket/v2/socketio.go`
- Modify: `evsocket/v2/socketio_test.go`

**Interfaces:**
- Consumes: `RequestHeader`, `OnConnected`, `OnDisconnected`, `OnConnectError`, `OnConnectFailed`
- Produces: 与上层 FSM / retry 配合稳定的 facade 行为

- [ ] **Step 1: 写整合型 failing tests**

补一组接近真实调用方的测试：
- `NewClient(...)`
- 先设置 `RequestHeader`
- 再设置 `OnConnectError`
- 再设置 `OnDisconnected`
- 再 `Connect()`
- 断线后外部调用 `Reconnect()`
- 验证回调和状态迁移顺序

- [ ] **Step 2: 运行整合测试确认失败或不完整**

Run: `go test ./evsocket/v2 -run 'TestClientLifecycleForExternalFSM' -count=1`

Expected: 当前实现如果在状态、保活或 header 访问上不完整，应先失败。

- [ ] **Step 3: 实现与外部 FSM 配合所需的最小补丁**

在 `evsocket/v2/socketio.go` 中确保：
- facade 不自动重试
- 断线时快速进入非活动态
- `Reconnect()` 不会抹掉已有 UUID / attributes / callbacks
- 失败语义对外稳定

- [ ] **Step 4: 运行整合测试确认通过**

Run: `go test ./evsocket/v2 -run 'TestClientLifecycleForExternalFSM' -count=1`

Expected: 测试通过，说明 v2 客户端可用于你当前 OneBot 风格调用方。

### Task 5: 中文文档、示例、迁移说明补全

**Files:**
- Modify: `evsocket/v2/README.md`
- Modify: `evsocket/v2/MIGRATION.md`
- Modify: `evsocket/v2/example_test.go`

**Interfaces:**
- Consumes: 任务 1-4 最终 API
- Produces: 中文说明与可编译示例

- [ ] **Step 1: 更新 README**

在 `evsocket/v2/README.md` 中明确说明：
- `SocketInstance` 定位
- client/server 统一 facade
- 正式客户端 API：`NewClient` / `Connect` / `Reconnect` / `Dial`
- 主动 Ping 保活策略
- 不再暴露原始 `Conn`

- [ ] **Step 2: 更新 MIGRATION**

在 `evsocket/v2/MIGRATION.md` 中新增：
- `GetRequestHeader` 的使用方式
- 客户端回调语义说明
- heartbeat 配置说明
- 与 v1 `ClientConnect` 的差异

- [ ] **Step 3: 补充中文示例**

在 `evsocket/v2/example_test.go` 增加或更新示例：
- 服务端示例
- 客户端 `Dial` 示例
- 客户端 `NewClient + Connect + Reconnect` 示例
- 读取握手头示例（服务端反向模式）

- [ ] **Step 4: 运行示例测试**

Run: `go test ./evsocket/v2 -run Example -count=1`

Expected: 示例可编译并通过。

### Task 6: 全量验证与稳态检查

**Files:**
- Test: `evsocket/v2/*.go`
- Test: `./...`

**Interfaces:**
- Consumes: 全部已完成改动
- Produces: 可交付的 v2 客户端与保活实现

- [ ] **Step 1: 跑 v2 全包测试**

Run: `go test ./evsocket/v2 -count=1`

Expected: 全部通过。

- [ ] **Step 2: 跑 v2 race**

Run: `go test ./evsocket/v2 -race -count=1`

Expected: 无数据竞争。

- [ ] **Step 3: 跑全仓测试**

Run: `go test ./... -count=1`

Expected: 不破坏其他包。

- [ ] **Step 4: 跑全仓构建**

Run: `go build ./...`

Expected: 构建通过。

- [ ] **Step 5: 稳态检查**

确认以下契约都在测试中被覆盖：
- 首次连接失败
- 重连失败
- 主动 Ping
- `Ping/Pong/Message` 刷新 deadline
- timeout 断开
- 只读握手头访问
- facade 对象稳定
- UUID 稳定
- 外部 FSM 可安全接管重试策略
