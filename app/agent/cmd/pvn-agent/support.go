package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// relayCandidates 直接实现控制面候选查询，返回 peer.AddrInfo。
type relayCandidates struct {
	httpClient *http.Client
	baseURL    string
}

func newRelayCandidates(baseURL string) *relayCandidates {
	return &relayCandidates{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
	}
}

// Candidates 实现 tunnel.RelaySource：返回可用中继候选。
func (r *relayCandidates) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/relays/candidates?limit=%d", r.baseURL, number), nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay candidates: status %d", resp.StatusCode)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Candidates []struct {
				PeerID string   `json:"peer_id"`
				Addrs  []string `json:"addrs"`
			} `json:"candidates"`
		} `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("relay candidates: code %d", envelope.Code)
	}
	payload := envelope.Data
	out := make([]peer.AddrInfo, 0, len(payload.Candidates))
	for _, item := range payload.Candidates {
		id, err := peer.Decode(item.PeerID)
		if err != nil {
			continue
		}
		addrs := make([]ma.Multiaddr, 0, len(item.Addrs))
		for _, raw := range item.Addrs {
			if addr, addrErr := ma.NewMultiaddr(raw); addrErr == nil {
				addrs = append(addrs, addr)
			}
		}
		if len(addrs) == 0 {
			// 没有可用地址的候选无法用于建连，跳过。
			continue
		}
		out = append(out, peer.AddrInfo{ID: id, Addrs: addrs})
	}
	return out, nil
}

func defaultOS() string {
	return runtime.GOOS
}

func postJSON(url string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("code %d: %s", envelope.Code, envelope.Message)
	}
	if envelope.Data == nil {
		envelope.Data = map[string]any{}
	}
	return envelope.Data, nil
}

// configureTUN 给 TUN 网卡配置虚拟 IP。Windows 走 netsh，Linux 走 ip 命令。
func configureTUN(name, ip string, prefixBits int) error {
	switch runtime.GOOS {
	case "windows":
		return run("netsh", "interface", "ip", "set", "address",
			"name="+name, "source=static", "addr="+ip, "mask=255.255.255.0")
	case "linux":
		if err := run("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, prefixBits), "dev", name); err != nil {
			return err
		}
		return run("ip", "link", "set", "dev", name, "up")
	case "darwin":
		return run("ifconfig", name, ip, ip, "up")
	default:
		return fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

func run(name string, args ...string) error {
	cmd := execCommand(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
