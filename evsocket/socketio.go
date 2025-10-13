package socketio

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Source @url:https://github.com/gorilla/websocket/blob/master/conn.go#L61
// The message types are defined in RFC 6455, section 11.8.
const (
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	TextMessage = 1
	// BinaryMessage denotes a binary data message.
	BinaryMessage = 2
	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	CloseMessage = 8
	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PingMessage = 9
	// PongMessage denotes a pong control message. The optional message payload
	// is UTF-8 encoded text.
	PongMessage = 10
)

// Supported event list
// 考虑将EventMessage等增加一个hash，这样就不会和正常的重复。
const (
	// EventMessage Fired when a Text/Binary message is received
	EventMessage = "message"
	// EventPing More details here:
	// @url https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets
	EventPing = "ping"
	EventPong = "pong"
	// EventDisconnect Fired on disconnection
	// The error provided in disconnection event
	// is defined in RFC 6455, section 11.7.
	// @url https://github.com/gofiber/websocket/blob/cd4720c435de415b864d975a9ca23a47eaf081ef/websocket.go#L192
	EventDisconnect = "disconnect"
	// EventConnect Fired on first connection
	EventConnect = "connect"
	// EventClose Fired when the connection is actively closed from the server
	EventClose = "close"
	// EventError Fired when some error appears useful also for debugging websockets
	EventError = "error"
)

var (
	// ErrorInvalidConnection The addressed Conn connection is not available anymore
	// error data is the uuid of that connection
	ErrorInvalidConnection = errors.New("message cannot be delivered invalid/gone connection")
	// ErrorUUIDDuplication The UUID already exists in the pool
	ErrorUUIDDuplication = errors.New("UUID already exists in the available connections pool")
	ErrorUUIDIsEmpty     = errors.New("SingleMode with UUID empty is prohibited")
	// ErrorClosedByOperator The connection is closed as there has a new connection
	ErrorClosedError = errors.New("UUID already exists in the available connections pool")

	// 新增：详细错误类型
	ErrSessionClosed      = errors.New("websocket session is closed")
	ErrMessageBufferFull  = errors.New("message buffer is full")
	ErrWriteTimeout       = errors.New("write operation timeout")
	ErrReadTimeout        = errors.New("read operation timeout")
	ErrInvalidMessageType = errors.New("invalid message type")
	ErrConnectionLost     = errors.New("websocket connection lost")
)

// SocketError 错误详情结构
type SocketError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	UUID    string `json:"uuid,omitempty"`
	Cause   error  `json:"-"`
}

func (e *SocketError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// 错误代码常量
const (
	ErrCodeConnectionClosed = 1001
	ErrCodeBufferFull       = 1002
	ErrCodeTimeout          = 1003
	ErrCodeInvalidUUID      = 1004
	ErrCodeDuplicateUUID    = 1005
	ErrCodeSessionClosed    = 1006
	ErrCodeInvalidMessage   = 1007
	ErrCodeConnectionLost   = 1008
)

var (
	PongTimeout = 1 * time.Second
	// RetrySendTimeout retry after 20 ms if there is an error
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry define max retries if there are socket issues
	MaxSendRetry = 5
	// ReadTimeout Instead of reading in a for loop, try to avoid full CPU load taking some pause
	ReadTimeout = 10 * time.Millisecond
)

// ServerOptions 服务端配置结构体（与现有 ConnectionOptions 保持一致的设计风格）
type ServerOptions struct {
	// 基础配置
	WriteWait         time.Duration `json:"write_wait"`
	PongWait          time.Duration `json:"pong_wait"`
	PingPeriod        time.Duration `json:"ping_period"`
	MaxMessageSize    int64         `json:"max_message_size"`
	MessageBufferSize int           `json:"message_buffer_size"`

	// 并发与互斥控制
	ConcurrentMessageHandling bool `json:"concurrent_message_handling"`
	EnableMutexGuard          bool `json:"enable_mutex_guard"`

	// 重试与超时
	MaxSendRetry     int           `json:"max_send_retry"`
	RetrySendTimeout time.Duration `json:"retry_send_timeout"`
	ReadTimeout      time.Duration `json:"read_timeout"`

	// 错误与日志
	EnableDetailedErrors bool `json:"enable_detailed_errors"`
	ErrorBufferSize      int  `json:"error_buffer_size"`
	EnableLogging        bool `json:"enable_logging"`
}

// DefaultServerOptions 默认服务端配置函数
func DefaultServerOptions() *ServerOptions {
	return &ServerOptions{
		WriteWait:                 10 * time.Second,
		PongWait:                  PongTimeout,
		PingPeriod:                54 * time.Second,
		MaxMessageSize:            512,
		MessageBufferSize:         256,
		ConcurrentMessageHandling: false,
		EnableMutexGuard:          false,
		MaxSendRetry:              MaxSendRetry,
		RetrySendTimeout:          RetrySendTimeout,
		ReadTimeout:               ReadTimeout,
		EnableDetailedErrors:      false,
		ErrorBufferSize:           100,
		EnableLogging:             false,
	}
}

// Raw form of websocket message
type message struct {
	// Message type
	mType int
	// Message data
	data []byte
	// Message send retries when error
	retries int

	// 新增：过滤器函数（可选）
	filter func(ws) bool `json:"-"`

	// 新增：目标 UUID 列表（与过滤器互斥）
	targetUUIDs []string `json:"target_uuids,omitempty"`
}

// FilterFunc 定义过滤器函数类型
type FilterFunc func(ws) bool

// 预定义的常用过滤器
var (
	// FilterAlive 过滤活跃连接
	FilterAlive FilterFunc = func(kws ws) bool {
		return kws.IsAlive()
	}
)

// FilterByAttribute 过滤具有特定属性的连接
func FilterByAttribute(key string, value interface{}) FilterFunc {
	return func(kws ws) bool {
		return kws.GetAttribute(key) == value
	}
}

// FilterExcludeUUID 排除特定 UUID
func FilterExcludeUUID(excludeUUID string) FilterFunc {
	return func(kws ws) bool {
		return kws.GetUUID() != excludeUUID
	}
}

// FilterByUUIDs 只包含指定的 UUID 列表
func FilterByUUIDs(uuids []string) FilterFunc {
	uuidMap := make(map[string]bool)
	for _, uuid := range uuids {
		uuidMap[uuid] = true
	}
	return func(kws ws) bool {
		return uuidMap[kws.GetUUID()]
	}
}

// FilterCombineAND 组合多个过滤器（AND 逻辑）
func FilterCombineAND(filters ...FilterFunc) FilterFunc {
	return func(kws ws) bool {
		for _, filter := range filters {
			if !filter(kws) {
				return false
			}
		}
		return true
	}
}

// FilterCombineOR 组合多个过滤器（OR 逻辑）
func FilterCombineOR(filters ...FilterFunc) FilterFunc {
	return func(kws ws) bool {
		for _, filter := range filters {
			if filter(kws) {
				return true
			}
		}
		return false
	}
}

type WebsocketWrapper struct {
	once sync.Once
	mu   sync.RWMutex
	// websocket raw
	Conn *websocket.Conn
	// Define if the connection is alive or not
	isAlive bool
	// Queue of messages sent from the socket
	queue chan message
	// Channel to signal when this websocket is closed
	// so go routines will stop gracefully
	done chan struct{}
	// Attributes map collection for the connection
	attributes map[string]interface{}
	// Unique id of the connection
	UUID string
	// Manager
	manager *SocketInstance
	// ClientMode Needed
	WebsocketDialer   *websocket.Dialer
	URL               string
	ConnectionOptions ConnectionOptions
	RequestHeader     http.Header
	ClientFlag        bool

	// GoWebSocket 风格的快捷回调（可选）
	OnConnected     func()
	OnDisconnected  func(err error)
	OnConnectError  func(err error)
	OnTextMessage   func(message string)
	OnBinaryMessage func(data []byte)
	OnPingReceived  func(data string)
	OnPongReceived  func(data string)

	// 可选的互斥保护
	sendMu    sync.Mutex
	receiveMu sync.Mutex
}

// ConnectionOptions Copied from gowebsocket.
type ConnectionOptions struct {
	UseCompression bool
	UseSSL         bool
	Proxy          func(*http.Request) (*url.URL, error)
	Subprotocols   []string
	// 过期时间 TODO: 存疑是否可以使用
	Timeout time.Duration
}

// EventPayload Event Payload is the object that
// stores all the information about the event and
// the connection
type EventPayload struct {
	// The connection object
	Kws *WebsocketWrapper
	// The name of the event
	Name string
	// Unique connection UUID
	SocketUUID string
	// Optional websocket attributes
	SocketAttributes map[string]any
	// Optional error when are fired events like
	// - Disconnect
	// - Error
	Error error
	// Data is used on Message and on Error event
	Data []byte
}
type eventCallback func(payload *EventPayload)

type ws interface {
	IsAlive() bool
	GetUUID() string
	SetUUID(uuid string) error
	SetAttribute(key string, attribute interface{})
	GetAttribute(key string) interface{}
	GetIntAttribute(key string) int
	GetStringAttribute(key string) string
	EmitToList(uuids []string, message []byte, mType ...int)
	EmitTo(uuid string, message []byte, mType ...int) error
	Broadcast(message []byte, except bool, mType ...int)
	Fire(event string, data []byte)
	Emit(message []byte, mType ...int)
	Close()
	pong(ctx context.Context)
	write(messageType int, messageBytes []byte)
	run()
	read(ctx context.Context)
	disconnected(err error)
	createUUID() string
	randomUUID() string
	fireEvent(event string, data []byte, error error)
}

// 公共方法抽离

type SocketInstance struct {
	pool      safePool
	listeners safeListeners
	// 是否不允许多连接。启动后，第二个连接时，会自动踹掉
	SingleMode atomic.Bool
	// 当使用上述模式时，使用的UUID是什么（必填）
	SingleModeUUID string
	// ServerOpts 服务端配置（可选，保持向后兼容）
	ServerOpts *ServerOptions `json:"server_options,omitempty"`
}

func NewSocketInstance() *SocketInstance {
	// 使用SyncMap后，可以直接省略这里的初始化
	return &SocketInstance{
		pool:      safePool{},
		listeners: safeListeners{},
	}
}

// NewSocketInstanceWithServerOptions 新增：带服务端配置的构造函数
func NewSocketInstanceWithServerOptions(opts *ServerOptions) *SocketInstance {
	if opts == nil {
		opts = DefaultServerOptions()
	}
	return &SocketInstance{
		pool:       safePool{},
		listeners:  safeListeners{},
		ServerOpts: opts,
	}
}

// 配置获取方法（向后兼容）
func (sm *SocketInstance) getWriteWait() time.Duration {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.WriteWait
	}
	return 10 * time.Second // 默认值
}

func (sm *SocketInstance) getPongWait() time.Duration {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.PongWait
	}
	return PongTimeout
}

func (sm *SocketInstance) getMaxSendRetry() int {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.MaxSendRetry
	}
	return MaxSendRetry
}

func (sm *SocketInstance) isConcurrentHandlingEnabled() bool {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.ConcurrentMessageHandling
	}
	return false
}

func (sm *SocketInstance) getMessageBufferSize() int {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.MessageBufferSize
	}
	return 256 // 默认值
}

func (sm *SocketInstance) isDetailedErrorsEnabled() bool {
	if sm.ServerOpts != nil {
		return sm.ServerOpts.EnableDetailedErrors
	}
	return false
}

// EmitToList Emit the message to a specific socket uuids list
// Ignores all errors
func (sm *SocketInstance) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = sm.EmitTo(wsUUID, message, mType...)
	}
}

// Broadcast to all the active connections
// 在客户端模式下，这个UUID列表实际上只有一个，所以做了一点无用功，但性能影响应该不大
func (sm *SocketInstance) Broadcast(message []byte, mType ...int) {
	for _, kws := range sm.pool.all() {
		kws.Emit(message, mType...)
	}
}

// BroadcastFilter 新增：基于过滤器的广播方法
func (sm *SocketInstance) BroadcastFilter(message []byte, filter FilterFunc, mType ...int) {
	for _, kws := range sm.pool.all() {
		if filter == nil || filter(kws) {
			kws.Emit(message, mType...)
		}
	}
}

// BroadcastExcept 新增：排除特定连接的广播
func (sm *SocketInstance) BroadcastExcept(message []byte, exceptUUID string, mType ...int) {
	sm.BroadcastFilter(message, func(kws ws) bool {
		return kws.GetUUID() != exceptUUID
	}, mType...)
}

// EmitTo Emit to a specific socket connection
func (sm *SocketInstance) EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := sm.pool.get(uuid)
	if err != nil {
		return err
	}

	if !sm.pool.contains(uuid) || !conn.IsAlive() {
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// Fire custom event on all connections
func (sm *SocketInstance) Fire(event string, data []byte) {
	sm.fireGlobalEvent(event, data, nil)
}

// Fires event on all connections.
// 在客户端模式下，这个UUID列表实际上只有一个，所以做了一点无用功，但性能影响应该不大
func (sm *SocketInstance) fireGlobalEvent(event string, data []byte, error error) {
	for _, kws := range sm.pool.all() {
		kws.fireEvent(event, data, error)
	}
}

// On Add listener callback for an event into the listeners list
func (sm *SocketInstance) On(event string, callback eventCallback) {
	sm.listeners.set(event, callback)
}

func fakeNewWrapper(handler func(conn *websocket.Conn), conn *websocket.Conn) http.HandlerFunc {
	return func(_ http.ResponseWriter, _ *http.Request) {
		handler(conn)
	}
}

func (sm *SocketInstance) New(callback func(kws *WebsocketWrapper), conn *websocket.Conn) (http.HandlerFunc, error) {
	// 如果是单UUID Mode，需要踹掉上一个连接 注意不要触发检查
	if sm.SingleMode.Load() && sm.SingleModeUUID == "" {
		return nil, ErrorUUIDIsEmpty
	}
	// 这个函数不报错的原因是因为conn部分已经被初始化过了
	tempFunction := func(c *websocket.Conn) {
		kws := &WebsocketWrapper{
			Conn:       c,
			queue:      make(chan message, 100),
			done:       make(chan struct{}, 1),
			attributes: make(map[string]interface{}),
			isAlive:    true,
			manager:    sm,
		}

		// 每次查找原本的pool有没有，有的话踹掉
		if sm.SingleMode.Load() {
			kws.UUID = sm.SingleModeUUID
			if sm.pool.contains(kws.UUID) {
				get, err := sm.pool.get(kws.UUID)
				if err != nil {
					return
				}
				get.disconnected(ErrorClosedError)
				sm.pool.delete(kws.UUID)
			}
		} else {
			// Generate uuid
			kws.UUID = kws.createUUID()
		}
		// register the connection into the pool
		sm.pool.set(kws)

		// execute the callback of the socket initialization
		callback(kws)

		kws.fireEvent(EventConnect, nil, nil)

		// Run the loop for the given connection
		kws.run()
	}
	return fakeNewWrapper(tempFunction, conn), nil
}

func (kws *WebsocketWrapper) setConnectionOptions() {
	kws.WebsocketDialer.EnableCompression = kws.ConnectionOptions.UseCompression
	kws.WebsocketDialer.TLSClientConfig =
		&tls.Config{InsecureSkipVerify: kws.ConnectionOptions.UseSSL} //nolint:gosec // 考虑到部分情况下，确实证书是自签的难以信任
	kws.WebsocketDialer.Proxy = kws.ConnectionOptions.Proxy
	kws.WebsocketDialer.Subprotocols = kws.ConnectionOptions.Subprotocols
}
func (kws *WebsocketWrapper) Connect() error {
	var err error
	var resp *http.Response
	kws.setConnectionOptions()

	// 应用Dialer配置
	applyDialerOptions(kws.WebsocketDialer, kws.ConnectionOptions)

	kws.Conn, resp, err = kws.WebsocketDialer.Dial(kws.URL, kws.RequestHeader)
	if err != nil {
		// logger.Error.Println("Error while connecting to server ", err)
		if resp != nil {
			// logger.Error.Println("HTTP Response %d status: %s", resp.StatusCode, resp.Status)
		}
		// 调用连接错误回调
		if kws.OnConnectError != nil {
			kws.OnConnectError(err)
		}
		return err
	}

	// 设置ClientFlag = true
	kws.ClientFlag = true

	// 安装默认处理器
	kws.installDefaultHandlers()

	// 调用连接成功回调
	if kws.OnConnected != nil {
		kws.OnConnected()
	}

	// logger.Info.Println("Connected to server")
	return nil
}

// NewClient
func (sm *SocketInstance) NewClient(callback func(kws *WebsocketWrapper), url string, options ConnectionOptions) error {
	// 通过类似 gowebsocket 的初始化方式进行初始化
	kws := &WebsocketWrapper{
		queue:             make(chan message, 100),
		done:              make(chan struct{}, 1),
		attributes:        make(map[string]interface{}),
		isAlive:           true,
		manager:           sm,
		ConnectionOptions: options,
		URL:               url,
		// 客户端模式需要 Dialer 与 Header
		WebsocketDialer:   &websocket.Dialer{},
		RequestHeader:     http.Header{},
	}
	if err := kws.Connect(); err != nil {
		return err
	}
	// Generate uuid
	kws.UUID = kws.createUUID()

	// register the connection into the pool
	sm.pool.set(kws)

	// execute the callback of the socket initialization
	callback(kws)

	kws.fireEvent(EventConnect, nil, nil)

	// Run the loop for the given connection (非阻塞)
	go kws.run()
	return nil
}

func (kws *WebsocketWrapper) GetUUID() string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.UUID
}

func (kws *WebsocketWrapper) SetUUID(uuid string) error {
	// TODO: .
	kws.mu.Lock()
	defer kws.mu.Unlock()

	if kws.manager.pool.contains(uuid) {
		return ErrorUUIDDuplication
	}
	kws.UUID = uuid
	return nil
}

// SetAttribute Set a specific attribute for the specific socket connection
func (kws *WebsocketWrapper) SetAttribute(key string, attribute interface{}) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.attributes[key] = attribute
}

// GetAttribute Get a specific attribute from the socket attributes
func (kws *WebsocketWrapper) GetAttribute(key string) interface{} {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value
	}
	return nil
}

// GetIntAttribute Convenience method to retrieve an attribute as an int.
// Will panic if attribute is not an int.
func (kws *WebsocketWrapper) GetIntAttribute(key string) int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(int)
	}
	return 0
}

// GetStringAttribute Convenience method to retrieve an attribute as a string.
func (kws *WebsocketWrapper) GetStringAttribute(key string) string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(string)
	}
	return ""
}

// EmitToList Emit the message to a specific socket uuids list
func (kws *WebsocketWrapper) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// EmitTo Emit to a specific socket connection
func (kws *WebsocketWrapper) EmitTo(uuid string, message []byte, mType ...int) error {

	conn, err := kws.manager.pool.get(uuid)
	if err != nil {
		return err
	}
	if !kws.manager.pool.contains(uuid) || !conn.IsAlive() {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// Broadcast to all the active connections
// except avoid broadcasting the message to itself
func (kws *WebsocketWrapper) Broadcast(message []byte, except bool, mType ...int) {
	// 特殊判断：若是客户端，except能且只能是false
	if kws.ClientFlag {
		except = false
	}
	for wsUUID := range kws.manager.pool.all() {
		if except && kws.UUID == wsUUID {
			continue
		}
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// Fire custom event
func (kws *WebsocketWrapper) Fire(event string, data []byte) {
	kws.fireEvent(event, data, nil)
}

// Emit /Write the message into the given connection
func (kws *WebsocketWrapper) Emit(message []byte, mType ...int) {
	t := TextMessage
	if len(mType) > 0 {
		t = mType[0]
	}
	kws.write(t, message)
}

// Close Actively close the connection from the server
func (kws *WebsocketWrapper) Close() {
	kws.write(CloseMessage, []byte("Connection closed"))
	kws.fireEvent(EventClose, nil, nil)
}

func (kws *WebsocketWrapper) IsAlive() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.isAlive
}

func (kws *WebsocketWrapper) hasConn() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.Conn != nil
}

func (kws *WebsocketWrapper) setAlive(alive bool) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.isAlive = alive
}

func (kws *WebsocketWrapper) queueLength() int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return len(kws.queue)
}

// pong writes a control message to the client
func (kws *WebsocketWrapper) pong(ctx context.Context) {
	timeoutTicker := time.NewTicker(PongTimeout)
	defer timeoutTicker.Stop()
	for {
		select {
		case <-timeoutTicker.C:
			kws.write(PongMessage, []byte{})
		case <-ctx.Done():
			return
		}
	}
}

// Add in message queue
func (kws *WebsocketWrapper) write(messageType int, messageBytes []byte) {
	kws.queue <- message{
		mType:   messageType,
		data:    messageBytes,
		retries: 0,
	}
}

// Send out message queue
func (kws *WebsocketWrapper) send(ctx context.Context) {
	for {
		select {
		case rawMessage := <-kws.queue:
			if !kws.hasConn() {
				if rawMessage.retries <= MaxSendRetry {
					// retry without blocking the sending thread
					go func() {
						time.Sleep(RetrySendTimeout)
						rawMessage.retries++
						kws.queue <- rawMessage
					}()
				}
				continue
			}

			kws.mu.RLock()
			err := kws.Conn.WriteMessage(rawMessage.mType, rawMessage.data)
			kws.mu.RUnlock()

			if err != nil {
				kws.disconnected(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Start Pong/Read/Write functions
//
// Needs to be blocking, otherwise the connection would close.
func (kws *WebsocketWrapper) run() {
	ctx, cancelFunc := context.WithCancel(context.Background())

	go kws.pong(ctx)
	go kws.read(ctx)
	go kws.send(ctx)

	<-kws.done // block until one event is sent to the done channel

	cancelFunc()
}

// Listen for incoming messages
// and filter by message type
// handleMessage 处理接收到的消息，支持并发和顺序处理
func (kws *WebsocketWrapper) handleMessage(mType int, msg []byte) {
	// 根据不同信息类型，发送对应的数据
	// 收到PING和PONG信息时
	if mType == PingMessage {
		kws.fireEvent(EventPing, nil, nil)
		// 调用快捷回调
		if kws.OnPingReceived != nil {
			kws.OnPingReceived(string(msg))
		}
		return
	}

	if mType == PongMessage {
		kws.fireEvent(EventPong, nil, nil)
		// 调用快捷回调
		if kws.OnPongReceived != nil {
			kws.OnPongReceived(string(msg))
		}
		return
	}
	// 收到关闭消息时
	if mType == CloseMessage {
		kws.disconnected(nil)
		return
	}

	// 处理文本和二进制消息
	if mType == TextMessage {
		// 调用快捷回调
		if kws.OnTextMessage != nil {
			kws.OnTextMessage(string(msg))
		}
	} else if mType == BinaryMessage {
		// 调用快捷回调
		if kws.OnBinaryMessage != nil {
			kws.OnBinaryMessage(msg)
		}
	}

	// We have a message and we fire the message event
	kws.fireEvent(EventMessage, msg, nil)
}

func (kws *WebsocketWrapper) read(ctx context.Context) {
	timeoutTicker := time.NewTicker(ReadTimeout)
	defer timeoutTicker.Stop()
	for {
		select {
		case <-timeoutTicker.C:
			if !kws.hasConn() {
				continue
			}

			kws.mu.RLock()
			mType, msg, err := kws.Conn.ReadMessage()
			kws.mu.RUnlock()

			// 读取消息错误的时候断开连接
			if err != nil {
				kws.disconnected(err)
				return
			}

			// 检查是否启用并发消息处理
			if kws.manager != nil && kws.manager.isConcurrentHandlingEnabled() {
				// 并发处理：在新的goroutine中处理消息
				go kws.handleMessage(mType, msg)
			} else {
				// 顺序处理：在当前goroutine中处理消息
				kws.handleMessage(mType, msg)
				// 如果是关闭消息，需要退出读取循环
				if mType == CloseMessage {
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// When the connection closes, disconnected method
func (kws *WebsocketWrapper) disconnected(err error) {
	// 我们要在这里考虑判断逻辑，然后重新连接（若需要）
	kws.fireEvent(EventDisconnect, nil, err)

	// 调用快捷回调
	if kws.OnDisconnected != nil {
		kws.OnDisconnected(err)
	}

	// may be called multiple times from different go routines
	if kws.IsAlive() {
		kws.once.Do(func() {
			kws.setAlive(false)
			close(kws.done)
		})
	}

	// Fire error event if the connection is
	// disconnected by an error
	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}

	// Remove the socket from the pool
	kws.manager.pool.delete(kws.UUID)
}

// Create random UUID for each connection
func (kws *WebsocketWrapper) createUUID() string {
	return kws.randomUUID()
}

// Generate random UUID.
func (kws *WebsocketWrapper) randomUUID() string {
	return uuid.New().String()
}

// fireEvent 触发指定事件并调用所有监听该事件的回调函数。
// 参数:
//
//	event - 事件名称，用于识别要触发的事件。
//	data - 事件数据，以字节切片形式传递给回调函数。
//	error - 事件错误，传递给回调函数的错误信息（如果有）。
//
// applyDialerOptions 统一应用Dialer配置
func applyDialerOptions(d *websocket.Dialer, opts ConnectionOptions) {
	d.EnableCompression = opts.UseCompression
	if opts.UseSSL {
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	d.Proxy = opts.Proxy
	d.Subprotocols = opts.Subprotocols
}

// BuildProxy 构造代理函数
func BuildProxy(proxyURL string) (func(*http.Request) (*url.URL, error), error) {
	if proxyURL == "" {
		return nil, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %v", err)
	}

	return func(*http.Request) (*url.URL, error) {
		return u, nil
	}, nil
}

// installDefaultHandlers 安装默认的WebSocket处理器
func (kws *WebsocketWrapper) installDefaultHandlers() {
	if kws.Conn == nil {
		return
	}

	// 设置Ping处理器
	kws.Conn.SetPingHandler(func(data string) error {
		kws.fireEvent(EventPing, []byte(data), nil)
		if kws.OnPingReceived != nil {
			kws.OnPingReceived(data)
		}
		return nil
	})

	// 设置Pong处理器
	kws.Conn.SetPongHandler(func(data string) error {
		kws.fireEvent(EventPong, []byte(data), nil)
		if kws.OnPongReceived != nil {
			kws.OnPongReceived(data)
		}
		return nil
	})

	// 设置Close处理器
	kws.Conn.SetCloseHandler(func(code int, text string) error {
		kws.fireEvent(EventClose, []byte(text), nil)
		kws.disconnected(nil)
		return nil
	})
}

func (kws *WebsocketWrapper) fireEvent(event string, data []byte, error error) {
	// 获取指定事件的所有回调函数。
	callbacks := kws.manager.listeners.get(event)

	// 遍历所有回调函数并调用它们。
	for _, callback := range callbacks {
		// 创建 EventPayload 实例并填充它，以便传递给回调函数。
		callback(&EventPayload{
			Kws:              kws,
			Name:             event,
			SocketUUID:       kws.UUID,
			SocketAttributes: kws.attributes,
			Data:             data,
			Error:            error,
		})
	}
}
