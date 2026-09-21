package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/gatewayproto"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/gorilla/websocket"
)

// ---- 测试基建：起一个带桥接能力的真网关（client 桩不入群，仅测 WS 面）----

type bridgeGW struct {
	srv *httptest.Server
	url string // ws://host/gateway
}

func newBridgeGW(t *testing.T) *bridgeGW {
	t.Helper()
	s := &Server{
		cfg:     Config{Path: "/gateway"},
		client:  &lanet.Client{}, // 零值桩：nodeInfo() 返回零值，InviteCode 为空
		clients: make(map[string]*wsConn),
	}
	srv := httptest.NewServer(http.HandlerFunc(s.handleWS))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway"
	return &bridgeGW{srv: srv, url: url}
}

// dialClient 建立一条已鉴权的客户端连接。
func (g *bridgeGW) dialClient(t *testing.T, name, mode string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(g.url, nil)
	if err != nil {
		t.Fatalf("连接网关失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	auth, _ := json.Marshal(authReq{InviteCode: "", Name: name, Mode: mode})
	if err := c.WriteMessage(websocket.BinaryMessage,
		gatewayproto.Marshal(gatewayproto.Frame{Type: gatewayproto.TypeAuth, Payload: auth})); err != nil {
		t.Fatalf("发送 Auth 失败: %v", err)
	}
	f := readFrame(t, c)
	if f.Type == gatewayproto.TypeAuthErr {
		t.Fatalf("鉴权被拒 name=%s: %s", name, f.Payload)
	}
	if f.Type != gatewayproto.TypeAuthOk {
		t.Fatalf("鉴权应答类型错误: %d", f.Type)
	}
	return c
}

// readFrame 带超时读一帧。
func readFrame(t *testing.T, c *websocket.Conn) gatewayproto.Frame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	f, err := gatewayproto.Unmarshal(data)
	if err != nil {
		t.Fatalf("解帧失败: %v", err)
	}
	return f
}

func sendFrame(t *testing.T, c *websocket.Conn, f gatewayproto.Frame) {
	t.Helper()
	if err := c.WriteMessage(websocket.BinaryMessage, gatewayproto.Marshal(f)); err != nil {
		t.Fatalf("发帧失败: %v", err)
	}
}

// ---- 桥接全流程 ----

// TestBridgeRoundTrip 双客户端互连：开流 → 双向数据 → 半关闭 → 关闭后对端仍可发。
func TestBridgeRoundTrip(t *testing.T) {
	g := newBridgeGW(t)
	alice := g.dialClient(t, "alice", gatewayproto.ModeClient)
	bob := g.dialClient(t, "bob", gatewayproto.ModeClient)

	// alice 开流到 bob。
	req, _ := json.Marshal(dialReq{Peer: "bob", Protocol: "/game/1.0.0"})
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeDial, StreamID: 101, Payload: req})

	f := readFrame(t, alice)
	if f.Type != gatewayproto.TypeDialOk || f.StreamID != 101 {
		t.Fatalf("期望 DialOk(101)，收到 type=%d sid=%d payload=%s", f.Type, f.StreamID, f.Payload)
	}

	// bob 收到入向流（StreamOpen），记录网关分配的 bID。
	f = readFrame(t, bob)
	if f.Type != gatewayproto.TypeStreamOpen {
		t.Fatalf("期望 StreamOpen，收到 type=%d payload=%s", f.Type, f.Payload)
	}
	var open streamOpen
	if err := json.Unmarshal(f.Payload, &open); err != nil {
		t.Fatalf("StreamOpen 载荷解析失败: %v", err)
	}
	if open.Protocol != "/game/1.0.0" || open.RemotePeer != "alice" {
		t.Fatalf("StreamOpen 元数据错误: %+v", open)
	}
	bID := f.StreamID

	// alice → bob。
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeData, StreamID: 101, Payload: []byte("hello bob")})
	f = readFrame(t, bob)
	if f.Type != gatewayproto.TypeData || f.StreamID != bID || string(f.Payload) != "hello bob" {
		t.Fatalf("bob 侧数据错误: type=%d sid=%d payload=%q", f.Type, f.StreamID, f.Payload)
	}

	// bob → alice（反向）。
	sendFrame(t, bob, gatewayproto.Frame{Type: gatewayproto.TypeData, StreamID: bID, Payload: []byte("hi alice")})
	f = readFrame(t, alice)
	if f.Type != gatewayproto.TypeData || f.StreamID != 101 || string(f.Payload) != "hi alice" {
		t.Fatalf("alice 侧数据错误: type=%d sid=%d payload=%q", f.Type, f.StreamID, f.Payload)
	}

	// alice 半关闭写端 → bob 读到 EOF（Close 帧）。
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeClose, StreamID: 101})
	f = readFrame(t, bob)
	if f.Type != gatewayproto.TypeClose || f.StreamID != bID {
		t.Fatalf("期望 Close(bID)，收到 type=%d sid=%d", f.Type, f.StreamID)
	}

	// 半关闭是单向的：bob 仍可继续发送，alice 仍可继续收。
	sendFrame(t, bob, gatewayproto.Frame{Type: gatewayproto.TypeData, StreamID: bID, Payload: []byte("still-alive")})
	f = readFrame(t, alice)
	if f.Type != gatewayproto.TypeData || string(f.Payload) != "still-alive" {
		t.Fatalf("半关闭后反向数据应可达: type=%d payload=%q", f.Type, f.Payload)
	}

	// bob 也半关闭 → alice 收 Close，流双向终结。
	sendFrame(t, bob, gatewayproto.Frame{Type: gatewayproto.TypeClose, StreamID: bID})
	f = readFrame(t, alice)
	if f.Type != gatewayproto.TypeClose || f.StreamID != 101 {
		t.Fatalf("期望 Close(101)，收到 type=%d sid=%d", f.Type, f.StreamID)
	}
}

// TestBridgeReset 一端强制中止 → 对端收到 Reset。
func TestBridgeReset(t *testing.T) {
	g := newBridgeGW(t)
	alice := g.dialClient(t, "alice", gatewayproto.ModeClient)
	bob := g.dialClient(t, "bob", gatewayproto.ModeClient)

	req, _ := json.Marshal(dialReq{Peer: "bob", Protocol: "/game/1.0.0"})
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeDial, StreamID: 7, Payload: req})
	readFrame(t, alice) // DialOk
	f := readFrame(t, bob)
	if f.Type != gatewayproto.TypeStreamOpen {
		t.Fatalf("期望 StreamOpen，收到 type=%d", f.Type)
	}
	bID := f.StreamID

	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeReset, StreamID: 7})
	f = readFrame(t, bob)
	if f.Type != gatewayproto.TypeReset || f.StreamID != bID {
		t.Fatalf("期望 Reset(bID)，收到 type=%d sid=%d", f.Type, f.StreamID)
	}
}

// TestBridgePeerOffline 目标不在线 → DialErr。
func TestBridgePeerOffline(t *testing.T) {
	g := newBridgeGW(t)
	alice := g.dialClient(t, "alice", gatewayproto.ModeClient)

	req, _ := json.Marshal(dialReq{Peer: "nobody", Protocol: "/game/1.0.0"})
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeDial, StreamID: 9, Payload: req})
	f := readFrame(t, alice)
	if f.Type != gatewayproto.TypeDialErr || f.StreamID != 9 {
		t.Fatalf("期望 DialErr(9)，收到 type=%d sid=%d", f.Type, f.StreamID)
	}
}

// TestBridgeDialSelf 不能连自己。
func TestBridgeDialSelf(t *testing.T) {
	g := newBridgeGW(t)
	alice := g.dialClient(t, "alice", gatewayproto.ModeClient)

	req, _ := json.Marshal(dialReq{Peer: "alice", Protocol: "/game/1.0.0"})
	sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeDial, StreamID: 11, Payload: req})
	f := readFrame(t, alice)
	if f.Type != gatewayproto.TypeDialErr || f.StreamID != 11 {
		t.Fatalf("期望 DialErr(11)，收到 type=%d sid=%d", f.Type, f.StreamID)
	}
}

// TestAuthDuplicateName 重名拒绝。
func TestAuthDuplicateName(t *testing.T) {
	g := newBridgeGW(t)
	g.dialClient(t, "alice", gatewayproto.ModeClient)

	c, _, err := websocket.DefaultDialer.Dial(g.url, nil)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	auth, _ := json.Marshal(authReq{InviteCode: "", Name: "alice", Mode: gatewayproto.ModeClient})
	_ = c.WriteMessage(websocket.BinaryMessage,
		gatewayproto.Marshal(gatewayproto.Frame{Type: gatewayproto.TypeAuth, Payload: auth}))
	f := readFrame(t, c)
	if f.Type != gatewayproto.TypeAuthErr {
		t.Fatalf("重名应被拒绝，收到 type=%d", f.Type)
	}
}

// TestBridgeDisconnectCleanup 目标断开后开流应报「不在线」。
func TestBridgeDisconnectCleanup(t *testing.T) {
	g := newBridgeGW(t)
	alice := g.dialClient(t, "alice", gatewayproto.ModeClient)
	bob := g.dialClient(t, "bob", gatewayproto.ModeClient)
	_ = bob.Close()
	// 给网关一点时间处理断开；每次重试换 streamID，避免同 ID 重复登记被丢弃。
	deadline := time.Now().Add(2 * time.Second)
	sid := uint32(21)
	for {
		sid++
		req, _ := json.Marshal(dialReq{Peer: "bob", Protocol: "/game/1.0.0"})
		sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeDial, StreamID: sid, Payload: req})
		f := readFrame(t, alice)
		if f.Type == gatewayproto.TypeDialErr {
			return // 路由已清理
		}
		if f.Type == gatewayproto.TypeDialOk {
			sendFrame(t, alice, gatewayproto.Frame{Type: gatewayproto.TypeReset, StreamID: sid})
		}
		if time.Now().After(deadline) {
			t.Fatal("目标断开后路由未清理")
		}
	}
}
