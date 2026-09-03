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
	var payload struct {
		Candidates []struct {
			PeerID string   `json:"peer_id"`
			Addrs  []string `json:"addrs"`
		} `json:"candidates"`
	}
	if err = json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]peer.AddrInfo, 0, len(payload.Candidates))
	for _, item := range payload.Candidates {
		id, err := peer.Decode(item.PeerID)
		if err != nil {
			continue
		}
		addrs := make([]string, 0, len(item.Addrs))
		out = append(out, peer.AddrInfo{ID: id})
		_ = addrs
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
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
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
