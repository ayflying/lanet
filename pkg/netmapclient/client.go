package netmapclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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
	// FirstSeen/LastSeen 仅 Standalone（本地发现）模式有值；
	// 控制面 NetMap 无此概念，为零值。
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
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
func (c *Client) Announce(ctx context.Context, addrs []string) error {
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
