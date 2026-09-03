package group

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/service/node"
	"github.com/gogf/gf/v2/errors/gerror"
)

// 群组网络模型：
// - 每个群组从 100.64.0.0/16 中分配一个独立 /24 子网，群组之间网络隔离。
// - 创建者自动成为第一个成员；其他节点凭邀请码加入。
// - NetMap 只返回同一群组的成员，作为 P2P 直连与后续隧道的唯一可见范围。

const (
	baseCIDR          = "100.64.0.0/16"
	inviteCodeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	inviteCodeLength  = 10
)

type CreateInput struct {
	PeerID    string `json:"peer_id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	GroupName string `json:"group_name"`
}

type JoinInput struct {
	InviteCode string `json:"invite_code"`
	PeerID     string `json:"peer_id"`
	Name       string `json:"name"`
	OS         string `json:"os"`
}

type AnnounceInput struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

type Group struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CreatorPeerID string    `json:"creator_peer_id"`
	InviteCode    string    `json:"invite_code"`
	CIDR          string    `json:"cidr"`
	CreatedAt     time.Time `json:"created_at"`
	Version       uint64    `json:"version"`

	registry    *node.Registry
	enrollToken string
}

type MemberView struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	VirtualIP string   `json:"virtual_ip"`
	Addrs     []string `json:"addrs"`
}

type NetMap struct {
	GroupID   string       `json:"group_id"`
	GroupName string       `json:"group_name"`
	CIDR      string       `json:"cidr"`
	Version   uint64       `json:"version"`
	Members   []MemberView `json:"members"`
}

type Registry struct {
	mu             sync.RWMutex
	base           netip.Prefix
	nextSubnet     int
	groupsByID     map[string]*Group
	groupsByInvite map[string]*Group
	groupByPeer    map[string]string
	announcedAddrs map[string][]string
}

func NewRegistry() (*Registry, error) {
	base, err := netip.ParsePrefix(baseCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse base CIDR: %w", err)
	}
	return &Registry{
		base:           base.Masked(),
		nextSubnet:     0,
		groupsByID:     make(map[string]*Group),
		groupsByInvite: make(map[string]*Group),
		groupByPeer:    make(map[string]string),
		announcedAddrs: make(map[string][]string),
	}, nil
}

func (r *Registry) Create(ctx context.Context, input CreateInput) (*Group, node.Node, error) {
	if input.PeerID == "" || input.Name == "" || input.GroupName == "" {
		return nil, node.Node{}, gerror.New("peer_id, name and group_name are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.groupByPeer[input.PeerID]; exists {
		return nil, node.Node{}, gerror.New("peer already belongs to a group; leave it before creating another")
	}
	if r.nextSubnet > 255 {
		return nil, node.Node{}, gerror.New("group subnet pool is exhausted")
	}

	cidr := fmt.Sprintf("%s.%d.0/24", subnetBase(r.base), r.nextSubnet)
	token := "grp-" + randomString(24)
	registry, err := node.NewRegistry(cidr, []string{token})
	if err != nil {
		return nil, node.Node{}, err
	}

	inviteCode := randomString(inviteCodeLength)
	grp := &Group{
		ID:            fmt.Sprintf("g%d", r.nextSubnet),
		Name:          input.GroupName,
		CreatorPeerID: input.PeerID,
		InviteCode:    inviteCode,
		CIDR:          cidr,
		CreatedAt:     time.Now(),
		Version:       1,
		registry:      registry,
		enrollToken:   token,
	}
	r.nextSubnet++

	creator, err := registry.Enroll(ctx, node.EnrollRequest{
		Token: token, PeerID: input.PeerID, Name: input.Name, OS: input.OS,
	})
	if err != nil {
		return nil, node.Node{}, err
	}

	r.groupsByID[grp.ID] = grp
	r.groupsByInvite[inviteCode] = grp
	r.groupByPeer[input.PeerID] = grp.ID
	return grp, creator, nil
}

func (r *Registry) Join(ctx context.Context, input JoinInput) (*Group, node.Node, error) {
	if input.InviteCode == "" || input.PeerID == "" || input.Name == "" {
		return nil, node.Node{}, gerror.New("invite_code, peer_id and name are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	grp, ok := r.groupsByInvite[input.InviteCode]
	if !ok {
		return nil, node.Node{}, gerror.New("invalid invite code")
	}
	if existingGroupID, exists := r.groupByPeer[input.PeerID]; exists {
		if existingGroupID != grp.ID {
			return nil, node.Node{}, gerror.New("peer already belongs to another group")
		}
	}
	member, err := grp.registry.Enroll(ctx, node.EnrollRequest{
		Token: grp.enrollToken, PeerID: input.PeerID, Name: input.Name, OS: input.OS,
	})
	if err != nil {
		return nil, node.Node{}, err
	}
	r.groupByPeer[input.PeerID] = grp.ID
	grp.Version++
	return grp, member, nil
}

func (r *Registry) Announce(ctx context.Context, input AnnounceInput) error {
	if input.PeerID == "" || len(input.Addrs) == 0 {
		return gerror.New("peer_id and at least one address are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.groupByPeer[input.PeerID]; !ok {
		return gerror.New("unknown peer")
	}
	addrs := make([]string, 0, len(input.Addrs))
	for _, addr := range input.Addrs {
		if addr != "" {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return gerror.New("no valid addresses provided")
	}
	r.announcedAddrs[input.PeerID] = addrs
	return nil
}

func (r *Registry) NetMapFor(ctx context.Context, peerID string) (*NetMap, error) {
	if peerID == "" {
		return nil, gerror.New("peer_id is required")
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	groupID, ok := r.groupByPeer[peerID]
	if !ok {
		return nil, gerror.New("peer has not joined any group")
	}
	grp := r.groupsByID[groupID]

	members := make([]MemberView, 0, 8)
	for _, item := range grp.registry.List(ctx) {
		members = append(members, MemberView{
			PeerID:    item.PeerID,
			Name:      item.Name,
			OS:        item.OS,
			VirtualIP: item.VirtualIP,
			Addrs:     append([]string(nil), r.announcedAddrs[item.PeerID]...),
		})
	}
	return &NetMap{
		GroupID:   grp.ID,
		GroupName: grp.Name,
		CIDR:      grp.CIDR,
		Version:   grp.Version,
		Members:   members,
	}, nil
}

func (r *Registry) ListGroups(ctx context.Context) []Group {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]Group, 0, len(r.groupsByID))
	for _, grp := range r.groupsByID {
		items = append(items, *grp)
	}
	return items
}

func (r *Registry) GroupOf(peerID string) (*Group, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	groupID, ok := r.groupByPeer[peerID]
	if !ok {
		return nil, false
	}
	grp := r.groupsByID[groupID]
	return grp, true
}

func subnetBase(base netip.Prefix) string {
	addr := base.Addr().As4()
	return fmt.Sprintf("%d.%d", addr[0], addr[1])
}

func randomString(length int) string {
	result := make([]byte, length)
	max := big.NewInt(int64(len(inviteCodeCharset)))
	for i := range result {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(fmt.Sprintf("generate random string: %v", err))
		}
		result[i] = inviteCodeCharset[n.Int64()]
	}
	return string(result)
}
