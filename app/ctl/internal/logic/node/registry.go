package node

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
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
	// VirtualIPv6 群组 IPv6 /64 内的成员地址（见 pkg/protocol.MemberIPv6），
	// 主机号与 VirtualIP 相同；老数据可能为空，恢复时按规则补齐。
	VirtualIPv6 string `json:"virtual_ipv6,omitempty"`
}

type Registry struct {
	mu       sync.RWMutex
	prefix   netip.Prefix
	prefix6  netip.Prefix
	tokens   map[string]struct{}
	nodes    map[string]Node
	usedIPs  map[netip.Addr]string
	usedIPv6 map[netip.Addr]string
}

// NewRegistry 创建群组内节点注册表：IPv4 /24 与 IPv6 /64 成对使用
// （ipv6Prefix 由调用方按 pkg/protocol.GroupIPv6Prefix 算出，保证控制面与
// 节点侧 TUN 用同一套地址方案）。IPv4 仍要求 /24：地址池与主机号规则不变。
func NewRegistry(cidr string, ipv6Prefix string, tokens []string) (*Registry, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse PVN CIDR: %w", err)
	}
	if !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return nil, gerror.New("MVP currently requires an IPv4 /24 PVN CIDR")
	}
	prefix6, err := netip.ParsePrefix(ipv6Prefix)
	if err != nil {
		return nil, fmt.Errorf("parse PVN IPv6 CIDR: %w", err)
	}
	if !prefix6.Addr().Is6() || prefix6.Bits() != 64 {
		return nil, gerror.New("PVN IPv6 CIDR must be a /64")
	}
	items := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token != "" {
			items[token] = struct{}{}
		}
	}
	return &Registry{
		prefix:   prefix.Masked(),
		prefix6:  prefix6.Masked(),
		tokens:   items,
		nodes:    make(map[string]Node),
		usedIPs:  make(map[netip.Addr]string),
		usedIPv6: make(map[netip.Addr]string),
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
	host, err := r.nextHost()
	if err != nil {
		return Node{}, err
	}
	address, err := r.addrForHost(host)
	if err != nil {
		return Node{}, err
	}
	address6, err := r.addr6ForHost(host)
	if err != nil {
		return Node{}, err
	}
	node := Node{
		PeerID: input.PeerID, Name: input.Name, OS: input.OS,
		VirtualIP: address.String(), VirtualIPv6: address6.String(), EnrolledAt: time.Now(),
	}
	r.nodes[node.PeerID] = node
	r.usedIPs[address] = node.PeerID
	r.usedIPv6[address6] = node.PeerID
	return node, nil
}

// RestoreNode 将已持久化的节点按原虚拟地址恢复进注册表（用于服务重启后的数据回放）。
// 若 PeerID 已存在则视为重复恢复，直接忽略。
//
// VirtualIPv6 为空时按 IPv4 主机号补齐（老库只有 virtual_ip 一列），
// 这样升级后老成员也立刻有 IPv6，不需要重新入网。
func (r *Registry) RestoreNode(input Node) error {
	peerID := input.PeerID
	if peerID == "" || input.VirtualIP == "" {
		return gerror.New("peer_id and virtual_ip are required for restore")
	}
	address, err := netip.ParseAddr(input.VirtualIP)
	if err != nil {
		return fmt.Errorf("parse virtual IP %s: %w", input.VirtualIP, err)
	}
	if !r.prefix.Contains(address) {
		return gerror.New("virtual IP is outside of registry CIDR")
	}
	address6 := netip.Addr{}
	if input.VirtualIPv6 != "" {
		address6, err = netip.ParseAddr(input.VirtualIPv6)
		if err != nil {
			return fmt.Errorf("parse virtual IPv6 %s: %w", input.VirtualIPv6, err)
		}
		if !r.prefix6.Contains(address6) {
			return gerror.New("virtual IPv6 is outside of registry IPv6 CIDR")
		}
	} else {
		raw := address.As4()
		if address6, err = r.addr6ForHost(int(raw[3])); err != nil {
			return err
		}
	}
	if _, ok := r.nodes[peerID]; ok {
		return nil
	}
	if existing, used := r.usedIPs[address]; used && existing != peerID {
		return gerror.New("virtual IP already assigned to another peer")
	}
	if existing, used := r.usedIPv6[address6]; used && existing != peerID {
		return gerror.New("virtual IPv6 already assigned to another peer")
	}
	node := Node{
		PeerID: peerID, Name: input.Name, OS: input.OS,
		VirtualIP: input.VirtualIP, VirtualIPv6: address6.String(), EnrolledAt: time.Now(),
	}
	r.nodes[node.PeerID] = node
	r.usedIPs[address] = node.PeerID
	r.usedIPv6[address6] = node.PeerID
	return nil
}

// RemoveNode 将成员移出注册表并回收其虚拟 IP（供群主踢人使用）。
// 被回收的 IPv4/IPv6 都会重新进入可用池；若 PeerID 不存在则报错。
func (r *Registry) RemoveNode(peerID string) (Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[peerID]
	if !ok {
		return Node{}, gerror.New("peer is not a member of this group")
	}
	delete(r.nodes, peerID)
	if address, err := netip.ParseAddr(node.VirtualIP); err == nil {
		delete(r.usedIPs, address)
	}
	if node.VirtualIPv6 != "" {
		if address6, err := netip.ParseAddr(node.VirtualIPv6); err == nil {
			delete(r.usedIPv6, address6)
		}
	}
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

// CIDR6 本群组的虚拟 IPv6 /64。
func (r *Registry) CIDR6() string {
	return r.prefix6.String()
}

// nextHost 返回第一个在 IPv4 与 IPv6 两个池里都空闲的主机号（2~254）。
// 两个地址由同一主机号派生（见 pkg/protocol.MemberIPv6），因此必须成对空闲，
// 避免出现「IPv4 已分配、IPv6 撞车」这种半分配状态。
func (r *Registry) nextHost() (int, error) {
	for host := 2; host < 255; host++ {
		address, err := r.addrForHost(host)
		if err != nil {
			return 0, err
		}
		if _, used := r.usedIPs[address]; used {
			continue
		}
		address6, err := r.addr6ForHost(host)
		if err != nil {
			return 0, err
		}
		if _, used := r.usedIPv6[address6]; used {
			continue
		}
		return host, nil
	}
	return 0, gerror.New("PVN address pool is exhausted")
}

// addrForHost 由主机号得到本群组的 IPv4 地址。
func (r *Registry) addrForHost(host int) (netip.Addr, error) {
	base := r.prefix.Addr().As4()
	candidate := netip.AddrFrom4([4]byte{base[0], base[1], base[2], byte(host)})
	if !r.prefix.Contains(candidate) {
		return netip.Addr{}, gerror.New("host is outside of registry CIDR")
	}
	return candidate, nil
}

// addr6ForHost 由主机号得到本群组的 IPv6 地址（与 IPv4 主机号同值）。
func (r *Registry) addr6ForHost(host int) (netip.Addr, error) {
	subnet, err := subnetIndexOf(r.prefix)
	if err != nil {
		return netip.Addr{}, err
	}
	return protocol.MemberIPv6(subnet, host)
}

// subnetIndexOf 取本群组 IPv4 /24 的第三段，作为 IPv6 /64 的子网序号
// （控制面里两者一一对应，见 pkg/protocol.GroupIPv6Prefix）。
func subnetIndexOf(prefix netip.Prefix) (int, error) {
	if !prefix.Addr().Is4() {
		return 0, gerror.New("IPv6 subnet index requires an IPv4 CIDR")
	}
	raw := prefix.Addr().As4()
	return int(raw[2]), nil
}
