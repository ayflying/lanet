package netmapclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/ayflying/pvn/pkg/p2pkit"
)

// 群组 NetMap 客户端：
// - Agent 定期从控制面拉取所在群组的成员目录。
// - 目录仅包含同群成员（虚拟 IP + PeerID + 可达地址）。
// - 本地维护"虚拟 IP → PeerID + 地址"路由表，供隧道直连或经中继转发。

type Member struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	VirtualIP string   `json:"virtual_ip"`
	Addrs     []string `json:"addrs"`
	// Hostname 虚拟主机名（含 .lanet 后缀，如 yunloli.lanet）。
	// 仅 Standalone（本地发现）模式由成员表推导填充；控制面模式暂无。
	Hostname string `json:"hostname,omitempty"`
	// FirstSeen/LastSeen 仅 Standalone（本地发现）模式有值；
	// 控制面 NetMap 无此概念，为零值。
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	// Version/Platform 成员程序版本与平台（info 协议交换；旧节点为空）。
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
	// OSHostname 成员操作系统主机名（info 协议交换；旧节点为空）。
	OSHostname string `json:"os_hostname,omitempty"`
	// LocalIPs 成员本机非回环网卡 IP（info 协议交换；旧节点为空）。
	LocalIPs []string `json:"local_ips,omitempty"`
}

type Snapshot struct {
	GroupID   string    `json:"group_id"`
	GroupName string    `json:"group_name"`
	CIDR      string    `json:"cidr"`
	Version   uint64    `json:"version"`
	Members   []Member  `json:"members"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Route 虚拟 IP 到对端的路由条目。
type Route struct {
	VirtualIP string
	PeerID    string
	Addrs     []string
}

type Client struct {
	baseURL    string
	peerID     string
	httpClient *http.Client

	mu       sync.RWMutex
	snapshot Snapshot
}

func NewClient(baseURL, peerID string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		peerID:     peerID,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// apiEnvelope gf MiddlewareHandlerResponse 的标准响应包装。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Refresh 拉取一次 NetMap 并更新本地路由表。
func (c *Client) Refresh(ctx context.Context) (Snapshot, error) {
	if c.peerID == "" {
		return Snapshot{}, fmt.Errorf("peer_id is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/groups/netmap?peer_id=%s", c.baseURL, c.peerID), nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("create netmap request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Snapshot{}, fmt.Errorf("request netmap: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("request netmap: unexpected status %s", response.Status)
	}
	var envelope apiEnvelope
	if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return Snapshot{}, fmt.Errorf("decode netmap: %w", err)
	}
	if envelope.Code != 0 {
		return Snapshot{}, fmt.Errorf("netmap: %s", envelope.Message)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(envelope.Data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode netmap data: %w", err)
	}
	snapshot.FetchedAt = time.Now()

	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
	return snapshot, nil
}

// Current 返回最近一次成功的 NetMap 快照。
func (c *Client) Current() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot
}

// Routes 把当前快照转换为路由表，按虚拟 IP 排序，保证输出稳定。
func (c *Client) Routes() []Route {
	snapshot := c.Current()
	routes := make([]Route, 0, len(snapshot.Members))
	for _, member := range snapshot.Members {
		routes = append(routes, Route{
			VirtualIP: member.VirtualIP,
			PeerID:    member.PeerID,
			Addrs:     append([]string(nil), member.Addrs...),
		})
	}
	for i := 1; i < len(routes); i++ {
		for j := i; j > 0 && routes[j].VirtualIP < routes[j-1].VirtualIP; j-- {
			routes[j], routes[j-1] = routes[j-1], routes[j]
		}
	}
	return routes
}

// Resolve 根据目标虚拟 IP 查找对端 PeerID 与地址。
func (c *Client) Resolve(virtualIP string) (Route, bool) {
	for _, route := range c.Routes() {
		if route.VirtualIP == virtualIP {
			return route, true
		}
	}
	return Route{}, false
}

// Announce 向控制面通告本节点可达地址（multiaddr）。
// Announce 把自己的可达地址通告给控制面，供同群成员按需直拨。
//
// 通告前统一清洗（见 FilterAnnounceAddrs）：历史实现是把 libp2p 的原始监听地址
// 整份发上去，于是回环（127.0.0.1）、未指定（0.0.0.0）、lanet overlay（10.7/16）
// 以及容器内网地址全被写进 netmap；同群节点拿到这些地址去拨，轻则必然超时
// （对端拨回环＝拨它自己），重则形成拨号风暴。真机实证：VPS 容器通告了
// 192.168.64.2:4001，其它成员冷启动预热反复拨它，每轮空耗 30~45 秒。
func (c *Client) Announce(ctx context.Context, addrs []string) error {
	addrs = FilterAnnounceAddrs(addrs)
	payload := map[string]any{"peer_id": c.peerID, "addrs": addrs}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode announce payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/groups/announce", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create announce request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("announce addresses: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("announce addresses: unexpected status %s", response.Status)
	}
	var envelope apiEnvelope
	if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode announce response: %w", err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("announce addresses: %s", envelope.Message)
	}
	return nil
}

// FilterAnnounceAddrs 清洗要对外通告的地址列表：
//   - 剔除回环（127/8、::1）、未指定（0.0.0.0、::）、lanet overlay（10.7/16）——
//     对端拨这些地址只会打到它自己的机器；
//   - 本机在容器里时剔除容器内网地址（外部永远不可达，见 p2pkit.InContainer）；
//   - 去重、按可达性排序、折叠同一链路的冗余（Windows 隐私 IPv6 会刷出七八条）；
//   - 把 LANET_ADVERTISE 显式声明的对外地址顶到最前。
//
// 无法解析为 multiaddr 的字符串**原样保留**：一条畸形成员地址不该让整份通告
// 消失。清洗后为空就通告空列表——这比通告一堆必然失败的地址更有价值。
func FilterAnnounceAddrs(addrs []string) []string {
	if len(addrs) == 0 {
		return addrs
	}
	parsed := make([]ma.Multiaddr, 0, len(addrs))
	var unparsed []string
	for _, s := range addrs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			unparsed = append(unparsed, s)
			continue
		}
		parsed = append(parsed, a)
	}
	out := make([]string, 0, len(parsed)+len(unparsed))
	for _, a := range p2pkit.ShareableAddrs(parsed) {
		out = append(out, a.String())
	}
	return append(out, unparsed...)
}

// RunLoop 周期性刷新 NetMap，直到 ctx 取消。
func (c *Client) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = c.Refresh(ctx)
		}
	}
}
