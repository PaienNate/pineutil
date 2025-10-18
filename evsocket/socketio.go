package socketio

import (
	"context"
	"crypto/tls"
	"errors"
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

// Raw form of websocket message
type message struct {
	// Message type
	mType int
	// Message data
	data []byte
	// Message send retries when error
	retries int
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

	URL string
	ClientOptions
	WebsocketDialer *websocket.Dialer
	RequestHeader   http.Header
	ClientFlag      bool

	// 移植代码： GoWebSocket 风格的快捷回调
	OnConnected     func()
	OnDisconnected  func(err error)
	OnConnectError  func(err error)
	OnConnectFailed func(err error) // 当第一次尝试就失败的时候
	OnTextMessage   func(message string)
	OnBinaryMessage func(data []byte)
	OnPingReceived  func(data string)
	OnPongReceived  func(data string)
}

// ClientOptions Copied from gowebsocket.
type ClientOptions struct {
	UseCompression bool
	UseSSL         bool
	Proxy          func(*http.Request) (*url.URL, error)
	Subprotocols   []string
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
	// 实例是否已关闭
	isShutdown atomic.Bool
	// 升级的时候用的
	Upgrader *websocket.Upgrader
}

func NewSocketInstance() *SocketInstance {
	// 使用SyncMap后，可以直接省略这里的初始化
	instance := &SocketInstance{
		pool:      safePool{},
		listeners: safeListeners{},
		Upgrader: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
	instance.isShutdown.Store(false)
	return instance
}

// EmitToList Emit the message to a specific socket uuids list
// Ignores all errors
func (sm *SocketInstance) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = sm.EmitTo(wsUUID, message, mType...)
	}
}

// Broadcast to all the active connections
func (sm *SocketInstance) Broadcast(message []byte, mType ...int) {
	if sm.IsShutdown() {
		return
	}
	for _, kws := range sm.pool.all() {
		kws.Emit(message, mType...)
	}
}

// EmitTo Emit to a specific socket connection
func (sm *SocketInstance) EmitTo(uuid string, message []byte, mType ...int) error {
	if sm.IsShutdown() {
		return errors.New("socket instance has been shutdown")
	}
	
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
	if sm.IsShutdown() {
		return
	}
	sm.fireGlobalEvent(event, data, nil)
}

// Fires event on all connections.
// 在客户端模式下，这个UUID列表实际上只有一个，所以做了一点无用功，但性能影响应该不大
func (sm *SocketInstance) fireGlobalEvent(event string, data []byte, error error) {
	for _, kws := range sm.pool.all() {
		kws.fireEvent(event, data, error)
	}
}

// IsShutdown 检查实例是否已关闭
func (sm *SocketInstance) IsShutdown() bool {
	return sm.isShutdown.Load()
}

// Shutdown 优雅关闭SocketInstance，关闭所有连接并清理资源
func (sm *SocketInstance) Shutdown() error {
	// 防止重复关闭
	if !sm.isShutdown.CompareAndSwap(false, true) {
		return nil // 已经关闭了
	}

	// 获取所有连接并关闭
	connections := sm.pool.all()
	for _, conn := range connections {
		if conn.IsAlive() {
			// 发送关闭消息
			conn.Close()
			// 等待连接自然断开，由disconnected方法处理清理
		}
	}

	// 清理监听器
	sm.listeners = safeListeners{}
	
	return nil
}

// On Add listener callback for an event into the listeners list
func (sm *SocketInstance) On(event string, callback eventCallback) {
	if sm.IsShutdown() {
		return // 已关闭的实例不接受新的监听器
	}
	sm.listeners.set(event, callback)
}

func (sm *SocketInstance) New(callback func(kws *WebsocketWrapper)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := sm.Upgrader.Upgrade(w, r, w.Header())
		if err != nil {
			return
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
			// todo: 单模式
			kws.UUID = kws.createUUID()

			// register the connection into the pool
			sm.pool.set(kws)
			// 优化初始化顺序：先启动循环，再执行回调，最后触发连接事件。
			// 这样可以避免在回调中等待消息时产生阻塞（读写循环尚未启动）。
			// 启动读写与心跳循环（在独立协程中阻塞，直到连接关闭）
			go kws.run()

			// 执行用户的初始化回调（此时循环已启动，可安全发送/等待消息）
			callback(kws)

			// 触发连接事件（保持与旧行为一致：在 callback 之后触发）
			kws.fireEvent(EventConnect, nil, nil)

			// 保持 Handler 阻塞直至连接关闭，避免提前返回导致资源清理混乱
			<-kws.done
		}
		tempFunction(conn)
	}
}

// NewClient
func (sm *SocketInstance) NewClient(url string, options ClientOptions) *WebsocketWrapper {
	// 通过类似 gowebsocket 的初始化方式进行初始化
	kws := &WebsocketWrapper{
		queue:           make(chan message, 100),
		attributes:      make(map[string]interface{}),
		isAlive:         false,
		manager:         sm,
		ClientOptions:   options,
		URL:             url,
		WebsocketDialer: &websocket.Dialer{},
		RequestHeader:   http.Header{},
	}
	return kws
}

func (kws *WebsocketWrapper) ClientConnect(callback func(kws *WebsocketWrapper)) error {
	// 如果已经连接，直接返回
	if kws.IsAlive() {
		return nil
	}
	
	// 检查manager是否已关闭
	if kws.manager.IsShutdown() {
		return errors.New("socket instance has been shutdown")
	}
	
	// 如果是重连，先清理之前的资源
	if kws.UUID != "" && kws.done != nil {
		// 确保之前的连接已经完全关闭
		select {
		case <-kws.done:
			// 之前的连接已经关闭，可以继续
		default:
			// 强制关闭之前的连接
			if kws.Conn != nil {
				kws.Conn.Close()
			}
			close(kws.done)
		}
	}
	
	err := kws.connect()
	if err != nil {
		return err
	}

	// 只在第一次连接时生成 UUID 和注册到池中
	if kws.UUID == "" {
		kws.UUID = kws.createUUID()
		kws.manager.pool.set(kws)
	}

	// Run the loop for the given connection
	kws.done = make(chan struct{}, 1)
	// 重置 once
	kws.once = sync.Once{}
	// 优化初始化顺序：先启动循环，再执行回调，最后触发连接事件。
	// 这样可以避免在回调中等待消息时产生阻塞（读写循环尚未启动）。
	// 启动读写与心跳循环（在独立协程中阻塞，直到连接关闭）
	go kws.run()

	// 执行用户的初始化回调（此时循环已启动，可安全发送/等待消息）
	callback(kws)

	// 触发连接事件（保持与旧行为一致：在 callback 之后触发）
	kws.fireEvent(EventConnect, nil, nil)

	// 保持 Handler 阻塞直至连接关闭，避免提前返回导致资源清理混乱
	<-kws.done
	return nil
}

func (kws *WebsocketWrapper) setClientOptions() {
	kws.WebsocketDialer.EnableCompression = kws.ClientOptions.UseCompression
	kws.WebsocketDialer.TLSClientConfig =
		&tls.Config{InsecureSkipVerify: kws.ClientOptions.UseSSL} //nolint:gosec // 考虑到部分情况下，确实证书是自签的难以信任
	kws.WebsocketDialer.Proxy = kws.ClientOptions.Proxy
	kws.WebsocketDialer.Subprotocols = kws.ClientOptions.Subprotocols
}
func (kws *WebsocketWrapper) connect() error {
	var err error
	kws.setClientOptions()

	kws.Conn, _, err = kws.WebsocketDialer.Dial(kws.URL, kws.RequestHeader)
	if err != nil {
		// 调用连接错误回调
		kws.setAlive(false)
		if kws.OnConnectError != nil {
			kws.OnConnectError(err)
		}
		return err
	}
	// 客户端模式
	kws.ClientFlag = true
	kws.setAlive(true)
	// 调用连接成功回调
	if kws.OnConnected != nil {
		kws.OnConnected()
	}
	return nil
}

func (kws *WebsocketWrapper) GetUUID() string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.UUID
}

func (kws *WebsocketWrapper) SetUUID(uuid string) error {
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
	if kws.manager.IsShutdown() {
		return
	}
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

			switch {
			case mType == PingMessage:
				kws.fireEvent(EventPing, nil, nil)
				if kws.OnPingReceived != nil {
					kws.OnPingReceived(string(msg))
				}
			case mType == PongMessage:
				kws.fireEvent(EventPong, nil, nil)
				if kws.OnPongReceived != nil {
					kws.OnPongReceived(string(msg))
				}
			case mType == CloseMessage:
				kws.disconnected(nil)
				return
			case mType == TextMessage:
				if kws.OnTextMessage != nil {
					kws.OnTextMessage(string(msg))
				}
				kws.fireEvent(EventMessage, msg, nil)
			case mType == BinaryMessage:
				if kws.OnBinaryMessage != nil {
					kws.OnBinaryMessage(msg)
				}
				kws.fireEvent(EventMessage, msg, nil)
			case err != nil:
				// 此处的快捷通知已经在函数内了
				kws.disconnected(err)
				return
			default:
				kws.fireEvent(EventMessage, msg, nil)
			}

		case <-ctx.Done():
			return
		}
	}
}

// When the connection closes, disconnected method
func (kws *WebsocketWrapper) disconnected(err error) {
	// may be called multiple times from different go routines
	if kws.IsAlive() {
		kws.once.Do(func() {
			kws.setAlive(false)
			// 客户端情况下显式关闭底层连接
			if kws.ClientFlag && kws.Conn != nil {
				kws.Conn.Close()
			}
			close(kws.done)
		})
	}

	// 发送断线等逻辑
	kws.fireEvent(EventDisconnect, nil, err)

	if kws.OnDisconnected != nil {
		kws.OnDisconnected(err)
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
