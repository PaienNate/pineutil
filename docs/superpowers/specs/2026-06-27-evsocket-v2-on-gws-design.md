# `evsocket/v2` On `gws` Design

**Goal**

Build a new `evsocket/v2` package that preserves the facade-level programming model of `SocketInstance` while replacing the hand-written websocket runtime with `gws`.

**Compatibility boundary**

- `v1` remains untouched and is considered frozen.
- `v2` preserves the server-side `SocketInstance` facade and session-level methods commonly used in callbacks.
- `v2` does **not** expose a raw websocket connection object.
- Client APIs are allowed to break, but the object returned to callers must be the same session facade used on the server side.

**Public API shape**

- Package path: `github.com/PaienNate/pineutil/evsocket/v2`
- Keep package name `socketio` for continuity.
- Public root objects:
  - `SocketInstance`
  - `WebsocketWrapper` (facade session object)
  - `EventPayload`
- Server entry:
  - `NewSocketInstance()`
  - `(*SocketInstance).New(callback func(kws *WebsocketWrapper)) http.HandlerFunc`
- Client entry:
  - `(*SocketInstance).Dial(url string, options ClientOptions) (*WebsocketWrapper, error)`
  - `(*WebsocketWrapper).Reconnect() error`

**Responsibilities**

- `SocketInstance`
  - connection registration/removal
  - UUID routing
  - global event listeners
  - broadcast and targeted emit
  - shutdown
- `WebsocketWrapper`
  - stable facade object for one logical session
  - UUID, attributes, alive state
  - emit/close/reconnect operations
  - optional convenience callbacks that bridge from the event bus
- internal transport adapter
  - owns `gws.Conn`
  - bridges `OnOpen`, `OnMessage`, `OnPing`, `OnPong`, `OnClose`
  - handles read loop and ping/pong

**Lifecycle design**

- A `WebsocketWrapper` is a stable logical session object.
- The active transport underneath it can change during reconnect.
- Server sessions are created on successful upgrade.
- Client sessions are created on successful dial.
- Global events and wrapper convenience callbacks are always driven from the same internal event dispatch path.

**Testing strategy**

- Write comprehensive tests in `evsocket/v2` before implementing behavior.
- Test facade behavior, not transport internals.
- Cover:
  - listener registration and event dispatch
  - message send/receive
  - targeted routing and broadcast
  - disconnect events
  - stable attributes snapshots
  - client dial and reconnect
  - shutdown semantics

**Migration**

- `v1` stays available.
- `v2` ships with a migration document covering:
  - changed import path
  - removal of raw `Conn`
  - client API change from `NewClient`/`ClientConnect` to `Dial`/`Reconnect`
  - using the same facade object for both server and client sessions
