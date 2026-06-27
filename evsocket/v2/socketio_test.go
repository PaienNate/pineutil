package socketio

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lxzan/gws"
)

const testTimeout = 2 * time.Second

func wsTestURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func waitForValue[T any](t *testing.T, description string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
	var zero T
	return zero
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type blockingDialer struct {
	started chan struct{}
	release <-chan struct{}
	err     error
}

func (d *blockingDialer) Dial(network, addr string) (net.Conn, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-d.release
	return nil, d.err
}

func TestSocketInstanceOnConcurrentRegistrationRetainsAllCallbacks(t *testing.T) {
	const listeners = 64
	sm := NewSocketInstance()
	start := make(chan struct{})
	var ready sync.WaitGroup
	var registered sync.WaitGroup
	var callbacks atomic.Int64
	ready.Add(listeners)
	registered.Add(listeners)
	for i := 0; i < listeners; i++ {
		go func() {
			ready.Done()
			<-start
			sm.On(EventMessage, func(payload *EventPayload) { callbacks.Add(1) })
			registered.Done()
		}()
	}
	ready.Wait()
	close(start)
	registered.Wait()
	kws := newServerWrapper(sm)
	kws.fireEvent(EventMessage, []byte("payload"), nil)
	if got := callbacks.Load(); got != listeners {
		t.Fatalf("fired %d callbacks, want %d", got, listeners)
	}
}

func TestSocketInstanceMessageEventUsesStableAttributeSnapshot(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 1)
	snapshotCaptured := make(chan struct{}, 1)
	observed := make(chan string, 1)
	allowAssertion := make(chan struct{})
	var releaseSnapshot sync.Once
	release := func() { releaseSnapshot.Do(func() { close(allowAssertion) }) }
	t.Cleanup(release)
	sm.On(EventMessage, func(payload *EventPayload) {
		attrs := payload.SocketAttributes
		snapshotCaptured <- struct{}{}
		<-allowAssertion
		value, _ := attrs["phase"].(string)
		observed <- value
	})
	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		kws.SetAttribute("phase", "before")
		connected <- kws
	}))
	t.Cleanup(server.Close)
	clientConn, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	serverSocket := waitForValue(t, "server connection", connected)
	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write test message: %v", err)
	}
	waitForValue(t, "message callback snapshot", snapshotCaptured)
	serverSocket.SetAttribute("phase", "after")
	release()
	if got := waitForValue(t, "snapshot assertion", observed); got != "before" {
		t.Fatalf("SocketAttributes changed after dispatch: got %q, want %q", got, "before")
	}
}

func TestWebsocketWrapperDisconnectedFiresDisconnectEventOnce(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 1)
	var disconnects atomic.Int64
	sm.On(EventDisconnect, func(payload *EventPayload) { disconnects.Add(1) })
	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) { connected <- kws }))
	t.Cleanup(server.Close)
	clientConn, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	_ = waitForValue(t, "server connection", connected)
	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client connection: %v", err)
	}
	waitForCondition(t, "first disconnect event", func() bool { return disconnects.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := disconnects.Load(); got != 1 {
		t.Fatalf("disconnect event fired %d times, want 1", got)
	}
}

func TestGetRequestHeader(t *testing.T) {
	t.Run("server reverse mode reads handshake authorization", func(t *testing.T) {
		sm := NewSocketInstance()
		authorization := make(chan string, 1)
		server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
			authorization <- kws.GetRequestHeader("authorization")
		}))
		t.Cleanup(server.Close)

		requestHeader := http.Header{}
		requestHeader.Set("Authorization", "Bearer reverse-mode-token")
		clientConn, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), requestHeader)
		if err != nil {
			t.Fatalf("dial test server: %v", err)
		}
		t.Cleanup(func() { _ = clientConn.Close() })

		if got := waitForValue(t, "server authorization header", authorization); got != "Bearer reverse-mode-token" {
			t.Fatalf("GetRequestHeader(Authorization) = %q, want %q", got, "Bearer reverse-mode-token")
		}
	})

	t.Run("client facade reads configured request header", func(t *testing.T) {
		sm := NewSocketInstance()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}))
		t.Cleanup(server.Close)

		requestHeader := http.Header{}
		requestHeader.Set("X-Client-Token", "forward-mode-token")
		client, err := sm.Dial(wsTestURL(server.URL), ClientOptions{RequestHeader: requestHeader})
		if err != nil {
			t.Fatalf("Dial returned error: %v", err)
		}
		t.Cleanup(client.Close)

		requestHeader.Set("X-Client-Token", "mutated-after-dial")
		if got := client.GetRequestHeader("x-client-token"); got != "forward-mode-token" {
			t.Fatalf("GetRequestHeader(X-Client-Token) = %q, want %q", got, "forward-mode-token")
		}
	})
}

func TestSocketInstanceDialReturnsFacadeSessionAndReceivesEvents(t *testing.T) {
	sm := NewSocketInstance()
	received := make(chan string, 1)
	disconnects := make(chan error, 1)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, payload); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	client, err := sm.Dial(wsTestURL(server.URL), ClientOptions{})
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	if !client.IsAlive() {
		t.Fatal("expected dialed client session to be alive")
	}
	client.OnTextMessage = func(message string) { received <- message }
	client.OnDisconnected = func(err error) { disconnects <- err }

	payload := []byte("hello-client")
	client.Emit(payload)
	if got := waitForValue(t, "client text callback", received); got != string(payload) {
		t.Fatalf("client callback got %q, want %q", got, payload)
	}

	client.Close()
	_ = waitForValue(t, "client disconnect callback", disconnects)
}

func TestSocketInstanceEmitToAndBroadcastRouteAcrossServerSessions(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 2)

	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		connected <- kws
	}))
	t.Cleanup(server.Close)

	clientA, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial client A: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial client B: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })

	sessionA := waitForValue(t, "session A", connected)
	sessionB := waitForValue(t, "session B", connected)

	directedPayload := []byte("only-b")
	if err := sm.EmitTo(sessionB.GetUUID(), directedPayload); err != nil {
		t.Fatalf("EmitTo returned error: %v", err)
	}
	messageType, payload, err := clientB.ReadMessage()
	if err != nil {
		t.Fatalf("read directed payload from client B: %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != string(directedPayload) {
		t.Fatalf("client B got (%d,%q), want (%d,%q)", messageType, payload, websocket.TextMessage, directedPayload)
	}

	broadcastPayload := []byte("to-all")
	sessionA.Broadcast(broadcastPayload, false)
	for idx, client := range []*websocket.Conn{clientA, clientB} {
		messageType, payload, err = client.ReadMessage()
		if err != nil {
			t.Fatalf("read broadcast payload from client %d: %v", idx, err)
		}
		if messageType != websocket.TextMessage || string(payload) != string(broadcastPayload) {
			t.Fatalf("client %d got (%d,%q), want (%d,%q)", idx, messageType, payload, websocket.TextMessage, broadcastPayload)
		}
	}
}

func TestSocketInstanceDialReconnectKeepsFacadeIdentity(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		go func() {
			defer conn.Close()
			for {
				messageType, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(messageType, payload); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)

	client, err := sm.Dial(wsTestURL(server.URL), ClientOptions{})
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	firstUUID := client.GetUUID()
	firstConn := waitForValue(t, "first server-side conn", serverConns)
	if err := firstConn.Close(); err != nil {
		t.Fatalf("close first server-side conn: %v", err)
	}
	waitForCondition(t, "client disconnect", func() bool { return !client.IsAlive() })

	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect returned error: %v", err)
	}
	_ = waitForValue(t, "second server-side conn", serverConns)
	if client.GetUUID() != firstUUID {
		t.Fatalf("Reconnect changed UUID: got %q, want %q", client.GetUUID(), firstUUID)
	}
	if !sm.pool.contains(firstUUID) {
		t.Fatalf("reconnected client missing from pool for UUID %q", firstUUID)
	}
	pooled, err := sm.pool.get(firstUUID)
	if err != nil {
		t.Fatalf("pool lookup after reconnect returned error: %v", err)
	}
	if pooled != client {
		t.Fatal("Reconnect replaced facade object instead of reusing the original client")
	}
	received := make(chan string, 1)
	client.OnTextMessage = func(message string) { received <- message }
	payload := []byte("after-reconnect")
	if err := sm.EmitTo(firstUUID, payload); err != nil {
		t.Fatalf("EmitTo after reconnect returned error: %v", err)
	}
	if got := waitForValue(t, "reconnected payload", received); got != string(payload) {
		t.Fatalf("reconnected payload got %q, want %q", got, payload)
	}
}

func TestSocketInstanceNewClientConnectAndReconnect(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		go func() {
			defer conn.Close()
			for {
				messageType, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(messageType, payload); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{})
	textReceived := make(chan string, 2)
	var disconnects atomic.Int64
	client.OnTextMessage = func(message string) { textReceived <- message }
	client.OnDisconnected = func(err error) { disconnects.Add(1) }

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if !client.IsAlive() {
		t.Fatal("expected client facade to be alive after Connect")
	}
	firstUUID := client.GetUUID()
	firstConn := waitForValue(t, "first server conn", serverConns)
	payload := []byte("first-connect")
	client.Emit(payload)
	if got := waitForValue(t, "first connect echo", textReceived); got != string(payload) {
		t.Fatalf("first connect got %q, want %q", got, payload)
	}

	if err := firstConn.Close(); err != nil {
		t.Fatalf("close first server conn: %v", err)
	}
	waitForCondition(t, "client disconnect after first close", func() bool { return !client.IsAlive() })
	waitForCondition(t, "client disconnect callback", func() bool { return disconnects.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := disconnects.Load(); got != 1 {
		t.Fatalf("OnDisconnected fired %d times after a single disconnect, want 1", got)
	}

	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect returned error: %v", err)
	}
	_ = waitForValue(t, "second server conn", serverConns)
	if client.GetUUID() != firstUUID {
		t.Fatalf("Reconnect changed UUID: got %q, want %q", client.GetUUID(), firstUUID)
	}
	pooled, err := sm.pool.get(firstUUID)
	if err != nil {
		t.Fatalf("pool lookup after reconnect returned error: %v", err)
	}
	if pooled != client {
		t.Fatal("Reconnect replaced facade object instead of reusing the original client")
	}
	payload = []byte("second-connect")
	client.Emit(payload)
	if got := waitForValue(t, "second connect echo", textReceived); got != string(payload) {
		t.Fatalf("second connect got %q, want %q", got, payload)
	}
}

func TestClientLifecycleForExternalFSM(t *testing.T) {
	stype := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	sm := NewSocketInstance()
	serverConns := make(chan *websocket.Conn, 2)
	handshakeHeaders := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := stype.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		select {
		case handshakeHeaders <- r.Header.Get("X-Client-Token"):
		default:
		}
		serverConns <- conn
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)

	type callbackSnapshot struct {
		name   string
		alive  bool
		pooled bool
		uuid   string
		token  string
		phase  string
		err    error
	}

	callbackEvents := make(chan callbackSnapshot, 8)
	var client *WebsocketWrapper
	pushSnapshot := func(name string, err error) {
		callbackEvents <- callbackSnapshot{
			name:   name,
			alive:  client.IsAlive(),
			pooled: sm.pool.contains(client.GetUUID()),
			uuid:   client.GetUUID(),
			token:  client.GetRequestHeader("x-client-token"),
			phase:  client.GetStringAttribute("phase"),
			err:    err,
		}
	}

	requestHeader := http.Header{}
	requestHeader.Set("X-Client-Token", "fsm-token")
	client = sm.NewClient(wsTestURL(server.URL), ClientOptions{
		RequestHeader: requestHeader,
		PingInterval:  5 * time.Second,
		PingWait:      5 * time.Second,
	})
	requestHeader.Set("X-Client-Token", "mutated-after-newclient")
	client.SetAttribute("phase", "stable")
	if err := client.SetUUID("fsm-client"); err != nil {
		t.Fatalf("SetUUID returned error: %v", err)
	}
	var connectFailedCount atomic.Int64
	client.OnConnected = func() { pushSnapshot("connected", nil) }
	client.OnDisconnected = func(err error) { pushSnapshot("disconnected", err) }
	client.OnConnectError = func(err error) { pushSnapshot("connect_error", err) }
	client.OnConnectFailed = func(err error) { connectFailedCount.Add(1) }

	assertSnapshot := func(got callbackSnapshot, wantName string, wantAlive, wantPooled bool) {
		t.Helper()
		if got.name != wantName {
			t.Fatalf("callback name = %q, want %q", got.name, wantName)
		}
		if got.alive != wantAlive {
			t.Fatalf("callback %q alive = %v, want %v", got.name, got.alive, wantAlive)
		}
		if got.pooled != wantPooled {
			t.Fatalf("callback %q pooled = %v, want %v", got.name, got.pooled, wantPooled)
		}
		if got.uuid != "fsm-client" {
			t.Fatalf("callback %q UUID = %q, want %q", got.name, got.uuid, "fsm-client")
		}
		if got.token != "fsm-token" {
			t.Fatalf("callback %q token = %q, want %q", got.name, got.token, "fsm-token")
		}
		if got.phase != "stable" {
			t.Fatalf("callback %q phase = %q, want %q", got.name, got.phase, "stable")
		}
	}

	assertNoCallback := func(description string) {
		t.Helper()
		select {
		case event := <-callbackEvents:
			t.Fatalf("unexpected callback while waiting for %s: %+v", description, event)
		case <-time.After(150 * time.Millisecond):
		}
	}

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	assertSnapshot(waitForValue(t, "first OnConnected callback", callbackEvents), "connected", true, true)
	if got := waitForValue(t, "first client handshake header", handshakeHeaders); got != "fsm-token" {
		t.Fatalf("first handshake header = %q, want %q", got, "fsm-token")
	}
	if got := client.GetUUID(); got != "fsm-client" {
		t.Fatalf("client UUID after Connect = %q, want %q", got, "fsm-client")
	}
	if got := client.GetStringAttribute("phase"); got != "stable" {
		t.Fatalf("client phase after Connect = %q, want %q", got, "stable")
	}
	assertNoCallback("manual reconnect trigger after first connect")

	firstConn := waitForValue(t, "first server connection", serverConns)
	if err := firstConn.Close(); err != nil {
		t.Fatalf("close first server connection: %v", err)
	}
	assertSnapshot(waitForValue(t, "first OnDisconnected callback", callbackEvents), "disconnected", false, false)
	if client.IsAlive() {
		t.Fatal("expected client to become inactive before external FSM calls Reconnect")
	}
	assertNoCallback("external FSM deciding whether to reconnect")

	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect returned error: %v", err)
	}
	assertSnapshot(waitForValue(t, "second OnConnected callback", callbackEvents), "connected", true, true)
	if got := waitForValue(t, "second client handshake header", handshakeHeaders); got != "fsm-token" {
		t.Fatalf("second handshake header = %q, want %q", got, "fsm-token")
	}
	pooled, err := sm.pool.get("fsm-client")
	if err != nil {
		t.Fatalf("pool lookup after successful Reconnect returned error: %v", err)
	}
	if pooled != client {
		t.Fatal("Reconnect replaced the client facade instead of reusing it")
	}

	secondConn := waitForValue(t, "second server connection", serverConns)
	if err := secondConn.Close(); err != nil {
		t.Fatalf("close second server connection: %v", err)
	}
	assertSnapshot(waitForValue(t, "second OnDisconnected callback", callbackEvents), "disconnected", false, false)
	server.Close()
	assertNoCallback("manual reconnect after second disconnect")

	if err := client.Reconnect(); err == nil {
		t.Fatal("expected Reconnect to fail after server shutdown")
	}
	assertSnapshot(waitForValue(t, "OnConnectError after failed reconnect", callbackEvents), "connect_error", false, false)
	if connectFailedCount.Load() != 0 {
		t.Fatalf("OnConnectFailed fired %d times during external FSM lifecycle, want 0", connectFailedCount.Load())
	}
	assertNoCallback("failed reconnect without built-in retry")
}

func TestClientConnectAndReconnectReturnShutdownAfterSocketInstanceShutdown(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{})
	disconnects := make(chan error, 1)
	connectErrors := make(chan error, 1)
	client.OnDisconnected = func(err error) { disconnects <- err }
	client.OnConnectError = func(err error) { connectErrors <- err }

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if err := sm.Shutdown(); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	_ = waitForValue(t, "disconnect callback on shutdown", disconnects)
	if client.IsAlive() {
		t.Fatal("expected client to become inactive after shutdown")
	}
	if err := client.Connect(); !errors.Is(err, ErrorShutdown) {
		t.Fatalf("Connect after Shutdown got %v, want %v", err, ErrorShutdown)
	}
	if err := client.Reconnect(); !errors.Is(err, ErrorShutdown) {
		t.Fatalf("Reconnect after Shutdown got %v, want %v", err, ErrorShutdown)
	}
	select {
	case err := <-connectErrors:
		t.Fatalf("unexpected OnConnectError after shutdown sentinel: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestConnectDoesNotCreateSecondTransportWhenAlive(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{})
	if err := client.Connect(); err != nil {
		t.Fatalf("first Connect returned error: %v", err)
	}
	_ = waitForValue(t, "first server conn", serverConns)

	if err := client.Connect(); err != nil {
		t.Fatalf("second Connect returned error: %v", err)
	}
	if !client.IsAlive() {
		t.Fatal("expected client to remain alive after repeated Connect")
	}
	select {
	case extraConn := <-serverConns:
		_ = extraConn.Close()
		t.Fatal("expected Connect on an alive client not to create a second transport")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestConnectRejectsWhileConnectInProgress(t *testing.T) {
	sm := NewSocketInstance()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	client := sm.NewClient("ws://example.invalid", ClientOptions{
		NewDialer: func() (gws.Dialer, error) {
			return &blockingDialer{started: started, release: release, err: errors.New("dial aborted")}, nil
		},
	})

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- client.Connect()
	}()
	waitForValue(t, "first connect dial start", started)

	secondErr := make(chan error, 1)
	go func() {
		secondErr <- client.Connect()
	}()
	select {
	case err := <-secondErr:
		if err == nil {
			t.Fatal("expected concurrent Connect to fail while the first Connect is still in progress")
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("concurrent Connect blocked instead of failing while the first Connect was in progress")
	}
	close(release)
	if err := waitForValue(t, "first connect result", firstErr); err == nil {
		t.Fatal("expected first Connect to fail after releasing the test dialer")
	}
}

func TestReconnectRejectsWhileConnectInProgress(t *testing.T) {
	sm := NewSocketInstance()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	client := sm.NewClient("ws://example.invalid", ClientOptions{
		NewDialer: func() (gws.Dialer, error) {
			return &blockingDialer{started: started, release: release, err: errors.New("dial aborted")}, nil
		},
	})

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- client.Connect()
	}()
	waitForValue(t, "first connect dial start", started)

	reconnectErr := make(chan error, 1)
	go func() {
		reconnectErr <- client.Reconnect()
	}()
	select {
	case err := <-reconnectErr:
		if err == nil {
			t.Fatal("expected Reconnect to fail while Connect is still in progress")
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("Reconnect blocked instead of failing while Connect was in progress")
	}
	close(release)
	if err := waitForValue(t, "first connect result", firstErr); err == nil {
		t.Fatal("expected first Connect to fail after releasing the test dialer")
	}
}

func TestConnectFailureTriggersConnectCallbacks(t *testing.T) {
	sm := NewSocketInstance()
	client := sm.NewClient("ws://127.0.0.1:1", ClientOptions{HandshakeTimeout: 200 * time.Millisecond})
	connectError := make(chan error, 1)
	connectFailed := make(chan error, 1)
	client.OnConnectError = func(err error) { connectError <- err }
	client.OnConnectFailed = func(err error) { connectFailed <- err }

	err := client.Connect()
	if err == nil {
		t.Fatal("expected Connect to fail for invalid target")
	}
	if got := waitForValue(t, "OnConnectError callback", connectError); got == nil {
		t.Fatal("expected OnConnectError to receive an error")
	}
	if got := waitForValue(t, "OnConnectFailed callback", connectFailed); got == nil {
		t.Fatal("expected OnConnectFailed to receive an error")
	}
}

func TestReconnectFailureDoesNotTriggerConnectFailed(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{})
	connectFailedCount := atomic.Int64{}
	connectErrors := make(chan error, 1)
	client.OnConnectFailed = func(err error) { connectFailedCount.Add(1) }
	client.OnConnectError = func(err error) { connectErrors <- err }

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	serverConn := waitForValue(t, "server websocket conn", serverConns)
	if err := serverConn.Close(); err != nil {
		t.Fatalf("close server websocket conn: %v", err)
	}
	waitForCondition(t, "client disconnect after server close", func() bool { return !client.IsAlive() })
	server.Close()
	if err := client.Reconnect(); err == nil {
		t.Fatal("expected Reconnect to fail after server shutdown")
	}
	if got := waitForValue(t, "OnConnectError on reconnect failure", connectErrors); got == nil {
		t.Fatal("expected OnConnectError during reconnect failure")
	}
	if got := connectFailedCount.Load(); got != 0 {
		t.Fatalf("OnConnectFailed fired %d times on reconnect failure, want 0", got)
	}
}

func TestClientHeartbeatAppliesDefaultOptions(t *testing.T) {
	sm := NewSocketInstance()
	client := sm.NewClient("ws://example.invalid", ClientOptions{})

	if got := client.clientOptions.HandshakeTimeout; got != 10*time.Second {
		t.Fatalf("default HandshakeTimeout = %v, want %v", got, 10*time.Second)
	}
	if got := client.clientOptions.PingInterval; got != 15*time.Second {
		t.Fatalf("default PingInterval = %v, want %v", got, 15*time.Second)
	}
	if got := client.clientOptions.PingWait; got != 45*time.Second {
		t.Fatalf("default PingWait = %v, want %v", got, 45*time.Second)
	}
}

func TestClientHeartbeatSendsPingAndStaysAliveAfterPong(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	pingReceived := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPingHandler(func(appData string) error {
			select {
			case pingReceived <- appData:
			default:
			}
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	disconnects := make(chan error, 1)
	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{
		PingInterval: 40 * time.Millisecond,
		PingWait:     130 * time.Millisecond,
	})
	client.OnDisconnected = func(err error) { disconnects <- err }
	t.Cleanup(client.Close)

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	_ = waitForValue(t, "client heartbeat ping", pingReceived)
	time.Sleep(150 * time.Millisecond)
	if !client.IsAlive() {
		t.Fatal("expected client to stay alive after receiving Pong frames")
	}
	select {
	case err := <-disconnects:
		t.Fatalf("unexpected disconnect after Pong heartbeat: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
}

func TestClientDeadlineRefreshesOnMessage(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	messageReceived := make(chan string, 1)
	disconnects := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = conn.WriteMessage(websocket.TextMessage, []byte("refresh-deadline"))
		}()
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{
		PingInterval: 5 * time.Second,
		PingWait:     200 * time.Millisecond,
	})
	client.OnTextMessage = func(message string) { messageReceived <- message }
	client.OnDisconnected = func(err error) { disconnects <- err }
	t.Cleanup(client.Close)

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if got := waitForValue(t, "deadline refresh message", messageReceived); got != "refresh-deadline" {
		t.Fatalf("client received %q, want %q", got, "refresh-deadline")
	}
	time.Sleep(150 * time.Millisecond)
	if !client.IsAlive() {
		t.Fatal("expected ordinary message to refresh the client deadline")
	}
	if err := waitForValue(t, "disconnect after refreshed deadline", disconnects); err == nil {
		t.Fatal("expected timeout disconnect after the refreshed deadline elapsed")
	}
	waitForCondition(t, "client to become inactive after refreshed deadline", func() bool { return !client.IsAlive() })
}

func TestClientDeadlineRefreshesOnPingAndRepliesWithMatchingPong(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	pongReceived := make(chan string, 1)
	disconnects := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.SetPongHandler(func(appData string) error {
			select {
			case pongReceived <- appData:
			default:
			}
			return nil
		})
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = conn.WriteControl(websocket.PingMessage, []byte("refresh-deadline"), time.Now().Add(time.Second))
		}()
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{
		PingInterval: 5 * time.Second,
		PingWait:     200 * time.Millisecond,
	})
	client.OnDisconnected = func(err error) { disconnects <- err }
	t.Cleanup(client.Close)

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if got := waitForValue(t, "pong reply after inbound ping", pongReceived); got != "refresh-deadline" {
		t.Fatalf("client pong payload = %q, want %q", got, "refresh-deadline")
	}
	time.Sleep(150 * time.Millisecond)
	if !client.IsAlive() {
		t.Fatal("expected inbound Ping to refresh the client deadline")
	}
	if err := waitForValue(t, "disconnect after ping refreshed deadline", disconnects); err == nil {
		t.Fatal("expected timeout disconnect after the ping-refreshed deadline elapsed")
	}
	waitForCondition(t, "client to become inactive after ping-refreshed deadline", func() bool { return !client.IsAlive() })
}

func TestSocketInstanceServerOptionsAreAppliedToUpgrade(t *testing.T) {
	sm := NewSocketInstance()
	sm.ServerOptions = &gws.ServerOption{
		SubProtocols: []string{"demo-subprotocol"},
		ResponseHeader: http.Header{
			"X-Server-Mode": []string{"configured"},
		},
	}
	connected := make(chan *WebsocketWrapper, 1)
	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		connected <- kws
	}))
	t.Cleanup(server.Close)

	clientConn, resp, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), http.Header{
		"Sec-WebSocket-Protocol": []string{"demo-subprotocol"},
	})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	_ = waitForValue(t, "server connection", connected)
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "demo-subprotocol" {
		t.Fatalf("subprotocol = %q, want %q", got, "demo-subprotocol")
	}
	if got := resp.Header.Get("X-Server-Mode"); got != "configured" {
		t.Fatalf("response header = %q, want %q", got, "configured")
	}
}

func TestWebsocketWrapperSetUUIDRejectsConcurrentDuplicates(t *testing.T) {
	sm := NewSocketInstance()
	first := sm.NewClient("ws://127.0.0.1:65535/ws", ClientOptions{})
	second := sm.NewClient("ws://127.0.0.1:65535/ws", ClientOptions{})
	targetUUID := "duplicate-uuid"

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- first.SetUUID(targetUUID)
	}()
	go func() {
		<-start
		results <- second.SetUUID(targetUUID)
	}()
	close(start)

	firstErr := <-results
	secondErr := <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("expected exactly one SetUUID success, got first=%v second=%v", firstErr, secondErr)
	}
	if firstErr != nil && !errors.Is(firstErr, ErrorUUIDDuplication) {
		t.Fatalf("unexpected first error: %v", firstErr)
	}
	if secondErr != nil && !errors.Is(secondErr, ErrorUUIDDuplication) {
		t.Fatalf("unexpected second error: %v", secondErr)
	}
}

func TestClientDisconnectOnTimeout(t *testing.T) {
	sm := NewSocketInstance()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		<-release
		_ = conn.Close()
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	disconnects := make(chan error, 1)
	client := sm.NewClient(wsTestURL(server.URL), ClientOptions{
		PingInterval: 40 * time.Millisecond,
		PingWait:     130 * time.Millisecond,
	})
	client.OnDisconnected = func(err error) { disconnects <- err }
	t.Cleanup(client.Close)

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	serverConn := waitForValue(t, "server connection", serverConns)
	t.Cleanup(func() { _ = serverConn.Close() })
	if err := waitForValue(t, "timeout disconnect", disconnects); err == nil {
		t.Fatal("expected timeout to trigger OnDisconnected")
	}
	waitForCondition(t, "client timeout inactive state", func() bool { return !client.IsAlive() })
}

func TestSessionQuickCallbacksReceivePingPongAndBinary(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 1)
	pingReceived := make(chan string, 1)
	pongReceived := make(chan string, 1)
	binaryReceived := make(chan []byte, 1)

	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		kws.OnPingReceived = func(data string) { pingReceived <- data }
		kws.OnPongReceived = func(data string) { pongReceived <- data }
		kws.OnBinaryMessage = func(data []byte) { binaryReceived <- append([]byte(nil), data...) }
		connected <- kws
	}))
	t.Cleanup(server.Close)

	clientConn, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	_ = waitForValue(t, "server connection", connected)

	if err := clientConn.WriteControl(websocket.PingMessage, []byte("ping-data"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write ping frame: %v", err)
	}
	if got := waitForValue(t, "ping callback", pingReceived); got != "ping-data" {
		t.Fatalf("ping callback got %q, want %q", got, "ping-data")
	}

	if err := clientConn.WriteControl(websocket.PongMessage, []byte("pong-data"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write pong frame: %v", err)
	}
	if got := waitForValue(t, "pong callback", pongReceived); got != "pong-data" {
		t.Fatalf("pong callback got %q, want %q", got, "pong-data")
	}

	binaryPayload := []byte{1, 2, 3, 4}
	if err := clientConn.WriteMessage(websocket.BinaryMessage, binaryPayload); err != nil {
		t.Fatalf("write binary message: %v", err)
	}
	if got := waitForValue(t, "binary callback", binaryReceived); string(got) != string(binaryPayload) {
		t.Fatalf("binary callback got %v, want %v", got, binaryPayload)
	}
}

func TestSocketInstanceBroadcastExceptSkipsSender(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 2)
	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		connected <- kws
	}))
	t.Cleanup(server.Close)

	clientA, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial client A: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial client B: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })

	sessionA := waitForValue(t, "session A", connected)
	_ = waitForValue(t, "session B", connected)

	payload := []byte("broadcast-with-except")
	sessionA.Broadcast(payload, true)

	if err := clientA.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("set client A read deadline: %v", err)
	}
	if _, _, err := clientA.ReadMessage(); err == nil {
		t.Fatal("client A received a message despite except=true")
	}
	if err := clientB.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set client B read deadline: %v", err)
	}
	messageType, got, err := clientB.ReadMessage()
	if err != nil {
		t.Fatalf("read broadcast payload from client B: %v", err)
	}
	if messageType != websocket.TextMessage || string(got) != string(payload) {
		t.Fatalf("client B got (%d,%q), want (%d,%q)", messageType, got, websocket.TextMessage, payload)
	}
}

func TestSocketInstanceShutdownClosesSessionsAndPreventsFutureUse(t *testing.T) {
	sm := NewSocketInstance()
	connected := make(chan *WebsocketWrapper, 1)
	var disconnects atomic.Int64
	sm.On(EventDisconnect, func(payload *EventPayload) {
		disconnects.Add(1)
	})
	server := httptest.NewServer(sm.New(func(kws *WebsocketWrapper) {
		connected <- kws
	}))
	t.Cleanup(server.Close)

	clientConn, _, err := websocket.DefaultDialer.Dial(wsTestURL(server.URL), nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	session := waitForValue(t, "session", connected)

	if err := sm.Shutdown(); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	waitForCondition(t, "session to become inactive", func() bool { return !session.IsAlive() })
	if disconnects.Load() == 0 {
		t.Fatal("expected disconnect event during shutdown")
	}
	if err := sm.EmitTo(session.GetUUID(), []byte("nope")); !errors.Is(err, ErrorShutdown) {
		t.Fatalf("EmitTo after shutdown got %v, want %v", err, ErrorShutdown)
	}
	if _, err := sm.Dial(wsTestURL(server.URL), ClientOptions{}); !errors.Is(err, ErrorShutdown) {
		t.Fatalf("Dial after shutdown got %v, want %v", err, ErrorShutdown)
	}
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("plain HTTP request after shutdown: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("HTTP handler after shutdown returned %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
}
