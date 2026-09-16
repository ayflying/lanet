// Package mobile 是 lanet 节点的移动端门面（gomobile bind 目标）。
//
// 它只做三件事：
//  1. 把 Android/iOS 宿主传进来的参数（JSON）翻译成 sdk/go/lanet 的 Config；
//  2. 接管宿主 VpnService 建立的虚拟网卡 fd，把手机变成真正的网络成员
//     （可 ping 虚拟 IP、访问任意 TCP/UDP，而不只是经网关转发应用流）；
//  3. 把节点状态以 JSON 字符串暴露给 Java/Kotlin 侧（gomobile 的结构体绑定
//     限制较多，统一用 JSON 传递复杂数据最省心）。
//
// 业务逻辑一律不在本包实现——它是薄封装，所有能力都来自 sdk/go/lanet。
//
// 编译（产出 AAR，供 Android 工程或 uni-app 原生插件引用）：
//
//	cd sdk/go/mobile
//	export ANDROID_HOME=<sdk>  ANDROID_NDK_HOME=<ndk>  JAVA_HOME=<jdk>
//	export PATH="$JAVA_HOME/bin:$PATH"   # gomobile 最后一步要调 javac
//	gomobile bind -target=android/arm64,android/amd64 -androidapi 21 \
//	  -javapkg=com.lanet -ldflags="-checklinkname=0 -s -w" -o lanet.aar .
//
// 环境前提：ANDROID_HOME（含 platforms/android-XX/android.jar）、
// ANDROID_NDK_HOME（r23+）、JAVA_HOME。四个实测踩过的点：
//
//  1. -androidapi 默认 16，现代 NDK 的 meta/platforms.json 下限是 21，
//     不显式指定会报 "unsupported API version 16"；
//  2. -javapkg 是「前缀 + Go 包名」，传 com.lanet 得到的 Java 包名才是
//     com.lanet.mobile（对应 Kotlin 侧 import com.lanet.mobile.Node），
//     直接传 com.lanet.mobile 会得到 com.lanet.mobile.mobile；
//  3. Go 1.23+ 的 linkname 校验会拦下 libp2p 依赖的 github.com/wlynxg/anet
//     （报 invalid reference to net.zoneCache），必须加 -checklinkname=0；
//  4. -s -w 顺手瘦身：单架构 AAR 22MB → 13MB，双架构 26MB。
//
// target 里的 x86_64 是 MuMu 等模拟器需要的；只出 arm64 时模拟器装不上。
//
// 产物落在本目录，需拷到 sdk/android/app/libs/lanet.aar 供宿主工程引用
// （该文件已在 .gitignore 中忽略，构建前必须先编译一次）。
package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

// mobileVersion 本门面自身的版本，随 Status 一起暴露，便于排查"客户端与
// AAR 版本不匹配"这类问题。
const mobileVersion = "0.1.0"

// options 启动参数（JSON）。字段刻意保持扁平与稳定，作为 Java/Kotlin 与
// Go 之间的契约；后续新增字段一律加可选字段，不改动既有语义。
type options struct {
	// DataDir 应用私有目录（必填）。Android 传 getFilesDir().getAbsolutePath()。
	// 身份密钥、地址簿、防火墙状态都落在这里；换目录等于换一台新设备。
	DataDir string `json:"data_dir"`
	// Name 本机在成员表里显示的名字（默认 "android"）。
	Name string `json:"name"`
	// NetworkKey 网络密钥：与目标网络一致才能互相发现（官方节点读的就是这个）。
	NetworkKey string `json:"network_key"`
	// Bootstrap 引导种子 multiaddr 列表（填任意已在网成员的地址即可入网）。
	Bootstrap []string `json:"bootstrap"`
	// TunFD 宿主 VpnService 建立虚拟网卡后交出的文件描述符（>0 生效）。
	// 置位后手机成为真正的网络成员：可 ping 虚拟 IP、访问任意 TCP/UDP。
	// 为 0 时退化为纯应用层（仍可 Dial/PortFWD，但不能 ping）。
	TunFD int `json:"tun_fd"`
	// AutoAccept 自动同意陌生节点的连接申请（默认 false，走人工审批）。
	AutoAccept bool `json:"auto_accept"`
	// RequireApproval 是否要求连接审批；nil = 用 SDK 默认（要求）。
	RequireApproval *bool `json:"require_approval,omitempty"`
	// EnablePublicDHT 启用公共 DHT 兜底（默认关闭，省流量）。
	EnablePublicDHT bool `json:"enable_public_dht"`
	// FirewallMode 入向防火墙初始模式：deny-all / allow-list / allow-all。
	// 留空 = allow-all（与官方发行版 pvn-node 的默认一致）。
	//
	// 为什么必须让默认值等于 pvn-node：SDK 自己的默认是 deny-all，桌面节点是
	// 因为 pvn-node 显式给了 allow-all 才「能被人 ping 通」。移动端若沿用 SDK
	// 默认，会出现「手机 ping 得通所有人、所有人都 ping 不通手机」——现象上看
	// 像链路故障，实际是入向包全被防火墙丢了（日志形态见
	// [firewall] TUN 入向包被拒绝：协议=ip:1）。真正的安全边界是连接审批，
	// 陌生节点不点头根本进不了这张网；防火墙是用来做更细粒度控制的。
	FirewallMode string `json:"firewall_mode"`
	// Version 本客户端版本，随 info 协议上报给同网络成员。
	Version string `json:"version"`
}

// Node 一个移动端节点句柄。必须先 Start 才能使用其余方法。
type Node struct {
	mu     sync.Mutex
	client *lanet.Client
	cancel context.CancelFunc
	cfg    options
}

// NewNode 创建节点句柄（此时尚未入网）。
//
// 命名约束：gomobile 的 Java 生成器只把「`New<类型名>` 前缀」的函数映射成
// Java 构造函数（见 x/mobile/bind/genjava.go 的前缀判断），所以这里必须是
// NewNode——写成 `New` 既不符合该规则，还会撞上 Java 关键字。生成后 Kotlin
// 侧写作 `Node()`。
func NewNode() *Node { return &Node{} }

// Start 启动节点并入网。重复调用返回错误（先 Stop）。
//
// 关于渠道（Channel）：这里刻意**不设置**，让它落到 SDK 默认值。原因是
// 官方发行版 pvn-node 构造的同样是 lanet.Config（见 pvn-node/main.go 的
// cfg := lanet.Config{...}），其空 Channel 会被 sdk/go/lanet 归一化成同一
// 取值——也就是说官方桌面节点与移动端天然处于同一渠道、同一张网。实测
// （0.5.47）以该配置入网的探针与桌面节点得到完全相同的网络组指纹
// standalone-1bb6614b，并被 yunloli-server / fnos / sonow-ai 三台成员
// 主动发现。此处若显式改渠道，等于把手机隔离到另一张网。
func (n *Node) Start(configJSON string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client != nil {
		return fmt.Errorf("节点已在运行，请先 Stop")
	}

	var o options
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &o); err != nil {
			return fmt.Errorf("解析配置失败: %w", err)
		}
	}
	if o.DataDir == "" {
		return fmt.Errorf("data_dir 必填（Android 传 getFilesDir().getAbsolutePath()）")
	}
	if o.Name == "" {
		o.Name = "android"
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	cfg := lanet.Config{
		Name:            o.Name,
		NetworkKey:      o.NetworkKey,
		Standalone:      true,
		Bootstrap:       o.Bootstrap,
		EnablePublicDHT: o.EnablePublicDHT,
		IdentityFile:    filepath.Join(o.DataDir, "node.key"),
		DBPath:          filepath.Join(o.DataDir, "lanet.db"),
		StateFile:       filepath.Join(o.DataDir, "state.json"),
		// 移动端不启内置 Web 控制台：手机上没有浏览器，8900 端口也无意义，
		// 而且会和同机的桌面实例抢端口。
		ConsoleAddr:     "-",
		AutoAccept:      o.AutoAccept,
		RequireApproval: o.RequireApproval,
		Version:         o.Version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
	}
	// TUN：Android 上非 root 进程无法自行打开 /dev/net/tun，只能由宿主
	// VpnService 建卡后把 fd 交进来（wireguard-android 同款做法）。
	if o.TunFD > 0 {
		cfg.Tun = true
		cfg.TunFD = o.TunFD
	}
	// 防火墙：留空对齐 pvn-node 的默认（allow-all），理由见 options.FirewallMode。
	cfg.FirewallMode = lanet.FirewallModeAllowAll
	if o.FirewallMode != "" {
		cfg.FirewallMode = lanet.FirewallMode(o.FirewallMode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	client, err := lanet.New(ctx, cfg)
	if err != nil {
		cancel()
		return fmt.Errorf("入网失败: %w", err)
	}
	go client.Run(ctx)

	n.client = client
	n.cancel = cancel
	n.cfg = o
	return nil
}

// Stop 关闭节点（幂等）。
func (n *Node) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client == nil {
		return nil
	}
	if n.cancel != nil {
		n.cancel()
	}
	err := n.client.Close()
	n.client = nil
	n.cancel = nil
	return err
}

// IsRunning 节点是否已入网。
func (n *Node) IsRunning() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.client != nil
}

// MobileVersion 本门面版本。
func (n *Node) MobileVersion() string { return mobileVersion }

// Status 节点状态（JSON）：
//
//	{"running":true,"peer_id":"…","virtual_ip":"10.7.x.x","virtual_host":"xx.lanet",
//	 "group":"standalone-…","name":"…","member_count":3,"pending_count":0,
//	 "trusted_count":2,"data_dir":"/data/user/0/…/files"}
//
// 未运行时返回 {"running":false}。
func (n *Node) Status() string {
	c := n.current()
	if c == nil {
		return `{"running":false}`
	}
	info := c.Info()
	// 防火墙模式要出现在状态里：移动端很容易踩到「能 ping 出去、别人 ping
	// 不回来」，而根因就是入向被 deny-all 丢掉，不暴露就只能靠翻 logcat。
	fwMode, _ := c.Firewall()
	st := map[string]any{
		"running":       true,
		"peer_id":       info.PeerID,
		"virtual_ip":    info.VirtualIP,
		"virtual_host":  info.VirtualHost,
		"group":         info.Group,
		"name":          info.Name,
		"member_count":  len(c.NetMap().Members),
		"pending_count": len(c.PendingList()),
		"trusted_count": len(c.TrustedPeers()),
		"data_dir":      n.dataDir(),
		"tun_fd":        n.tunFD(),
		"mobile":        mobileVersion,
		"firewall":      string(fwMode),
	}
	return toJSON(st)
}

// Members 当前成员表（JSON 数组）。成员要出现，前提是**双方都已互相审批**。
func (n *Node) Members() string {
	c := n.current()
	if c == nil {
		return "[]"
	}
	snap := c.NetMap()
	out := make([]map[string]any, 0, len(snap.Members))
	for _, m := range snap.Members {
		out = append(out, map[string]any{
			"peer_id":    m.PeerID,
			"name":       m.Name,
			"virtual_ip": m.VirtualIP,
			"hostname":   m.Hostname,
			"platform":   m.Platform,
			"version":    m.Version,
			"path":       c.LastPathUsed(m.PeerID),
		})
	}
	return toJSON(out)
}

// Pending 待审批申请（JSON 数组）。运行在有陌生节点申请连接时，UI 应提示用户。
func (n *Node) Pending() string {
	c := n.current()
	if c == nil {
		return "[]"
	}
	list := c.PendingList()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{
			"peer_id":      p.PeerID,
			"name":         p.Name,
			"addrs":        p.Addrs,
			"reason":       p.Reason,
			"requested_at": p.RequestedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return toJSON(out)
}

// Peers 已信任节点（地址簿，JSON 数组）。
func (n *Node) Peers() string {
	c := n.current()
	if c == nil {
		return "[]"
	}
	list := c.TrustedPeers()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{
			"peer_id": p.PeerID,
			"name":    p.Name,
			"last_ip": p.LastIP,
		})
	}
	return toJSON(out)
}

// Nearby 附近节点（发现到但未经互信的节点，JSON 数组）。可用于 UI 的
// "发现新设备"列表——用户从这里发起添加。
func (n *Node) Nearby() string {
	c := n.current()
	if c == nil {
		return "[]"
	}
	list := c.NearbyList()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{
			"peer_id":    p.PeerID,
			"name":       p.Name,
			"addrs":      p.Addrs,
			"source":     p.Source,
			"first_seen": p.FirstSeen.Format("2006-01-02 15:04:05"),
			"last_seen":  p.LastSeen.Format("2006-01-02 15:04:05"),
		})
	}
	return toJSON(out)
}

// Approve 同意（approve=true）或拒绝某节点的连接申请。
//
// 语义提醒（实测确认，决定了 APP 交互设计）：对方主动拨入本机**不会**在
// 本机产生待审批记录；本机主动填对方地址才会"先信任对方"。因此手机要入网
// 通常需要用户显式执行一次 Connect（填桌面节点的连接码），而不是被动等待。
func (n *Node) Approve(peerID string, approve bool) error {
	c := n.current()
	if c == nil {
		return errNotRunning
	}
	if peerID == "" {
		return fmt.Errorf("peer_id 不能为空")
	}
	if approve {
		return c.ApprovePeer(peerID)
	}
	return c.RejectPeer(peerID)
}

// Remove 删除已信任节点（撤信任 + 移出地址簿）。
func (n *Node) Remove(peerID string) error {
	c := n.current()
	if c == nil {
		return errNotRunning
	}
	return c.RemovePeer(peerID)
}

// Connect 主动连接一个节点，address 支持三种写法（SDK 自动识别）：
//
//	裸 PeerID           12D3KooW…
//	连接码              lanet://<ID>@ip:port,ip2:port2
//	multiaddr           /ip4/1.2.3.4/tcp/4001/p2p/12D3KooW…
//
// 返回 JSON：{"peer_id":"…","pending":false,"searching":false,"message":"…",
// "name":"…","virtual_ip":"10.7.x.x","via":"local"}
//
// pending=true 表示已提交申请、等对端同意；searching=true 表示已记录但
// 暂未在 DHT 里找到对方（不是失败，周期发现会自动完成连接）。
func (n *Node) Connect(address string) (string, error) {
	c := n.current()
	if c == nil {
		return "", errNotRunning
	}
	res, err := c.ConnectPeer(context.Background(), address)
	if err != nil {
		return "", err
	}
	return toJSON(res), nil
}

// ConnectSeed 按连接种子（multiaddr）直连，等价于控制台的"连接种子"。
func (n *Node) ConnectSeed(seed string) (string, error) {
	c := n.current()
	if c == nil {
		return "", errNotRunning
	}
	peerID, err := c.ConnectSeed(seed)
	if err != nil {
		return "", err
	}
	return peerID, nil
}

// InviteCode 本机的连接码（lanet://<ID>@ip:port,…）。把它给别人，对方即可
// 通过 Connect 连上本机——这是"把手机加入网络"最短的一条路径。
func (n *Node) InviteCode() string {
	c := n.current()
	if c == nil {
		return ""
	}
	return c.InviteCode()
}

// SeedAddrs 本机可作为引导种子使用的 multiaddr 列表（JSON 数组）。
func (n *Node) SeedAddrs() string {
	c := n.current()
	if c == nil {
		return "[]"
	}
	return toJSON(c.SeedAddrs())
}

// ---- 内部工具 ----

var errNotRunning = fmt.Errorf("节点未运行，请先 Start")

func (n *Node) current() *lanet.Client {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.client
}

func (n *Node) dataDir() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.DataDir
}

func (n *Node) tunFD() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.TunFD
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"序列化失败"}`
	}
	return string(b)
}
