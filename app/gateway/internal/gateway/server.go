// Package gateway ws-gateway：让无法运行 libp2p 的客户端
// （C# / Unity / 小程序 / uniapp 等）经 WebSocket 帧协议接入群组网格。
//
// 网关本身以 Go SDK 节点身份入群，客户端连接后：
//   - client 模式：dial{ip, port} 经 PortFWD 访问网格内 TCP 服务，
//     或 dial{ip, protocol} 打开自定义协议流；
//   - service 模式：接收网格内其他节点对本网关身份发来的隧道流。
//
// 帧协议见 pkg/gatewayproto（一条二进制 WS 消息 = 一个帧）。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/ayflying/pvn/pkg/gatewayproto"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/gorilla/websocket"
)

// Config 网关配置。
type Config struct {
	// CTLURL 控制面地址。
	CTLURL string
	// InviteCode 网关加入的群组邀请码；为空则创建新群组并打印。
	InviteCode string
	// GroupName 创建模式下的群组名。
	GroupName string
	// ListenAddr WS 监听地址，默认 ":8700"。
	ListenAddr string
	// Path WS 路径，默认 "/gateway"。
	Path string
	// Name 网关节点名称。
	Name string
}

// Server ws-gateway 服务。
type Server struct {
	cfg    Config
	client *lanet.Client
	srv    *http.Server

	mu          sync.Mutex
	serviceConn *wsConn // 当前 service 模式连接（至多一个）
}

// 鉴权请求/应答。
type authReq struct {
	InviteCode string `json:"invite_code"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
}

type authOk struct {
	VirtualIP string `json:"virtual_ip"`
	PeerID    string `json:"peer_id"`
	Group     string `json:"group"`
	Mode      string `json:"mode"`
}

type authErr struct {
	Error string `json:"error"`
}

type dialReq struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

type dialOk struct {
	ViaRelay bool `json:"via_relay"`
}

type streamOpen struct {
	Protocol   string `json:"protocol"`
	RemotePeer string `json:"remote_peer"`
}

// Run 启动网关并阻塞直到 ctx 取消。
func Run(ctx context.Context, cfg Config) (*lanet.Client, error) {
	if cfg.CTLURL == "" {
		return nil, fmt.Errorf("gateway: CTLURL 必填")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8700"
	}
	if cfg.Path == "" {
		cfg.Path = "/gateway"
	}
	if cfg.Name == "" {
		cfg.Name = "gateway"
	}

	client, err := lanet.New(ctx, lanet.Config{
		CTLURL:     cfg.CTLURL,
		InviteCode: cfg.InviteCode,
		GroupName:  cfg.GroupName,
		Name:       cfg.Name,
	})
	if err != nil {
		return nil, err
	}
	info := client.Info()
	log.Printf("[gateway] 网关节点已入群 group=%s virtual_ip=%s", info.Group, info.VirtualIP)

	// SDK 周期任务（NetMap 刷新 / 地址通告 / 中继预约），开流依赖 NetMap。
	go client.Run(ctx)

	s := &Server{cfg: cfg, client: client}

	// 入向流分发：交给 service 连接（没有则拒绝）。
	inbound := make(chan lanet.Stream, 16)
	client.OnStream(func(stream lanet.Stream) {
		inbound <- stream
	})
	go s.dispatchInbound(ctx, inbound)

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.srv = &http.Server{Addr: cfg.ListenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		_ = s.srv.Close()
	}()
	log.Printf("[gateway] WS 监听 %s%s", cfg.ListenAddr, cfg.Path)
	if err = s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return client, fmt.Errorf("gateway: %w", err)
	}
	return client, nil
}

// dispatchInbound 把网格内入向流推给 service 连接。
func (s *Server) dispatchInbound(ctx context.Context, inbound chan lanet.Stream) {
	for {
		select {
		case <-ctx.Done():
			return
		case stream := <-inbound:
			s.mu.Lock()
			svc := s.serviceConn
			s.mu.Unlock()
			if svc == nil {
				log.Printf("[gateway] 无 service 连接，拒绝入向流 remote=%s", stream.Protocol())
				_ = stream.Reset()
				continue
			}
			id := svc.newStreamID()
			st := &meshStream{rw: stream}
			svc.track(id, st)
			payload, _ := json.Marshal(streamOpen{
				Protocol:   stream.Protocol(),
				RemotePeer: remotePeerOf(stream),
			})
			if !svc.send(gatewayproto.Frame{Type: gatewayproto.TypeStreamOpen, StreamID: id, Payload: payload}) {
				_ = stream.Reset()
				svc.untrack(id)
				continue
			}
			go svc.pumpStreamToClient(id, st)
		}
	}
}

// handleWS 升级并服务一条客户端连接。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // SDK 客户端无浏览器同源限制语义
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway] WS 升级失败: %v", err)
		return
	}
	defer ws.Close()

	conn := &wsConn{
		ws:      ws,
		out:     make(chan []byte, 128),
		streams: make(map[uint32]*meshStream),
	}
	go conn.writeLoop()
	defer conn.closeAllStreams()

	// 1. 首帧必须是鉴权。
	msg, err := conn.readMessage()
	if err != nil {
		return
	}
	frame, err := gatewayproto.Unmarshal(msg)
	if err != nil || frame.Type != gatewayproto.TypeAuth {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
			Payload: mustJSON(authErr{Error: "首帧必须为 Auth"})})
		return
	}
	var req authReq
	if err = json.Unmarshal(frame.Payload, &req); err != nil {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
			Payload: mustJSON(authErr{Error: "Auth 载荷非法"})})
		return
	}
	info := s.client.Info()
	if req.InviteCode != info.InviteCode {
		log.Printf("[gateway] 鉴权失败：邀请码不匹配 name=%q", req.Name)
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
			Payload: mustJSON(authErr{Error: "邀请码无效"})})
		return
	}
	if req.Mode == "" {
		req.Mode = gatewayproto.ModeClient
	}
	if req.Mode != gatewayproto.ModeClient && req.Mode != gatewayproto.ModeService {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
			Payload: mustJSON(authErr{Error: "mode 仅支持 client / service"})})
		return
	}

	// service 模式：同一时刻仅一个连接（入向流路由需要确定性）。
	if req.Mode == gatewayproto.ModeService {
		s.mu.Lock()
		if s.serviceConn != nil {
			s.mu.Unlock()
			conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
				Payload: mustJSON(authErr{Error: "已有 service 连接占用"})})
			return
		}
		s.serviceConn = conn
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			if s.serviceConn == conn {
				s.serviceConn = nil
			}
			s.mu.Unlock()
		}()
	}

	log.Printf("[gateway] 客户端接入 name=%q mode=%s", req.Name, req.Mode)
	conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthOk, Payload: mustJSON(authOk{
		VirtualIP: info.VirtualIP, PeerID: info.PeerID, Group: info.Group, Mode: req.Mode,
	})})

	// 2. 主循环：处理客户端帧。
	for {
		msg, err = conn.readMessage()
		if err != nil {
			log.Printf("[gateway] 客户端断开 name=%q: %v", req.Name, err)
			return
		}
		frame, err = gatewayproto.Unmarshal(msg)
		if err != nil {
			continue
		}
		switch frame.Type {
		case gatewayproto.TypeDial:
			s.handleDial(conn, frame)
		case gatewayproto.TypeData:
			if st := conn.get(frame.StreamID); st != nil {
				_, _ = st.rw.Write(frame.Payload)
			}
		case gatewayproto.TypeClose:
			if st := conn.get(frame.StreamID); st != nil {
				if hc, ok := st.rw.(halfCloseWriter); ok {
					_ = hc.CloseWrite()
				}
			}
		case gatewayproto.TypeReset:
			if st := conn.untrack(frame.StreamID); st != nil {
				if r, ok := st.rw.(interface{ Reset() error }); ok {
					_ = r.Reset()
				} else {
					_ = st.rw.Close()
				}
			}
		case gatewayproto.TypePing:
			conn.send(gatewayproto.Frame{Type: gatewayproto.TypePong, Payload: frame.Payload})
		default:
			// 未知类型忽略（前向兼容）。
		}
	}
}

// handleDial 处理开流请求。
func (s *Server) handleDial(conn *wsConn, frame gatewayproto.Frame) {
	var req dialReq
	id := frame.StreamID
	if err := json.Unmarshal(frame.Payload, &req); err != nil || req.IP == "" {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id,
			Payload: []byte("Dial 载荷非法：需 {ip, port, protocol?}")})
		return
	}

	if req.Protocol != "" {
		// 自定义协议直开流（对端需注册了该协议处理器）。
		stream, viaRelay, err := s.client.DialProtocol(context.Background(), req.IP, req.Protocol)
		if err != nil {
			conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte(err.Error())})
			return
		}
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialOk, StreamID: id,
			Payload: mustJSON(dialOk{ViaRelay: viaRelay})})
		st := &meshStream{rw: stream}
		conn.track(id, st)
		go conn.pumpStreamToClient(id, st)
		return
	}

	// 端口转发语义：ip:port 的 TCP 服务。
	if req.Port <= 0 || req.Port > 65535 {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("无效端口")})
		return
	}
	nc, err := s.client.DialPortFWD(context.Background(), lanet.PortFWDTarget{VirtualIP: req.IP, Port: req.Port})
	if err != nil {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte(err.Error())})
		return
	}
	viaRelay := false
	if pfc, ok := nc.(*lanet.PortFWDConn); ok {
		viaRelay = pfc.ViaRelay()
	}
	conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialOk, StreamID: id,
		Payload: mustJSON(dialOk{ViaRelay: viaRelay})})
	st := &meshStream{rw: nc}
	conn.track(id, st)
	go conn.pumpStreamToClient(id, st)
}

// halfCloseWriter 支持半关闭写端的流（libp2p 流、TCP 连接等）。
type halfCloseWriter interface{ CloseWrite() error }

// meshStream 一条活跃流（端口转发 net.Conn 或原始 lanet.Stream）。
// 两者都满足 io.ReadWriteCloser，统一走 rw。
type meshStream struct {
	rw io.ReadWriteCloser
}

func (st *meshStream) read(p []byte) (int, error) { return st.rw.Read(p) }
func (st *meshStream) close() error               { return st.rw.Close() }

// wsConn 一条已鉴权的客户端 WebSocket 连接。
type wsConn struct {
	ws      *websocket.Conn
	out     chan []byte
	nextID  atomic.Uint32
	mu      sync.Mutex
	streams map[uint32]*meshStream
	closed  bool
}

func (c *wsConn) newStreamID() uint32 { return c.nextID.Add(1) }

func (c *wsConn) track(id uint32, st *meshStream) {
	c.mu.Lock()
	c.streams[id] = st
	c.mu.Unlock()
}

func (c *wsConn) get(id uint32) *meshStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *wsConn) untrack(id uint32) *meshStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.streams[id]
	delete(c.streams, id)
	return st
}

func (c *wsConn) send(f gatewayproto.Frame) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()
	select {
	case c.out <- gatewayproto.Marshal(f):
		return true
	default:
		log.Printf("[gateway] 发送队列满，丢弃帧 type=%d", f.Type)
		return false
	}
}

// readMessage 读一条 WS 消息（二进制/文本均可）。
func (c *wsConn) readMessage() ([]byte, error) {
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return nil, err
		}
		if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
			return data, nil
		}
	}
}

// writeLoop 单协程串行写 WS（gorilla 不支持并发写）。
func (c *wsConn) writeLoop() {
	for msg := range c.out {
		if err := c.ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return
		}
	}
}

// pumpStreamToClient 把网格流的数据以 Data 帧推给客户端，EOF 发 Close。
func (c *wsConn) pumpStreamToClient(id uint32, st *meshStream) {
	defer c.untrack(id)
	buf := make([]byte, 16*1024)
	for {
		n, err := st.read(buf)
		if n > 0 {
			if !c.send(gatewayproto.Frame{Type: gatewayproto.TypeData, StreamID: id,
				Payload: buf[:n]}) {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[gateway] 流读错误 stream=%d: %v", id, err)
				c.send(gatewayproto.Frame{Type: gatewayproto.TypeReset, StreamID: id})
				_ = st.close()
				return
			}
			c.send(gatewayproto.Frame{Type: gatewayproto.TypeClose, StreamID: id})
			return
		}
	}
}

func (c *wsConn) closeAllStreams() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	streams := c.streams
	c.streams = make(map[uint32]*meshStream)
	c.mu.Unlock()
	// 不 close(c.out)：writeLoop 依赖 ws.WriteMessage 失败退出，
	// 避免 send 与 close 的竞态 panic。
	for _, st := range streams {
		_ = st.close()
	}
}

// remotePeerOf 提取流对端 PeerID（streamAdapter 实现了 RemotePeer）。
func remotePeerOf(stream lanet.Stream) string {
	type peerIDer interface{ RemotePeer() string }
	if p, ok := stream.(peerIDer); ok {
		return p.RemotePeer()
	}
	return ""
}

// mustJSON 编码，失败返回空对象（编码内容全部为内部可控结构）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
