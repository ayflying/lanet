package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/multiformats/go-multiaddr"
)

func runRelay(ctx context.Context) {
	listenAddr := flag.String("listen", "/ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/tcp/4001/ws,/ip4/0.0.0.0/udp/4001/quic-v1", "comma-separated libp2p listen addresses")
	ctlURL := flag.String("ctl", os.Getenv("PVN_RELAY_CTL"), "控制面地址（如 http://ctl:8000），用于自注册与心跳；留空跳过注册")
	region := flag.String("region", envOr("PVN_RELAY_REGION", "default"), "地域标识")
	flag.Parse()

	host, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs:           splitAddresses(*listenAddr),
		RelayService:          true,
		RelayServiceDedicated: true,
		UserAgent:             "pvn-relay/0.1.0",
	})
	if err != nil {
		log.Fatalf("start relay: %v", err)
	}
	defer host.Close()

	log.Printf("PVN relay started: peer_id=%s", host.ID())
	for _, addr := range host.Addrs() {
		log.Printf("listen=%s/p2p/%s", addr, host.ID())
	}

	// 自注册 + 周期心跳：中继目录里的候选有心跳有效期，过期会被清掉，
	// agent 拿不到候选就无法走中继兜底。未配置 -ctl 时跳过（纯手动模式）。
	if strings.TrimSpace(*ctlURL) != "" {
		go relayRegisterLoop(ctx, host, *ctlURL, *region)
	} else {
		log.Print("no -ctl configured, skipping self-registration")
	}

	<-ctx.Done()
	log.Print("PVN relay stopped")
}

// relayRegisterLoop 首次注册带重试（ctl 可能比 relay 后就绪），
// 成功后每 30s 心跳保活；心跳连续失败则回到注册态重试。
func relayRegisterLoop(ctx context.Context, h host.Host, baseURL, region string) {
	base := strings.TrimRight(baseURL, "/")
	registered := false
	for attempt := 0; attempt < 30; attempt++ {
		if err := relayRegister(ctx, h, base, region); err == nil {
			registered = true
			log.Printf("registered to control plane %s", base)
			break
		} else {
			log.Printf("register to %s failed (retry): %v", base, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	if !registered {
		log.Print("registration gave up; relay keeps serving without directory listing")
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !registered {
				if err := relayRegister(ctx, h, base, region); err == nil {
					registered = true
					log.Printf("registered to control plane %s", base)
				}
				continue
			}
			if err := relayHeartbeat(ctx, h, base); err != nil {
				log.Printf("heartbeat failed: %v", err)
				registered = false // 失联后重新走注册。
			}
		}
	}
}

// relayPublicAddrs 返回注册用的可达地址：
// 优先 PVN_RELAY_ADVERTISE（逗号分隔 multiaddr，容器 NAT 环境建议显式配置），
// 否则过滤回环与链路本地地址；过滤后为空则回退全部地址。
// 每条地址追加 /p2p/<peer_id>，方便 agent 直接构造 AddrInfo。
func relayPublicAddrs(h host.Host) []string {
	if advertised := strings.TrimSpace(os.Getenv("PVN_RELAY_ADVERTISE")); advertised != "" {
		return strings.Split(advertised, ",")
	}
	out := make([]string, 0, len(h.Addrs()))
	for _, addr := range h.Addrs() {
		if !isUsableHostAddr(addr) {
			continue
		}
		out = append(out, addr.String()+"/p2p/"+h.ID().String())
	}
	if len(out) == 0 {
		for _, addr := range h.Addrs() {
			out = append(out, addr.String()+"/p2p/"+h.ID().String())
		}
	}
	return out
}

// isUsableHostAddr 过滤回环与链路本地地址（不适合对外通告）。
func isUsableHostAddr(addr multiaddr.Multiaddr) bool {
	s := addr.String()
	if strings.HasPrefix(s, "/ip4/127.") ||
		strings.HasPrefix(s, "/ip4/169.254.") ||
		strings.HasPrefix(s, "/ip6/::1") {
		return false
	}
	return true
}

func relayRegister(ctx context.Context, h host.Host, base, region string) error {
	payload := map[string]any{
		"peer_id": h.ID().String(),
		"addrs":   relayPublicAddrs(h),
		"region":  region,
		"score":   100,
	}
	return relayPostJSON(ctx, base+"/v1/relays/register", payload)
}

func relayHeartbeat(ctx context.Context, h host.Host, base string) error {
	payload := map[string]any{
		"peer_id":       h.ID().String(),
		"circuit_count": 0,
		"score":         100,
	}
	return relayPostJSON(ctx, base+"/v1/relays/heartbeat", payload)
}

func relayPostJSON(ctx context.Context, url string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitAddresses(value string) []string {
	var addresses []string
	var start int
	for index, char := range value {
		if char == ',' {
			if index > start {
				addresses = append(addresses, value[start:index])
			}
			start = index + 1
		}
	}
	if start < len(value) {
		addresses = append(addresses, value[start:])
	}
	return addresses
}
