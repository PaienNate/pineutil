package socketio

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const numTestConn = 10

// testWS wraps WebsocketWrapper and overrides Emit for testing without real network.
type testWS struct {
	WebsocketWrapper
	emitWG    *sync.WaitGroup
	emitCount *int32 // 用于计数Emit调用
}

func newTestWS(uuid string, sm *SocketInstance, wg *sync.WaitGroup) *testWS {
	t := &testWS{
		WebsocketWrapper: WebsocketWrapper{
			attributes: make(map[string]interface{}),
			isAlive:    true,
			UUID:       uuid,
			manager:    sm,
			queue:      make(chan message, 1),
			done:       make(chan struct{}),
		},
		emitWG:    wg,
		emitCount: new(int32),
	}
	return t
}

// Emit override: just signal the waitgroup to confirm broadcast/delivery happened.
func (t *testWS) Emit(_ []byte, _ ...int) {
	if t.emitWG != nil {
		t.emitWG.Done()
	}
	if t.emitCount != nil {
		atomic.AddInt32(t.emitCount, 1)
	}
}

func TestGlobalFire(t *testing.T) {
	sm := NewSocketInstance()

	var wg sync.WaitGroup
	var called int32

	// register custom event handler
	sm.On("customevent", func(_ *EventPayload) {
		atomic.AddInt32(&called, 1)
		wg.Done()
	})

	// simulate connections
	for i := 0; i < numTestConn; i++ {
		wg.Add(1)
		kws := newTestWS("id-"+string(rune('a'+i)), sm, nil)
		sm.pool.set(kws)
	}

	// fire global custom event on all connections
	sm.Fire("customevent", []byte("test"))

	wg.Wait()
	if atomic.LoadInt32(&called) != int32(numTestConn) {
		t.Fatalf("expected %d calls, got %d", numTestConn, called)
	}
}

func TestGlobalBroadcast(t *testing.T) {
	sm := NewSocketInstance()

	var wg sync.WaitGroup

	for i := 0; i < numTestConn; i++ {
		wg.Add(1)
		kws := newTestWS("id-"+string(rune('a'+i)), sm, &wg)
		sm.pool.set(kws)
	}

	// send global broadcast to all connections
	sm.Broadcast([]byte("test"))

	wg.Wait()
}

func TestGlobalEmitTo(t *testing.T) {
	sm := NewSocketInstance()

	aliveUUID := "alive-uuid"
	closedUUID := "closed-uuid"

	aliveWG := &sync.WaitGroup{}
	aliveWG.Add(1)
	alive := newTestWS(aliveUUID, sm, aliveWG)
	sm.pool.set(alive)

	closed := newTestWS(closedUUID, sm, nil)
	closed.setAlive(false)
	sm.pool.set(closed)

	// non-existent
	if err := sm.EmitTo("non-existent", []byte("error")); err != ErrorInvalidConnection {
		t.Fatalf("expected ErrorInvalidConnection for non-existent, got: %v", err)
	}

	// closed
	if err := sm.EmitTo(closedUUID, []byte("error")); err != ErrorInvalidConnection {
		t.Fatalf("expected ErrorInvalidConnection for closed, got: %v", err)
	}

	// alive
	if err := sm.EmitTo(aliveUUID, []byte("test")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	aliveWG.Wait()
}

func TestGlobalEmitToList(t *testing.T) {
	sm := NewSocketInstance()

	uuids := []string{"id-1", "id-2", "id-3"}
	var wg sync.WaitGroup
	wg.Add(len(uuids))

	for _, id := range uuids {
		kws := newTestWS(id, sm, &wg)
		sm.pool.set(kws)
	}

	sm.EmitToList(uuids, []byte("test"))
	wg.Wait()
}

func TestWebsocket_GetIntAttribute(t *testing.T) {
	kws := &WebsocketWrapper{attributes: make(map[string]interface{})}

	// unset returns 0
	vUnset := kws.GetIntAttribute("unset")
	if vUnset != 0 {
		t.Fatalf("expected 0 for unset int attribute, got %d", vUnset)
	}

	// non-int panics
	kws.SetAttribute("notInt", "")
	assertPanic(t, func() {
		_ = kws.GetIntAttribute("notInt")
	})

	// set int returns value
	kws.SetAttribute("int", 3)
	v := kws.GetIntAttribute("int")
	if v != 3 {
		t.Fatalf("expected 3, got %d", v)
	}
}

func TestWebsocket_GetStringAttribute(t *testing.T) {
	kws := &WebsocketWrapper{attributes: make(map[string]interface{})}

	// unset returns ""
	vUnset := kws.GetStringAttribute("unset")
	if vUnset != "" {
		t.Fatalf("expected empty string for unset attribute, got %q", vUnset)
	}

	// non-string panics
	kws.SetAttribute("notString", 3)
	assertPanic(t, func() {
		_ = kws.GetStringAttribute("notString")
	})

	// set string returns value
	kws.SetAttribute("str", "3")
	v := kws.GetStringAttribute("str")
	if v != "3" {
		t.Fatalf("expected \"3\", got %q", v)
	}
}

func assertPanic(t *testing.T, f func()) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("The code did not panic")
		}
	}()
	f()
}

// ------------------------
// Integration tests with real network using gorilla/websocket
// ------------------------

func startTestServer(sm *SocketInstance) (string, func(), error) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h, err := sm.New(func(kws *WebsocketWrapper) {}, conn)
		if err != nil {
			_ = conn.Close()
			return
		}
		h(w, r)
	})

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go srv.Serve(ln)

	serverURL := "ws://" + ln.Addr().String() + "/ws"
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
	}
	return serverURL, cleanup, nil
}

func TestIntegration_Echo(t *testing.T) {
	sm := NewSocketInstance()

	// Echo on message
	sm.On(EventMessage, func(payload *EventPayload) {
		if string(payload.Data) == "hello" {
			payload.Kws.Emit([]byte("world"))
		}
	})

	url, cleanup, err := startTestServer(sm)
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer cleanup()

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(msg) != "world" {
		t.Fatalf("expected 'world', got %q", msg)
	}
}

/*
func TestIntegration_Broadcast(t *testing.T) {
	sm := NewSocketInstance()
	url, cleanup, err := startTestServer(sm)
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer cleanup()

	var connWG sync.WaitGroup
	connWG.Add(2)
	sm.On(EventConnect, func(_ *EventPayload) {
		connWG.Done()
	})

	c1, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial1 failed: %v", err)
	}
	defer c1.Close()

	c2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial2 failed: %v", err)
	}
	defer c2.Close()

	connWG.Wait()

	sm.Broadcast([]byte("srv"))

	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, m1, err := c1.ReadMessage()
	if err != nil {
		t.Fatalf("read1 failed: %v", err)
	}
	if string(m1) != "srv" {
		t.Fatalf("expected 'srv' on c1, got %q", m1)
	}

	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, m2, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("read2 failed: %v", err)
	}
	if string(m2) != "srv" {
		t.Fatalf("expected 'srv' on c2, got %q", m2)
	}
}
*/

func TestIntegration_EmitTo(t *testing.T) {
	sm := NewSocketInstance()

	// Register client IDs sent as first message: "ID:<name>"
	var msgWG sync.WaitGroup
	msgWG.Add(2)
	sm.On(EventMessage, func(payload *EventPayload) {
		msg := string(payload.Data)
		if strings.HasPrefix(msg, "ID:") {
			payload.Kws.SetAttribute("client_id", strings.TrimPrefix(msg, "ID:"))
			msgWG.Done()
		}
	})

	url, cleanup, err := startTestServer(sm)
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer cleanup()

	c1, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial1 failed: %v", err)
	}
	defer c1.Close()
	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c1.WriteMessage(websocket.TextMessage, []byte("ID:client1")); err != nil {
		t.Fatalf("c1 write failed: %v", err)
	}

	c2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial2 failed: %v", err)
	}
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c2.WriteMessage(websocket.TextMessage, []byte("ID:client2")); err != nil {
		t.Fatalf("c2 write failed: %v", err)
	}

	// Wait for ID messages to be processed and attributes set
	msgWG.Wait()

	// Find server-side UUID for client2
	var targetUUID string
	for uuid, w := range sm.pool.all() {
		if w.GetStringAttribute("client_id") == "client2" {
			targetUUID = uuid
			break
		}
	}
	if targetUUID == "" {
		t.Fatalf("failed to find target uuid for client2")
	}

	// Emit to client2 only
	if err := sm.EmitTo(targetUUID, []byte("target")); err != nil {
		t.Fatalf("EmitTo failed: %v", err)
	}

	// client2 should receive the message
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg2, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("c2 read failed: %v", err)
	}
	if string(msg2) != "target" {
		t.Fatalf("expected 'target' on c2, got %q", msg2)
	}

	// client1 should NOT receive the message within short time
	c1.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := c1.ReadMessage(); err == nil {
		t.Fatalf("c1 should not receive target message")
	}
}

// TestServerOptions 测试ServerOptions配置功能
func TestServerOptions(t *testing.T) {
	// 测试默认配置
	defaultOpts := DefaultServerOptions()
	if defaultOpts.WriteWait != 10*time.Second {
		t.Errorf("Expected WriteWait to be 10s, got %v", defaultOpts.WriteWait)
	}
	if defaultOpts.MessageBufferSize != 256 {
		t.Errorf("Expected MessageBufferSize to be 256, got %d", defaultOpts.MessageBufferSize)
	}
	if defaultOpts.ConcurrentMessageHandling != false {
		t.Errorf("Expected ConcurrentMessageHandling to be false, got %v", defaultOpts.ConcurrentMessageHandling)
	}

	// 测试自定义配置
	customOpts := &ServerOptions{
		WriteWait:                 5 * time.Second,
		MessageBufferSize:         512,
		ConcurrentMessageHandling: true,
		EnableDetailedErrors:      true,
	}

	sm := NewSocketInstanceWithServerOptions(customOpts)
	if sm.ServerOpts.WriteWait != 5*time.Second {
		t.Errorf("Expected custom WriteWait to be 5s, got %v", sm.ServerOpts.WriteWait)
	}
	if sm.ServerOpts.MessageBufferSize != 512 {
		t.Errorf("Expected custom MessageBufferSize to be 512, got %d", sm.ServerOpts.MessageBufferSize)
	}
	if !sm.ServerOpts.ConcurrentMessageHandling {
		t.Errorf("Expected ConcurrentMessageHandling to be true")
	}

	// 测试配置获取方法
	if sm.getWriteWait() != 5*time.Second {
		t.Errorf("Expected getWriteWait to return 5s, got %v", sm.getWriteWait())
	}
	if sm.getMessageBufferSize() != 512 {
		t.Errorf("Expected getMessageBufferSize to return 512, got %d", sm.getMessageBufferSize())
	}
	if !sm.isConcurrentHandlingEnabled() {
		t.Errorf("Expected isConcurrentHandlingEnabled to return true")
	}
	if !sm.isDetailedErrorsEnabled() {
		t.Errorf("Expected isDetailedErrorsEnabled to return true")
	}
}

// TestServerOptionsWithNil 测试传入nil配置时使用默认配置
func TestServerOptionsWithNil(t *testing.T) {
	sm := NewSocketInstanceWithServerOptions(nil)
	if sm.ServerOpts == nil {
		t.Errorf("Expected ServerOpts to be set to default when nil is passed")
	}
	if sm.getWriteWait() != 10*time.Second {
		t.Errorf("Expected default WriteWait to be 10s, got %v", sm.getWriteWait())
	}
}

// TestFilterAlive 测试FilterAlive过滤器
func TestFilterAlive(t *testing.T) {
	sm := NewSocketInstance()

	// 创建测试连接
	aliveWS := newTestWS("alive-uuid", sm, nil)
	aliveWS.setAlive(true)

	deadWS := newTestWS("dead-uuid", sm, nil)
	deadWS.setAlive(false)

	// 测试FilterAlive
	if !FilterAlive(aliveWS) {
		t.Errorf("FilterAlive should return true for alive connection")
	}
	if FilterAlive(deadWS) {
		t.Errorf("FilterAlive should return false for dead connection")
	}
}

// TestFilterByAttribute 测试FilterByAttribute过滤器
func TestFilterByAttribute(t *testing.T) {
	sm := NewSocketInstance()

	// 创建测试连接并设置属性
	ws1 := newTestWS("uuid1", sm, nil)
	ws1.SetAttribute("role", "admin")
	ws1.SetAttribute("level", 5)

	ws2 := newTestWS("uuid2", sm, nil)
	ws2.SetAttribute("role", "user")
	ws2.SetAttribute("level", 1)

	// 测试字符串属性过滤
	adminFilter := FilterByAttribute("role", "admin")
	if !adminFilter(ws1) {
		t.Errorf("FilterByAttribute should return true for admin role")
	}
	if adminFilter(ws2) {
		t.Errorf("FilterByAttribute should return false for non-admin role")
	}

	// 测试数字属性过滤
	levelFilter := FilterByAttribute("level", 5)
	if !levelFilter(ws1) {
		t.Errorf("FilterByAttribute should return true for level 5")
	}
	if levelFilter(ws2) {
		t.Errorf("FilterByAttribute should return false for level 1")
	}
}

// TestFilterExcludeUUID 测试FilterExcludeUUID过滤器
func TestFilterExcludeUUID(t *testing.T) {
	sm := NewSocketInstance()

	ws1 := newTestWS("uuid1", sm, nil)
	ws2 := newTestWS("uuid2", sm, nil)

	excludeFilter := FilterExcludeUUID("uuid1")
	if excludeFilter(ws1) {
		t.Errorf("FilterExcludeUUID should return false for excluded UUID")
	}
	if !excludeFilter(ws2) {
		t.Errorf("FilterExcludeUUID should return true for non-excluded UUID")
	}
}

// TestFilterByUUIDs 测试FilterByUUIDs过滤器
func TestFilterByUUIDs(t *testing.T) {
	sm := NewSocketInstance()

	ws1 := newTestWS("uuid1", sm, nil)
	ws2 := newTestWS("uuid2", sm, nil)
	ws3 := newTestWS("uuid3", sm, nil)

	includeFilter := FilterByUUIDs([]string{"uuid1", "uuid3"})
	if !includeFilter(ws1) {
		t.Errorf("FilterByUUIDs should return true for included UUID1")
	}
	if includeFilter(ws2) {
		t.Errorf("FilterByUUIDs should return false for non-included UUID2")
	}
	if !includeFilter(ws3) {
		t.Errorf("FilterByUUIDs should return true for included UUID3")
	}
}

// TestFilterCombineAND 测试FilterCombineAND过滤器
func TestFilterCombineAND(t *testing.T) {
	sm := NewSocketInstance()

	ws1 := newTestWS("uuid1", sm, nil)
	ws1.setAlive(true)
	ws1.SetAttribute("role", "admin")

	ws2 := newTestWS("uuid2", sm, nil)
	ws2.setAlive(true)
	ws2.SetAttribute("role", "user")

	ws3 := newTestWS("uuid3", sm, nil)
	ws3.setAlive(false)
	ws3.SetAttribute("role", "admin")

	// 组合过滤器：活跃且为admin
	combinedFilter := FilterCombineAND(FilterAlive, FilterByAttribute("role", "admin"))

	if !combinedFilter(ws1) {
		t.Errorf("FilterCombineAND should return true for alive admin")
	}
	if combinedFilter(ws2) {
		t.Errorf("FilterCombineAND should return false for alive user")
	}
	if combinedFilter(ws3) {
		t.Errorf("FilterCombineAND should return false for dead admin")
	}
}

// TestFilterCombineOR 测试FilterCombineOR过滤器
func TestFilterCombineOR(t *testing.T) {
	sm := NewSocketInstance()

	ws1 := newTestWS("uuid1", sm, nil)
	ws1.setAlive(true)
	ws1.SetAttribute("role", "user")

	ws2 := newTestWS("uuid2", sm, nil)
	ws2.setAlive(false)
	ws2.SetAttribute("role", "admin")

	ws3 := newTestWS("uuid3", sm, nil)
	ws3.setAlive(false)
	ws3.SetAttribute("role", "user")

	// 组合过滤器：活跃或为admin
	combinedFilter := FilterCombineOR(FilterAlive, FilterByAttribute("role", "admin"))

	if !combinedFilter(ws1) {
		t.Errorf("FilterCombineOR should return true for alive user")
	}
	if !combinedFilter(ws2) {
		t.Errorf("FilterCombineOR should return true for dead admin")
	}
	if combinedFilter(ws3) {
		t.Errorf("FilterCombineOR should return false for dead user")
	}
}

// TestBroadcastFilter 测试BroadcastFilter功能
func TestBroadcastFilter(t *testing.T) {
	sm := NewSocketInstance()

	// 创建多个测试连接
	ws1 := newTestWS("uuid1", sm, nil)
	ws1.setAlive(true)
	ws1.SetAttribute("role", "admin")

	ws2 := newTestWS("uuid2", sm, nil)
	ws2.setAlive(true)
	ws2.SetAttribute("role", "user")

	ws3 := newTestWS("uuid3", sm, nil)
	ws3.setAlive(false)
	ws3.SetAttribute("role", "admin")

	// 添加到连接池
	sm.pool.set(ws1)
	sm.pool.set(ws2)
	sm.pool.set(ws3)

	// 测试只向活跃连接广播
	sm.BroadcastFilter([]byte("test message"), FilterAlive)

	// 计算总的Emit调用次数
	totalEmits := atomic.LoadInt32(ws1.emitCount) + atomic.LoadInt32(ws2.emitCount) + atomic.LoadInt32(ws3.emitCount)
	if totalEmits != 2 {
		t.Errorf("Expected 2 connections to receive message, got %d", totalEmits)
	}

	// 重置计数器
	atomic.StoreInt32(ws1.emitCount, 0)
	atomic.StoreInt32(ws2.emitCount, 0)
	atomic.StoreInt32(ws3.emitCount, 0)

	// 测试只向admin角色广播
	sm.BroadcastFilter([]byte("admin message"), FilterByAttribute("role", "admin"))

	totalEmits = atomic.LoadInt32(ws1.emitCount) + atomic.LoadInt32(ws2.emitCount) + atomic.LoadInt32(ws3.emitCount)
	if totalEmits != 2 {
		t.Errorf("Expected 2 admin connections to receive message, got %d", totalEmits)
	}
}

// TestBroadcastExcept 测试BroadcastExcept功能
func TestBroadcastExcept(t *testing.T) {
	sm := NewSocketInstance()

	// 创建测试连接
	ws1 := newTestWS("uuid1", sm, nil)
	ws2 := newTestWS("uuid2", sm, nil)
	ws3 := newTestWS("uuid3", sm, nil)

	// 添加到连接池
	sm.pool.set(ws1)
	sm.pool.set(ws2)
	sm.pool.set(ws3)

	// 测试排除特定连接的广播
	sm.BroadcastExcept([]byte("test message"), "uuid2")

	// 计算总的Emit调用次数
	totalEmits := atomic.LoadInt32(ws1.emitCount) + atomic.LoadInt32(ws2.emitCount) + atomic.LoadInt32(ws3.emitCount)
	if totalEmits != 2 {
		t.Errorf("Expected 2 connections to receive message (excluding uuid2), got %d", totalEmits)
	}

	// 验证uuid2没有收到消息
	if atomic.LoadInt32(ws2.emitCount) != 0 {
		t.Errorf("uuid2 should not have received any messages, but got %d", atomic.LoadInt32(ws2.emitCount))
	}
}

// TestConcurrentMessageHandling 测试并发消息处理功能
func TestConcurrentMessageHandling(t *testing.T) {
	// 测试启用并发处理
	opts := &ServerOptions{
		ConcurrentMessageHandling: true,
	}
	sm := NewSocketInstanceWithServerOptions(opts)
	
	if !sm.isConcurrentHandlingEnabled() {
		t.Errorf("Expected concurrent handling to be enabled")
	}
	
	// 测试禁用并发处理
	opts2 := &ServerOptions{
		ConcurrentMessageHandling: false,
	}
	sm2 := NewSocketInstanceWithServerOptions(opts2)
	
	if sm2.isConcurrentHandlingEnabled() {
		t.Errorf("Expected concurrent handling to be disabled")
	}
}

// TestSocketError 测试SocketError结构
func TestSocketError(t *testing.T) {
	// 测试不带原因的错误
	err1 := &SocketError{
		Code:    ErrCodeConnectionClosed,
		Message: "Connection closed",
		UUID:    "test-uuid",
	}
	
	expected1 := "[1001] Connection closed"
	if err1.Error() != expected1 {
		t.Errorf("Expected error string '%s', got '%s'", expected1, err1.Error())
	}
	
	// 测试带原因的错误
	cause := errors.New("underlying error")
	err2 := &SocketError{
		Code:    ErrCodeTimeout,
		Message: "Operation timeout",
		UUID:    "test-uuid",
		Cause:   cause,
	}
	
	expected2 := "[1003] Operation timeout: underlying error"
	if err2.Error() != expected2 {
		t.Errorf("Expected error string '%s', got '%s'", expected2, err2.Error())
	}
}

// TestQuickCallbacks 测试快捷回调功能
func TestQuickCallbacks(t *testing.T) {
	sm := NewSocketInstance()
	
	var disconnectedCalled bool
	var textMessageReceived string
	var binaryMessageReceived []byte
	var pingReceived string
	var pongReceived string
	
	// 创建测试连接
	ws := newTestWS("test-uuid", sm, nil)
	
	// 设置快捷回调
	ws.OnDisconnected = func(err error) {
		disconnectedCalled = true
	}
	
	ws.OnTextMessage = func(message string) {
		textMessageReceived = message
	}
	
	ws.OnBinaryMessage = func(data []byte) {
		binaryMessageReceived = data
	}
	
	ws.OnPingReceived = func(data string) {
		pingReceived = data
	}
	
	ws.OnPongReceived = func(data string) {
		pongReceived = data
	}
	
	// 测试文本消息回调
	ws.handleMessage(TextMessage, []byte("hello"))
	if textMessageReceived != "hello" {
		t.Errorf("Expected text message 'hello', got '%s'", textMessageReceived)
	}
	
	// 测试二进制消息回调
	binaryData := []byte{1, 2, 3, 4}
	ws.handleMessage(BinaryMessage, binaryData)
	if !bytes.Equal(binaryMessageReceived, binaryData) {
		t.Errorf("Expected binary message %v, got %v", binaryData, binaryMessageReceived)
	}
	
	// 测试Ping回调
	ws.handleMessage(PingMessage, []byte("ping-data"))
	if pingReceived != "ping-data" {
		t.Errorf("Expected ping data 'ping-data', got '%s'", pingReceived)
	}
	
	// 测试Pong回调
	ws.handleMessage(PongMessage, []byte("pong-data"))
	if pongReceived != "pong-data" {
		t.Errorf("Expected pong data 'pong-data', got '%s'", pongReceived)
	}
	
	// 测试断开连接回调
	ws.disconnected(nil)
	if !disconnectedCalled {
		t.Errorf("Expected OnDisconnected callback to be called")
	}
}

// TestBuildProxy 测试代理构造函数
func TestBuildProxy(t *testing.T) {
	// 测试空URL
	proxy, err := BuildProxy("")
	if err != nil {
		t.Errorf("Expected no error for empty URL, got %v", err)
	}
	if proxy != nil {
		t.Errorf("Expected nil proxy for empty URL")
	}
	
	// 测试有效URL
	proxyURL := "http://proxy.example.com:8080"
	proxy, err = BuildProxy(proxyURL)
	if err != nil {
		t.Errorf("Expected no error for valid URL, got %v", err)
	}
	if proxy == nil {
		t.Errorf("Expected non-nil proxy for valid URL")
	}
	
	// 测试代理函数
	if proxy != nil {
		req := &http.Request{}
		url, err := proxy(req)
		if err != nil {
			t.Errorf("Expected no error from proxy function, got %v", err)
		}
		if url.String() != proxyURL {
			t.Errorf("Expected proxy URL '%s', got '%s'", proxyURL, url.String())
		}
	}
	
	// 测试无效URL（使用真正无效的URL格式）
	_, err = BuildProxy("://invalid-url")
	if err == nil {
		t.Errorf("Expected error for invalid URL")
	}
}

// TestApplyDialerOptions 测试Dialer配置应用
func TestApplyDialerOptions(t *testing.T) {
	dialer := &websocket.Dialer{}
	
	opts := ConnectionOptions{
		UseCompression: true,
		UseSSL:         true,
		Subprotocols:   []string{"protocol1", "protocol2"},
	}
	
	applyDialerOptions(dialer, opts)
	
	if !dialer.EnableCompression {
		t.Errorf("Expected compression to be enabled")
	}
	
	if dialer.TLSClientConfig == nil {
		t.Errorf("Expected TLS config to be set")
	}
	
	if !dialer.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("Expected InsecureSkipVerify to be true")
	}
	
	if len(dialer.Subprotocols) != 2 {
		t.Errorf("Expected 2 subprotocols, got %d", len(dialer.Subprotocols))
	}
	
	if dialer.Subprotocols[0] != "protocol1" || dialer.Subprotocols[1] != "protocol2" {
		t.Errorf("Expected subprotocols [protocol1, protocol2], got %v", dialer.Subprotocols)
	}
}

// TestErrorConstants 测试错误常量
func TestErrorConstants(t *testing.T) {
	// 测试错误代码常量
	if ErrCodeConnectionClosed != 1001 {
		t.Errorf("Expected ErrCodeConnectionClosed to be 1001, got %d", ErrCodeConnectionClosed)
	}
	
	if ErrCodeBufferFull != 1002 {
		t.Errorf("Expected ErrCodeBufferFull to be 1002, got %d", ErrCodeBufferFull)
	}
	
	// 测试新的错误类型
	if ErrSessionClosed == nil {
		t.Errorf("Expected ErrSessionClosed to be defined")
	}
	
	if ErrMessageBufferFull == nil {
		t.Errorf("Expected ErrMessageBufferFull to be defined")
	}
	
	if ErrWriteTimeout == nil {
		t.Errorf("Expected ErrWriteTimeout to be defined")
	}
}

// TestServerOptionsGetters 测试ServerOptions的getter方法
func TestServerOptionsGetters(t *testing.T) {
	opts := &ServerOptions{
		WriteWait:                 5 * time.Second,
		PongWait:                  30 * time.Second,
		MaxSendRetry:              10,
		MessageBufferSize:         512,
		ConcurrentMessageHandling: true,
		EnableDetailedErrors:      true,
	}
	
	sm := NewSocketInstanceWithServerOptions(opts)
	
	if sm.getWriteWait() != 5*time.Second {
		t.Errorf("Expected WriteWait 5s, got %v", sm.getWriteWait())
	}
	
	if sm.getPongWait() != 30*time.Second {
		t.Errorf("Expected PongWait 30s, got %v", sm.getPongWait())
	}
	
	if sm.getMaxSendRetry() != 10 {
		t.Errorf("Expected MaxSendRetry 10, got %d", sm.getMaxSendRetry())
	}
	
	if sm.getMessageBufferSize() != 512 {
		t.Errorf("Expected MessageBufferSize 512, got %d", sm.getMessageBufferSize())
	}
	
	if !sm.isConcurrentHandlingEnabled() {
		t.Errorf("Expected ConcurrentMessageHandling to be true")
	}
	
	if !sm.isDetailedErrorsEnabled() {
		t.Errorf("Expected EnableDetailedErrors to be true")
	}
}
