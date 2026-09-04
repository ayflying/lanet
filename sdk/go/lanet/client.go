// Package lanet 是 Lanet 群组制 P2P 虚拟局域网的 Go SDK。
//
// 宿主程序只需十几行代码即可作为一个节点加入群组网格，并与
// 同群组成员建立流式连接（直连优先、中继兜底，与内置 agent
// 的三段隧道策略一致）：
//
//	client, err := lanet.New(lanet.Config{
//		CTLURL:     "http://ctl.example.com:8600",
//		Name:       "my-service",
//		InviteCode: code, // 留空则创建新群组
//		GroupName:  "my-group",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close()
//
//	// 接收入向流（可选）。
//	client.OnStream(func(stream lanet.Stream) {
//		defer stream.Close()
//		io.Copy(stream, stream) // echo 示例
//	})
//
//	// 按虚拟 IP 主动开流（可选）。
//	stream, viaRelay, err := client.Dial(ctx, "100.64.x.x")
//
//	client.Run(ctx) // 阻塞：周期刷新 NetMap / 通告地址 / 补充中继预约
//
// 浏览器/网页接入不走本包：网页 SDK 基于信令 + WebRTC（阶段 2）。
package lanet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/peersource"
	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
)

// Stream 是隧道流的对外视图：io.ReadWriteCloser + 链路信息。
type Stream interface {
	io.ReadWriteCloser
	// CloseWrite 半关闭写端：发送完毕必须调用，
	// 对端才能 ReadAll 判 EOF（Windows 语义尤其如此）。
	CloseWrite() error
	// ViaRelay 本流是否经中继转发（false 即 P2P 直连）。
	ViaRelay() bool
	// Protocol 流上的协议 ID。
	Protocol() string
}

// streamAdapter 把 libp2p network.Stream 适配为 SDK 的 Stream。
type streamAdapter struct {
	network.Stream
	viaRelay bool
}

func (s streamAdapter) CloseWrite() error { return s.Stream.CloseWrite() }
func (s streamAdapter) ViaRelay() bool    { return s.viaRelay }
func (s streamAdapter) Protocol() string  { return string(s.Stream.Protocol()) }

// Handler 收到入向流时的回调。
type Handler func(stream Stream)

// Config SDK 配置。
type Config struct {
	// CTLURL 控制面地址（必填），如 http://127.0.0.1:8600。
	CTLURL string
	// Name 节点名称（必填）。
	Name string
	// OS 操作系统标识，留空取 runtime.GOOS。
	OS string
	// InviteCode 凭邀请码加入群组；留空则创建新群组。
	InviteCode string
	// GroupName 创建模式下的群组名称（Join 模式忽略）。
	GroupName string
	// ListenAddrs 覆盖默认监听地址（默认 tcp/udp 全部随机端口）。
	ListenAddrs []string
	// WebRTC 启用 webrtc-direct 传输层（默认开启），浏览器节点可直连本节点。
	WebRTC *bool
	// NetMapInterval 周期任务间隔，默认 15s。
	NetMapInterval time.Duration
	// DialTimeout 开流超时，默认 8s。
	DialTimeout time.Duration
	// Quiet 为 true 时不打日志。
	Quiet bool
}

// Client 一个已入网的 SDK 节点。
type Client struct {
	cfg        Config
	node       host.Host
	peerSource *peersource.Client
	netmapCli  *netmapclient.Client
	tunnelSvc  *tunnel.Service

	peerID  string
	groupID string
	group   string
	myIP    string
	created bool // 是否为本 SDK 创建的群组

	handlers []Handler
}

// Info 节点入网后的身份信息。
type Info struct {
	PeerID   string `json:"peer_id"`
	GroupID  string `json:"group_id"`
	Group    string `json:"group"`
	VirtualIP string `json:"virtual_ip"`
	// Created 仅创建模式为 true（同时携带 InviteCode）。
	Created bool   `json:"created,omitempty"`
	InviteCode string `json:"invite_code,omitempty"`
}

// New 创建节点并入网：InviteCode 为空则创建新群组（本节点成为群主），
// 否则凭邀请码加入。创建/加入失败即返回错误。
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.CTLURL == "" || cfg.Name == "" {
		return nil, fmt.Errorf("lanet: CTLURL 与 Name 为必填项")
	}
	if cfg.OS == "" {
		cfg.OS = defaultOS()
	}
	if cfg.NetMapInterval <= 0 {
		cfg.NetMapInterval = 15 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 8 * time.Second
	}

	c := &Client{cfg: cfg}

	// 1. libp2p Host：打洞 + 自动中继（候选来自控制面）。
	c.peerSource = peersource.NewClient(cfg.CTLURL)
	listenAddrs := cfg.ListenAddrs
	if len(listenAddrs) == 0 {
		// tcp + ws（浏览器可直连）+ quic；webrtc-direct 由 WebRTC 选项追加。
		listenAddrs = []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/tcp/0/ws",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		}
	}
	node, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		UserAgent:   "lanet-sdk-go/1.0.0",
		RelaySource: c.peerSource.AutoRelayPeerSource(),
		ListenAddrs: listenAddrs,
		WebRTC:      cfg.WebRTC == nil || *cfg.WebRTC,
	})
	if err != nil {
		return nil, fmt.Errorf("lanet: create host: %w", err)
	}
	c.node = node
	c.peerID = node.ID().String()
	c.logf("节点 PeerID=%s", c.peerID)

	// 2. 入网。
	c.netmapCli = netmapclient.NewClient(cfg.CTLURL, c.peerID)
	if cfg.InviteCode == "" {
		if err = c.createGroup(ctx); err != nil {
			_ = node.Close()
			return nil, err
		}
	} else {
		if err = c.joinGroup(ctx); err != nil {
			_ = node.Close()
			return nil, err
		}
	}

	// 3. 隧道服务。
	c.tunnelSvc = tunnel.New(node, c.netmapCli, c.peerSource)
	return c, nil
}

// Info 返回节点身份信息。
func (c *Client) Info() Info {
	return Info{
		PeerID: c.peerID, GroupID: c.groupID, Group: c.group,
		VirtualIP:  c.myIP,
		Created:    c.created,
		InviteCode: c.cfg.InviteCode,
	}
}

// OnStream 注册入向流处理器（协议 Tunnel）。可注册多个，按序调用。
func (c *Client) OnStream(handler Handler) {
	c.handlers = append(c.handlers, handler)
	if len(c.handlers) == 1 {
		c.node.SetStreamHandler(protocol.Tunnel, c.handleInbound)
	}
}

// Dial 按虚拟 IP 打开到对端的隧道流（直连优先，中继兜底）。
// 返回流与是否经中继。用完必须 Close；发送完毕建议 CloseWrite。
func (c *Client) Dial(ctx context.Context, virtualIP string) (Stream, bool, error) {
	raw, viaRelay, err := c.tunnelSvc.OpenStreamToVirtualIP(ctx, virtualIP)
	if err != nil {
		return nil, false, err
	}
	return streamAdapter{Stream: raw, viaRelay: viaRelay}, viaRelay, nil
}

// LastPathUsed 返回到对端最近一次链路类型：direct / relay / unknown。
func (c *Client) LastPathUsed(peerID string) string { return c.tunnelSvc.LastPathUsed(peerID) }

// NetMap 当前群组成员目录快照。
func (c *Client) NetMap() netmapclient.Snapshot { return c.netmapCli.Current() }

// Host 暴露底层 libp2p Host（进阶用法：自定义协议等）。
func (c *Client) Host() host.Host { return c.node }

// Run 阻塞运行周期任务（刷新 NetMap / 通告地址 / 补充中继预约），
// 直到 ctx 取消。典型用法：lanet client 入网后 go 或直接调用本方法，
// 业务逻辑在其他 goroutine 中运行。
func (c *Client) Run(ctx context.Context) {
	// 入网后先完成一次通告 + 中继预约（兜底链路关键步骤）。
	c.announce(ctx)
	if err := p2pkit.EnsureRelayReservation(ctx, c.node, c.peerSource.AutoRelayPeerSource(), 2); err != nil {
		c.logf("relay 预约暂未成功（将周期重试）: %v", err)
	}

	ticker := time.NewTicker(c.cfg.NetMapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logf("收到退出信号，正在关闭")
			return
		case <-ticker.C:
			if _, err := c.netmapCli.Refresh(ctx); err != nil {
				c.logf("刷新 NetMap: %v", err)
				continue
			}
			c.announce(ctx)
			// 预约有有效期，周期补充（已有时 Reserve 会刷新有效期）。
			if err := p2pkit.EnsureRelayReservation(ctx, c.node, c.peerSource.AutoRelayPeerSource(), 2); err != nil {
				c.logf("relay 预约补充失败（下个周期重试）: %v", err)
			}
		}
	}
}

// Close 关闭节点与底层连接。
func (c *Client) Close() error { return c.node.Close() }

// handleInbound 入向流分发到已注册的 Handler。
func (c *Client) handleInbound(stream network.Stream) {
	viaRelay := hasCircuit(stream.Conn().RemoteMultiaddr())
	for _, h := range c.handlers {
		h(streamAdapter{Stream: stream, viaRelay: viaRelay})
	}
}

// announce 通告本节点可达地址。
func (c *Client) announce(ctx context.Context) {
	addrs := make([]string, 0, len(c.node.Addrs()))
	for _, addr := range c.node.Addrs() {
		addrs = append(addrs, addr.String())
	}
	if err := c.netmapCli.Announce(ctx, addrs); err != nil {
		c.logf("通告地址失败（不影响运行，将周期重试）: %v", err)
	}
}

// createGroup 创建群组（本节点成为群主）。
func (c *Client) createGroup(ctx context.Context) error {
	var resp struct {
		Group struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"group"`
		Creator struct {
			VirtualIP string `json:"virtual_ip"`
		} `json:"creator"`
		InviteCode string `json:"invite_code"`
	}
	payload := map[string]any{
		"peer_id": c.peerID, "name": c.cfg.Name, "os": c.cfg.OS, "group_name": c.cfg.GroupName,
	}
	if err := c.post(ctx, "/v1/groups/create", payload, &resp); err != nil {
		return fmt.Errorf("lanet: 创建群组: %w", err)
	}
	c.groupID, c.group, c.myIP = resp.Group.ID, resp.Group.Name, resp.Creator.VirtualIP
	c.cfg.InviteCode = resp.InviteCode
	c.created = true
	c.logf("群组 %s 已创建，虚拟 IP=%s，邀请码=%s", c.group, c.myIP, resp.InviteCode)
	return nil
}

// joinGroup 凭邀请码加入群组。
func (c *Client) joinGroup(ctx context.Context) error {
	var resp struct {
		Group struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"group"`
		Member struct {
			VirtualIP string `json:"virtual_ip"`
		} `json:"member"`
	}
	payload := map[string]any{
		"invite_code": c.cfg.InviteCode, "peer_id": c.peerID, "name": c.cfg.Name, "os": c.cfg.OS,
	}
	if err := c.post(ctx, "/v1/groups/join", payload, &resp); err != nil {
		return fmt.Errorf("lanet: 加入群组: %w", err)
	}
	c.groupID, c.group, c.myIP = resp.Group.ID, resp.Group.Name, resp.Member.VirtualIP
	c.logf("已加入群组 %s，虚拟 IP=%s", c.group, c.myIP)
	return nil
}

// post 调用控制面 JSON 接口（gf 标准响应包装）。
func (c *Client) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.CTLURL, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Code != 0 {
		return fmt.Errorf("code %d: %s", envelope.Code, envelope.Message)
	}
	if len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

// hasCircuit 判断地址是否经过 /p2p-circuit（即中继链路）。
func hasCircuit(address interface{ ValueForProtocol(int) (string, error) }) bool {
	if address == nil {
		return false
	}
	_, err := address.ValueForProtocol(290) // P_CIRCUIT
	return err == nil
}

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Quiet {
		return
	}
	log.Printf("[lanet-sdk] "+format, args...)
}
