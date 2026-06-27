# evsocket v2

`evsocket/v2` 是构建在 `github.com/lxzan/gws` 之上的中文业务 facade。

它的定位不是暴露底层 transport，而是给业务层提供一套稳定、统一、可重连的会话接口：

- `SocketInstance` 是服务端与客户端共用的 facade 入口
- 服务端通过 `New(...)` 接入 HTTP 升级
- 客户端通过 `NewClient` / `Connect` / `Reconnect` / `Dial` 建立连接
- 服务端与客户端最终都只拿到 `*WebsocketWrapper`
- 定向发送、广播、属性存储、事件监听都走 facade API

## 设计原则

### 1. 保持 facade-only

v2 正式不再暴露原始 websocket `Conn`。

业务代码应只依赖：

- `SocketInstance`
- `WebsocketWrapper`
- 事件回调与 facade 方法

这样可以避免业务层和 transport 实现细节耦合，也让后续重连、路由、会话生命周期保持一致。

### 2. 服务端与客户端统一成同一种会话抽象

无论连接来自：

- 服务端 `New(...)`
- 客户端 `Dial(...)`
- 客户端 `NewClient(...)` 后再 `Connect()`

业务层最终拿到的都是 `*WebsocketWrapper`。

### 3. 客户端由 facade 持有 transport

客户端正式 API 为：

- `NewClient(url, options)`：创建稳定 facade，但不立即连接
- `Connect()`：建立第一次连接
- `Reconnect()`：在同一个 facade 上重连
- `Dial(url, options)`：一步创建并连接

`Reconnect()` 只负责在当前 facade 上重新建立 transport；是否要重试、何时重试，仍由上层 FSM / retry 逻辑控制，库本身不内建自动重连策略。

### 4. 客户端默认主动 Ping 保活

客户端默认会：

- 以 `PingInterval` 主动发送 Ping
- 在收到入站有效帧时刷新 read deadline
- 用 `PingWait` 控制等待入站有效帧的最长时间

如果没有显式配置：

- `PingInterval` 默认 `15s`
- `PingWait` 默认 `45s`
- `HandshakeTimeout` 默认 `10s`

## 安装

```bash
go get github.com/PaienNate/pineutil/evsocket/v2
```

## 示例

可直接运行包内示例测试：

```bash
go test ./evsocket/v2 -run Example -count=1
```

示例覆盖：

- 服务端 `SocketInstance.New(...)`
- 客户端 `Dial(...)`
- 客户端 `NewClient(...) + Connect() + Reconnect()`
- 服务端读取握手头 `GetRequestHeader(...)`

## 常用 API

### `SocketInstance`

- `NewSocketInstance()`
- `ServerOptions`：服务端 `gws.ServerOption` 配置入口，可继续设置子协议、响应头、压缩、鉴权等握手参数
- `On(event, callback)`
- `New(callback)`
- `NewClient(url, options)`
- `Dial(url, options)`
- `EmitTo(uuid, message, mType...)`
- `EmitToList(uuids, message, mType...)`
- `Broadcast(message, mType...)`
- `Fire(event, data)`
- `Shutdown()`

### `WebsocketWrapper`

- `Connect()`
- `Reconnect()`
- `Emit(message, mType...)`
- `EmitTo(uuid, message, mType...)`
- `Broadcast(message, except, mType...)`
- `Close()`
- `SetAttribute(key, value)`
- `GetAttribute(key)`
- `GetRequestHeader(key)`
- `GetUUID()`
- `IsAlive()`

## 推荐使用方式

### 服务端

- 用 `SocketInstance` 维护全局监听与连接池
- 如果需要自定义服务端握手参数，请在 `SocketInstance.ServerOptions` 上设置
- **`New(callback)` 是同步初始化钩子**：仅用于设置 `UUID`、`attribute`、绑定回调等短小操作；不应在 `callback` 内等待后续消息
- 用 `On(...)` 监听 `EventConnect`、`EventMessage`、`EventDisconnect`
- 用 `New(...)` 挂到 `http.HandleFunc`
- 连接建立后的业务逻辑应写在 `OnConnected` 或 `EventConnect` 中，这些回调已脱离网络读循环异步执行
- `OnXxx` 快捷回调（`OnConnected`、`OnTextMessage`、`OnBinaryMessage`、`OnPingReceived`、`OnPongReceived`、`OnDisconnected`）是同名事件的兼容别名，会在事件监听器之前调用

例如：

```go
sm := socketio.NewSocketInstance()
sm.ServerOptions.SubProtocols = []string{"onebot"}
sm.ServerOptions.ResponseHeader = http.Header{
	"X-Server-Mode": []string{"demo"},
}
```

### 客户端

- 想一步连接时用 `Dial(...)`
- 想先设置回调、属性、UUID，再连接时用 `NewClient(...)` + `Connect()`
- 底层断开后，由上层状态机决定是否调用 `Reconnect()`

## 与 v1 的区别

- 原始 `Conn` 不再暴露
- 客户端 API 改成 `NewClient` + `Connect` / `Reconnect`，或 `Dial`
- `ClientConnect` 不再保留
- facade 对象在 `Reconnect()` 后保持稳定，UUID、属性和绑定回调不会丢失
- 客户端默认启用主动 Ping 保活

详细迁移说明见 `MIGRATION.md`。
