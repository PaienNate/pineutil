package socketio_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	socketio "github.com/PaienNate/pineutil/evsocket/v2"
	"github.com/gorilla/websocket"
)

const exampleTimeout = 2 * time.Second

func exampleWSURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func exampleReceive[T any](description string, ch <-chan T) T {
	select {
	case value := <-ch:
		return value
	case <-time.After(exampleTimeout):
		panic("timed out waiting for " + description)
	}
}

func newExampleEchoServer() (*httptest.Server, chan *websocket.Conn) {
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
	return server, serverConns
}

func ExampleSocketInstance_New() {
	sm := socketio.NewSocketInstance()
	connected := make(chan string, 1)
	messages := make(chan string, 1)

	sm.On(socketio.EventConnect, func(payload *socketio.EventPayload) {
		if err := payload.Kws.SetUUID("demo-server-session"); err != nil {
			panic(err)
		}
		payload.Kws.SetAttribute("role", "server-side")
		connected <- payload.Kws.GetUUID()
	})
	sm.On(socketio.EventMessage, func(payload *socketio.EventPayload) {
		messages <- string(payload.Data)
	})

	server := httptest.NewServer(sm.New())
	defer server.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial(exampleWSURL(server.URL), nil)
	if err != nil {
		panic(err)
	}
	defer clientConn.Close()

	fmt.Println("会话建立:", exampleReceive("连接事件", connected))
	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("你好，服务端")); err != nil {
		panic(err)
	}
	fmt.Println("收到消息:", exampleReceive("消息事件", messages))
	if err := sm.Shutdown(); err != nil {
		panic(err)
	}

	// Output:
	// 会话建立: demo-server-session
	// 收到消息: 你好，服务端
}

func ExampleSocketInstance_Dial() {
	sm := socketio.NewSocketInstance()
	server, _ := newExampleEchoServer()
	defer server.Close()

	client, err := sm.Dial(exampleWSURL(server.URL), socketio.ClientOptions{
		PingInterval: time.Second,
		PingWait:     3 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	received := make(chan string, 1)
	client.OnTextMessage = func(message string) {
		received <- message
	}
	client.Emit([]byte("你好，Dial"))

	fmt.Println("客户端已连接:", client.IsAlive())
	fmt.Println("收到回声:", exampleReceive("Dial 回声", received))

	// Output:
	// 客户端已连接: true
	// 收到回声: 你好，Dial
}

func ExampleWebsocketWrapper_Connect() {
	sm := socketio.NewSocketInstance()
	server, serverConns := newExampleEchoServer()
	defer server.Close()

	client := sm.NewClient(exampleWSURL(server.URL), socketio.ClientOptions{
		PingInterval: time.Second,
		PingWait:     3 * time.Second,
	})
	if err := client.SetUUID("stable-client"); err != nil {
		panic(err)
	}
	defer client.Close()

	connected := make(chan string, 2)
	disconnected := make(chan struct{}, 1)
	received := make(chan string, 1)
	client.OnConnected = func() {
		connected <- client.GetUUID()
	}
	client.OnDisconnected = func(err error) {
		disconnected <- struct{}{}
	}
	client.OnTextMessage = func(message string) {
		received <- message
	}

	if err := client.Connect(); err != nil {
		panic(err)
	}
	fmt.Println("首次连接:", exampleReceive("首次连接", connected))

	firstConn := exampleReceive("首个服务端连接", serverConns)
	if err := firstConn.Close(); err != nil {
		panic(err)
	}
	exampleReceive("断开回调", disconnected)

	if err := client.Reconnect(); err != nil {
		panic(err)
	}
	fmt.Println("重连后 UUID:", exampleReceive("重连事件", connected))

	client.Emit([]byte("重连成功"))
	fmt.Println("收到回声:", exampleReceive("重连回声", received))

	// Output:
	// 首次连接: stable-client
	// 重连后 UUID: stable-client
	// 收到回声: 重连成功
}

func ExampleWebsocketWrapper_GetRequestHeader() {
	sm := socketio.NewSocketInstance()
	authorization := make(chan string, 1)

	sm.On(socketio.EventConnect, func(payload *socketio.EventPayload) {
		authorization <- payload.Kws.GetRequestHeader("Authorization")
	})
	server := httptest.NewServer(sm.New())
	defer server.Close()

	requestHeader := http.Header{}
	requestHeader.Set("Authorization", "Bearer 示例令牌")
	clientConn, _, err := websocket.DefaultDialer.Dial(exampleWSURL(server.URL), requestHeader)
	if err != nil {
		panic(err)
	}
	defer clientConn.Close()

	fmt.Println("握手头:", exampleReceive("握手头", authorization))

	// Output:
	// 握手头: Bearer 示例令牌
}
