# `evsocket` v1 到 v2 迁移说明

## 一、导入路径变更

把：

```go
import socketio "github.com/PaienNate/pineutil/evsocket"
```

改成：

```go
import socketio "github.com/PaienNate/pineutil/evsocket/v2"
```

## 二、哪些能力继续保留

v2 继续保留以下 facade 级别能力：

### `SocketInstance`

- `NewSocketInstance()`
- `On(...)`
- `New(...)`
- `EmitTo(...)`
- `EmitToList(...)`
- `Broadcast(...)`
- `Fire(...)`
- `Shutdown()`

### `WebsocketWrapper`

- `Emit(...)`
- `EmitTo(...)`
- `Broadcast(...)`
- `Close()`
- `SetAttribute(...)`
- `GetAttribute(...)`
- `GetUUID()`
- `IsAlive()`

也就是说，**服务端主体使用方式基本不变**；大多数业务代码只需要改 import path。

如果你在 v1 中对服务端 `Upgrader` 做过自定义配置，则还需要把这些配置迁移到 `SocketInstance.ServerOptions`。

## 三、明确的破坏性变更

### 1. 不再暴露底层连接对象

v1 中业务代码如果直接使用：

- `WebsocketWrapper.Conn`
- 底层 websocket 的读写方法
- 底层连接的细节行为

在 v2 中都必须删除。

v2 只承诺 facade API，不承诺 transport 细节。

### 2. 客户端 API 重构

v1 客户端模式：

```go
client := sm.NewClient(url, options)
err := client.ClientConnect(func(kws *socketio.WebsocketWrapper) {
	// ...
})
```

v2 客户端模式有两种写法。

#### 写法 A：一步建立连接

```go
client, err := sm.Dial(url, socketio.ClientOptions{})
if err != nil {
	return err
}
```

#### 写法 B：先创建 facade，再显式连接

```go
client := sm.NewClient(url, socketio.ClientOptions{})

client.OnTextMessage = func(message string) {
	// ...
}

if err := client.Connect(); err != nil {
	return err
}
```

这里的 `NewClient(...)` 只创建 facade，不会自动发起连接。

### 3. `ClientConnect` 不再保留

如果你在 v1 中依赖：

- `ClientConnect(...)`

在 v2 中应改成：

- `Dial(...)`
- 或 `NewClient(...)` 后显式调用 `Connect()` / `Reconnect()`

这次改动的目标是让客户端 facade 生命周期更清晰：

- facade 先创建
- 回调和属性先挂好
- 连接和重连由上层明确触发

### 4. 重连方式变化

v2 使用：

```go
if err := client.Reconnect(); err != nil {
	return err
}
```

重连后的行为：

- `WebsocketWrapper` 对象本身不变
- UUID 不变
- 属性不变
- 已绑定的快捷回调不变
- 只替换底层 transport

这意味着业务层可以长期持有同一个 facade 对象。

同时也意味着：

- v2 不内建自动重连策略
- 是否重试、何时重试、重试间隔都由上层 FSM / retry 逻辑控制

## 四、握手头读取方式

v2 增加了只读握手头访问能力：

```go
token := kws.GetRequestHeader("Authorization")
```

适用场景：

- 服务端反向模式：在 `New(...)` 回调里读取客户端握手头
- 客户端正向模式：读取 `ClientOptions.RequestHeader` 中实际用于握手的头值

注意：

- `GetRequestHeader(key string) string` 是只读访问
- 该能力用于读取握手阶段信息，不代表重新暴露底层连接

## 五、客户端回调语义

### `OnConnectError`

每次连接失败都会触发，包括：

- 第一次连接失败
- 重连失败

### `OnConnectFailed`

只在**第一次连接尝试失败**时触发。

如果是 `Reconnect()` 失败，不会触发 `OnConnectFailed`。

### `OnConnected` / `OnDisconnected`

- `OnConnected` 会在 transport 建立完成后异步触发（脱离网络读循环），可以在里面做 request-response 等阻塞操作
- `OnDisconnected` 会在当前 transport 断开后异步触发
- 如果你要不要重连，请在 `OnDisconnected` 后由自己的状态机决定是否调用 `Reconnect()`
- 服务端的 `New(callback)` 仅用于同步初始化（设置 UUID、属性、绑定回调），**不应在 `callback` 内等待后续消息**

## 六、heartbeat 配置说明

v2 客户端默认使用主动 Ping 保活。

可配置项如下：

- `HandshakeTimeout`：握手超时，默认 `10s`
- `PingInterval`：主动发送 Ping 的间隔，默认 `15s`
- `PingWait`：等待入站有效帧的最长时间，默认 `45s`

行为说明：

- 客户端会主动发送 Ping
- 收到文本、二进制、Ping、Pong 等有效入站帧时会刷新 deadline
- 这套机制只负责保活与失活探测，不负责自动重连

示例：

```go
client := sm.NewClient(url, socketio.ClientOptions{
	PingInterval: 10 * time.Second,
	PingWait:     30 * time.Second,
})
```

如果你明确不希望客户端主动发 Ping，需要结合你的业务约束自行评估配置；默认推荐保留该行为。

## 七、推荐迁移方式

### 情况 1：你只用了服务端

通常只需要：

1. 改 import path
2. 确认没有直接访问底层 `Conn`
3. 如果原来从升级请求里读认证头，改成 `GetRequestHeader(...)`
4. 如果原来改过 v1 的服务端 `Upgrader` 配置，把它迁移到 `SocketInstance.ServerOptions`

例如：

```go
sm := socketio.NewSocketInstance()
sm.ServerOptions.SubProtocols = []string{"onebot"}
sm.ServerOptions.ResponseHeader = http.Header{
	"X-Server-Mode": []string{"demo"},
}
```

### 情况 2：你用了客户端，但没碰底层连接

把：

- `NewClient + ClientConnect`

改成：

- `Dial`
或
- `NewClient + Connect`

并把重连改为 `Reconnect()`。

如果你之前把 `ClientConnect` 当成“失败后自动继续尝试”的入口，需要把这部分逻辑外提到你自己的 FSM / retry 层。

### 情况 3：你直接依赖了底层连接

这类代码必须重写，改成只使用：

- `SocketInstance` 全局事件
- `WebsocketWrapper` facade 方法
- `Dial` / `Connect` / `Reconnect`

## 八、迁移建议

如果你在 v1 中做过以下事情：

- 直接调用原始 websocket 连接的 API
- 自己控制底层读写循环
- 把 `ClientConnect` 当成阻塞式生命周期入口

建议在迁移到 v2 时统一改成：

- 所有消息入口走事件回调
- 所有发送动作走 `Emit` / `EmitTo` / `Broadcast`
- 所有连接生命周期走 `Connect` / `Reconnect` / `Close`
- 所有握手阶段认证信息走 `GetRequestHeader(...)`

这样可以真正享受到 facade-only 设计带来的稳定性和可维护性。
