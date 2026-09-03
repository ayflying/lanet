package node

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/gogf/gf/v2/errors/gerror"
)

type EnrollRequest struct {
	Token  string `json:"token"`
	PeerID string `json:"peer_id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
}

type Node struct {
	PeerID     string    `json:"peer_id"`
	Name       string    `json:"name"`
	OS         string    `json:"os"`
	VirtualIP  string    `json:"virtual_ip"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

type Registry struct {
	mu      sync.RWMutex
	prefix  netip.Prefix
	tokens  map[string]struct{}
	nodes   map[string]Node
	usedIPs map[netip.Addr]string
}

func NewRegistry(cidr string, tokens []string) (*Registry, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse PVN CIDR: %w", err)
	}
	if !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return nil, gerror.New("MVP currently requires an IPv4 /24 PVN CIDR")
	}
	items := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token != "" {
			items[token] = struct{}{}
		}
	}
	return &Registry{
		prefix:  prefix.Masked(),
		tokens:  items,
		nodes:   make(map[string]Node),
		usedIPs: make(map[netip.Addr]string),
	}, nil
}

func (r *Registry) Enroll(ctx context.Context, input EnrollRequest) (Node, error) {
	if input.Token == "" || input.PeerID == "" || input.Name == "" {
		return Node{}, gerror.New("token, peer_id and name are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[input.Token]; !ok {
		return Node{}, gerror.New("invalid enroll token")
	}
	if node, ok := r.nodes[input.PeerID]; ok {
		return node, nil
	}
	address, err := r.nextIP()
	if err != nil {
		return Node{}, err
	}
	node := Node{PeerID: input.PeerID, Name: input.Name, OS: input.OS, VirtualIP: address.String(), EnrolledAt: time.Now()}
	r.nodes[node.PeerID] = node
	r.usedIPs[address] = node.PeerID
	return node, nil
}

func (r *Registry) List(ctx context.Context) []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]Node, 0, len(r.nodes))
	for _, item := range r.nodes {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].VirtualIP < items[j].VirtualIP })
	return items
}

func (r *Registry) CIDR() string {
	return r.prefix.String()
}

func (r *Registry) nextIP() (netip.Addr, error) {
	base := r.prefix.Addr().As4()
	for host := 2; host < 255; host++ {
		candidate := netip.AddrFrom4([4]byte{base[0], base[1], base[2], byte(host)})
		if r.prefix.Contains(candidate) {
			if _, used := r.usedIPs[candidate]; !used {
				return candidate, nil
			}
		}
	}
	return netip.Addr{}, gerror.New("PVN address pool is exhausted")
}
