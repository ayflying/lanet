package peersource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	ma "github.com/multiformats/go-multiaddr"
)

type Candidate struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

type candidateResponse struct {
	// gf MiddlewareHandlerResponse 标准包装：业务数据在 data 字段。
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    struct {
		Candidates []Candidate `json:"candidates"`
	} `json:"data"`
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *Client) PeerSource(ctx context.Context, number int) <-chan peer.AddrInfo {
	output := make(chan peer.AddrInfo)
	go func() {
		defer close(output)
		candidates, err := c.Candidates(ctx, number)
		if err != nil {
			return
		}
		for _, candidate := range candidates {
			select {
			case <-ctx.Done():
				return
			case output <- candidate:
			}
		}
	}()
	return output
}

func (c *Client) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	if number < 1 || number > 10 {
		return nil, fmt.Errorf("candidate number must be between 1 and 10")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/relays/candidates?limit=%d", c.baseURL, number), nil)
	if err != nil {
		return nil, fmt.Errorf("create relay candidate request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request relay candidates: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request relay candidates: unexpected status %s", response.Status)
	}

	var payload candidateResponse
	if err = json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode relay candidates: %w", err)
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("relay candidates: %s", payload.Message)
	}
	return toAddrInfos(payload.Data.Candidates)
}

func (c *Client) AutoRelayPeerSource() autorelay.PeerSource {
	return c.PeerSource
}

func toAddrInfos(candidates []Candidate) ([]peer.AddrInfo, error) {
	items := make([]peer.AddrInfo, 0, len(candidates))
	for _, candidate := range candidates {
		id, err := peer.Decode(candidate.PeerID)
		if err != nil {
			return nil, fmt.Errorf("decode relay peer id %q: %w", candidate.PeerID, err)
		}
		addresses := make([]ma.Multiaddr, 0, len(candidate.Addrs))
		for _, rawAddress := range candidate.Addrs {
			address, err := ma.NewMultiaddr(rawAddress)
			if err != nil {
				return nil, fmt.Errorf("decode relay address %q: %w", rawAddress, err)
			}
			addresses = append(addresses, address)
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("relay %s has no valid address", candidate.PeerID)
		}
		items = append(items, peer.AddrInfo{ID: id, Addrs: addresses})
	}
	return items, nil
}
