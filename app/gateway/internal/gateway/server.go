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
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/pkg/gatewayproto"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/gorilla/websocket"
)

// 网关稳定性相关的硬上限与超时，集中声明便于复核与测试：
//   - httpReadHeaderTimeout / httpIdleTimeout：HTTP 读头与空闲超时，避免
//     慢速/空闲连接长期占用 accept 协程与文件描述符；
//   - wsReadDeadlinePreAuth：鉴权首帧必须在限时内到达，杜绝握手期挂死；
//   - wsWriteDeadline：单条写出限时，写阻塞由它兜底（不靠全局读超时，
//     以免误杀鉴权后的正常空闲长连接）；
//   - maxStreams：单连接应用层流数硬上限，超限明确拒绝而非无限膨胀；
//   - outBufSize：单连接出站帧队列容量，满时显式终止会话而非静默丢帧。
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpIdleTimeout       = 60 * time.Second
	wsReadDeadlinePreAuth = 10 * time.Second
	wsWriteDeadline       = 15 * time.Second
	maxStreams            = 128
	outBufSize            = 128
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
	serviceConn *wsConn            // 当前 service 模式连接（至多一个）
	clients     map[string]*wsConn // 游戏客户端路由表：鉴权 name → 连接（玩家互连用）
}

// nodeInfo 网关节点身份。client 未就绪（如测试桩）时返回零值，不 panic。
func (s *Server) nodeInfo() lanet.Info {
	if s.client == nil {
		return lanet.Info{}
	}
	return s.client.Info()
}

// httpServer 构造带稳定性超时的 HTTP/WS 服务：读头超时与空闲超时由连接
// 进入 WS 升级前兜底，避免慢速或空闲连接长期占用文件描述符；空闲超时仅
// 作用于尚未升级的 HTTP 连接，升级后的 WebSocket 长连接不受其影响。
func (s *Server) httpServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
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
	// Peer 目标客户端鉴权名（玩家互连）。非空时网关在本机两个客户端
	// 连接之间桥接一条流，忽略 ip/port；为空时保持原语义（网格内开流）。
	Peer string `json:"peer"`
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client, err := lanet.New(ctx, lanet.Config{
		CTLURL:     cfg.CTLURL,
		InviteCode: cfg.InviteCode,
		GroupName:  cfg.GroupName,
		Name:       cfg.Name,
		// 网关职责是桥接 ws 客户端与群内成员：必须放行来自任意成员的
		// Tunnel 应用流入向（service 模式依赖），其余暴露面保持默认拒绝。
		FirewallMode: lanet.FirewallModeAllowList,
		FirewallRules: []lanet.FirewallRule{
			{Source: "*", Proto: firewall.ProtoAny},
		},
	})
	if err != nil {
		return nil, err
	}
	info := client.Info()
	log.Printf("[gateway] 网关节点已入群 group=%s virtual_ip=%s", info.Group, info.VirtualIP)

	// SDK 周期任务（NetMap 刷新 / 地址通告 / 中继预约），开流依赖 NetMap。
	go client.Run(ctx)

	s := &Server{cfg: cfg, client: client, clients: make(map[string]*wsConn)}

	// 入向流分发：交给 service 连接（没有则拒绝）。
	inbound := make(chan lanet.Stream)
	client.OnStream(func(stream lanet.Stream) {
		select {
		case inbound <- stream:
		case <-ctx.Done():
			_ = stream.Reset()
		default:
			_ = stream.Reset()
		}
	})
	go s.dispatchInbound(ctx, inbound)

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.srv = s.httpServer(mux)
	s.srv.BaseContext = func(net.Listener) context.Context { return ctx }

	go func() {
		<-ctx.Done()
		_ = s.srv.Close()
	}()
	log.Printf("[gateway] WS 监听 %s%s", cfg.ListenAddr, cfg.Path)
	if err = s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		_ = client.Close()
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
			if ctx.Err() != nil {
				_ = stream.Reset()
				return
			}
			s.mu.Lock()
			svc := s.serviceConn
			s.mu.Unlock()
			if svc == nil {
				log.Printf("[gateway] 无 service 连接，拒绝入向流 remote=%s", stream.Protocol())
				_ = stream.Reset()
				continue
			}
			id := svc.newStreamID()
			st := newMeshStream(stream)
			if !svc.track(id, st) {
				continue
			}
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
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(wsReadDeadlinePreAuth))

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn := &wsConn{
		ctx:        ctx,
		cancel:     cancel,
		ws:         ws,
		out:        make(chan []byte, outBufSize),
		writerDone: make(chan struct{}),
		streams:    make(map[uint32]*meshStream),
	}
	go conn.writeLoop()
	stopCancel := context.AfterFunc(ctx, func() {
		_ = ws.Close()
		conn.closeAllStreams()
	})
	defer stopCancel()
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
	info := s.nodeInfo()
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

	// 游戏客户端路由登记（玩家互连按 name 寻址）：name 全局唯一，重名拒绝，
	// 避免两台设备注册同一名字后互相打错人。service 连接同样入表（可被 Dial）。
	conn.name = req.Name
	s.mu.Lock()
	if s.clients == nil {
		s.clients = make(map[string]*wsConn)
	}
	if _, exists := s.clients[req.Name]; exists {
		s.mu.Unlock()
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthErr,
			Payload: mustJSON(authErr{Error: "名称已被占用，请换一个名字再连"})})
		return
	}
	s.clients[req.Name] = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.clients[req.Name] == conn {
			delete(s.clients, req.Name)
		}
		s.mu.Unlock()
	}()

	log.Printf("[gateway] 客户端接入 name=%q mode=%s", req.Name, req.Mode)
	conn.send(gatewayproto.Frame{Type: gatewayproto.TypeAuthOk, Payload: mustJSON(authOk{
		VirtualIP: info.VirtualIP, PeerID: info.PeerID, Group: info.Group, Mode: req.Mode,
	})})

	// 鉴权后允许正常空闲长连接，写阻塞另由写截止时间保护。
	_ = ws.SetReadDeadline(time.Time{})
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
				// 双方都已半关 → 桥接流终结：摘流并唤醒对侧 pump。
				if fc, ok := st.rw.(interface{ FullyHalfClosed() bool }); ok && fc.FullyHalfClosed() {
					if s2 := conn.untrack(frame.StreamID); s2 != nil {
						_ = s2.close()
					}
				}
			}
		case gatewayproto.TypeReset:
			if st := conn.untrack(frame.StreamID); st != nil {
				if r, ok := st.rw.(interface{ Reset() error }); ok {
					_ = r.Reset()
				} else {
					_ = st.rw.Close()
				}
				st.finish() // 唤醒 EOF 挂起中的 pump
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
	if err := json.Unmarshal(frame.Payload, &req); err != nil || (req.IP == "" && req.Peer == "") {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id,
			Payload: []byte("Dial 载荷非法：需 {ip, port, protocol?} 或 {peer, protocol?}")})
		return
	}

	// 玩家互连：目标为同网关在线客户端的鉴权名，网关在两条 WS 连接之间
	// 桥接一条流。协议子集（帧语义）与网格内开流完全一致，客户端无感。
	if req.Peer != "" {
		s.bridgeDial(conn, id, req)
		return
	}

	ctx := conn.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if req.Protocol != "" {
		// 自定义协议直开流（对端需注册了该协议处理器）。
		stream, viaRelay, err := s.client.DialProtocol(ctx, req.IP, req.Protocol)
		if err != nil {
			conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte(err.Error())})
			return
		}
		st := newMeshStream(stream)
		if !conn.track(id, st) {
			conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("流数超限或 ID 已占用")})
			return
		}
		if !conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialOk, StreamID: id,
			Payload: mustJSON(dialOk{ViaRelay: viaRelay})}) {
			return
		}
		go conn.pumpStreamToClient(id, st)
		return
	}

	// 端口转发语义：ip:port 的 TCP 服务。
	if req.Port <= 0 || req.Port > 65535 {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("无效端口")})
		return
	}
	nc, err := s.client.DialPortFWD(ctx, lanet.PortFWDTarget{VirtualIP: req.IP, Port: req.Port})
	if err != nil {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte(err.Error())})
		return
	}
	viaRelay := false
	if pfc, ok := nc.(*lanet.PortFWDConn); ok {
		viaRelay = pfc.ViaRelay()
	}
	st := newMeshStream(nc)
	if !conn.track(id, st) {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("流数超限或 ID 已占用")})
		return
	}
	if !conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialOk, StreamID: id,
		Payload: mustJSON(dialOk{ViaRelay: viaRelay})}) {
		return
	}
	go conn.pumpStreamToClient(id, st)
}

// bridgeDial 客户端互连：把发起方与目标客户端的两条 WS 连接桥接成一条流。
//
// 数据面：发起方 Data 帧 → 发起管道 → 目标管道 → 目标侧 Data 帧，反向对称。
// 两侧各自分配独立的 streamID（与各自连接上的其他流互不冲突），目标侧以
// StreamOpen 帧获知入向流（protocol + 发起方鉴权名）。目标断开时管道
// Close，两侧 pump 自然收尾，语义与网格内流一致。
func (s *Server) bridgeDial(conn *wsConn, id uint32, req dialReq) {
	s.mu.Lock()
	target := s.clients[req.Peer]
	s.mu.Unlock()
	if target == nil {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id,
			Payload: []byte("玩家不在线或名称不存在: " + req.Peer)})
		return
	}
	if target == conn {
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id,
			Payload: []byte("不能连接自己")})
		return
	}

	pipeA, pipeB := newBridgePipePair()

	// 发起方侧：复用普通出向流语义（DialOk + Data 泵）。
	stA := newMeshStream(pipeA)
	if !conn.track(id, stA) {
		_ = pipeB.Close()
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("流数超限或 ID 已占用")})
		return
	}
	// 目标侧：分配新 streamID，以 StreamOpen 通知（与网格入向流同帧型）。
	targetID := target.newStreamID()
	stB := newMeshStream(pipeB)
	if !target.track(targetID, stB) {
		// 回拆必须摘表并通知 done，单关管道会把 EOF 泵留在等待中。
		if st := conn.untrack(id); st != nil {
			_ = st.close()
		}
		_ = pipeB.Close()
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("目标流数超限或连接已关闭")})
		return
	}
	rollback := func() {
		if st := conn.untrack(id); st != nil {
			_ = st.close()
		}
		if st := target.untrack(targetID); st != nil {
			_ = st.close()
		}
	}
	if !target.send(gatewayproto.Frame{Type: gatewayproto.TypeStreamOpen, StreamID: targetID,
		Payload: mustJSON(streamOpen{Protocol: req.Protocol, RemotePeer: conn.name})}) {
		rollback()
		conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialErr, StreamID: id, Payload: []byte("目标连接已关闭")})
		return
	}
	if !conn.send(gatewayproto.Frame{Type: gatewayproto.TypeDialOk, StreamID: id,
		Payload: mustJSON(dialOk{ViaRelay: false})}) {
		rollback()
		target.send(gatewayproto.Frame{Type: gatewayproto.TypeReset, StreamID: targetID})
		return
	}
	go conn.pumpStreamToClient(id, stA)
	go target.pumpStreamToClient(targetID, stB)
}

// halfCloseWriter 支持半关闭写端的流（libp2p 流、TCP 连接等）。
type halfCloseWriter interface{ CloseWrite() error }

// meshStream 一条活跃流（端口转发 net.Conn 或原始 lanet.Stream，或桥接
// 管道）。两者都满足 io.ReadWriteCloser，统一走 rw。
//
// done 是流终结通知：对端 Reset、双方半关（桥接流）、连接断开时关闭，
// 用于让 EOF（对端半关）后挂起的 pump 知道何时收尾——半关闭是单向的，
// 对端半关后本端仍可能继续发送，不能提前摘除流。
type meshStream struct {
	rw        io.ReadWriteCloser
	done      chan struct{}
	closeOnce sync.Once
}

func newMeshStream(rw io.ReadWriteCloser) *meshStream {
	return &meshStream{rw: rw, done: make(chan struct{})}
}

// finish 标记流终结并唤醒等待者；幂等。
func (st *meshStream) finish() { st.closeOnce.Do(func() { close(st.done) }) }

// close 关闭底层流并通知终结。
func (st *meshStream) close() error {
	st.finish()
	return st.rw.Close()
}

// wsConn 一条已鉴权的客户端 WebSocket 连接。
type wsConn struct {
	ctx        context.Context
	cancel     context.CancelFunc
	ws         *websocket.Conn
	name       string // 鉴权名（玩家互连的路由键，网关内唯一）
	out        chan []byte
	writerDone chan struct{} // writeLoop 退出时关闭：closeAllStreams 等它排空队列后再关 ws
	nextID     atomic.Uint32
	mu         sync.Mutex
	streams    map[uint32]*meshStream
	closed     bool
}

func (c *wsConn) newStreamID() uint32 { return c.nextID.Add(1) }

func (c *wsConn) track(id uint32, st *meshStream) bool {
	c.mu.Lock()
	if c.closed || len(c.streams) >= maxStreams || c.streams[id] != nil {
		c.mu.Unlock()
		_ = st.close()
		return false
	}
	c.streams[id] = st
	c.mu.Unlock()
	return true
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
	select {
	case c.out <- gatewayproto.Marshal(f):
		c.mu.Unlock()
		return true
	default:
		c.mu.Unlock()
		log.Printf("[gateway] 发送队列满，关闭会话避免静默丢数据 type=%d", f.Type)
		c.closeAllStreams()
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
// 先通知写循环退出，再清理会话，避免清理等待自身；写失败必须唤醒读端。
func (c *wsConn) writeLoop() {
	defer func() {
		close(c.writerDone)
		c.closeAllStreams()
	}()
	for msg := range c.out {
		_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		if err := c.ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return
		}
	}
}

// pumpStreamToClient 把网格流的数据以 Data 帧推给客户端，EOF 发 Close。
//
// EOF 只代表对端半关闭写端，流并未终结（本端仍可继续发送，对端仍会
// 继续发数据过来）：发完 Close 帧后挂起等待流终结（对端 Reset、双方
// 半关、连接断开），绝不能提前 untrack——否则对端后续 Data 帧在路由表
// 里找不到落点，被静默丢弃（TCP 全双工语义被破坏）。
func (c *wsConn) pumpStreamToClient(id uint32, st *meshStream) {
	defer c.untrack(id)
	buf := make([]byte, 16*1024)
	for {
		n, err := st.rw.Read(buf)
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
			<-st.done // 等流终结；untrack 交由 defer
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
	// send 与关闭通道共用锁，让空队列上的写协程也能退出。
	close(c.out)
	streams := c.streams
	c.streams = make(map[uint32]*meshStream)
	c.mu.Unlock()
	for _, st := range streams {
		_ = st.close()
	}
	// 等写循环把已入队消息写完再关底层连接（writerDone 为 nil 说明是
	// 单测里手工构造的连接，无写循环）。
	if c.ws != nil && c.writerDone != nil {
		timer := time.AfterFunc(wsWriteDeadline, func() { _ = c.ws.Close() })
		<-c.writerDone
		timer.Stop()
		_ = c.ws.Close()
	} else if c.ws != nil {
		_ = c.ws.Close()
	}
	if c.cancel != nil {
		c.cancel()
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
