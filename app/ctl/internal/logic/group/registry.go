package group

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/logic/node"
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

	roleOwner  = "owner"
	roleMember = "member"
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

type ResetInviteInput struct {
	// OperatorPeerID 操作者，必须是群主。
	OperatorPeerID string `json:"operator_peer_id"`
	// GroupID 目标群组。
	GroupID string `json:"group_id"`
	// ValidSeconds 新邀请码有效期（秒）；0 或缺省表示永久有效。
	ValidSeconds int `json:"valid_seconds"`
}

type KickInput struct {
	// OperatorPeerID 操作者，必须是群主。
	OperatorPeerID string `json:"operator_peer_id"`
	// GroupID 目标群组。
	GroupID string `json:"group_id"`
	// TargetPeerID 被踢成员。
	TargetPeerID string `json:"target_peer_id"`
}

type Group struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	CreatorPeerID string     `json:"creator_peer_id"`
	InviteCode    string     `json:"invite_code"`
	InviteExpires *time.Time `json:"invite_expires_at,omitempty"`
	CIDR          string     `json:"cidr"`
	CreatedAt     time.Time  `json:"created_at"`
	Version       uint64     `json:"version"`

	registry    *node.Registry
	enrollToken string
}

type MemberView struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	VirtualIP string   `json:"virtual_ip"`
	Role      string   `json:"role"`
	Addrs     []string `json:"addrs"`
}

type NetMap struct {
	GroupID   string       `json:"group_id"`
	GroupName string       `json:"group_name"`
	CIDR      string       `json:"cidr"`
	Version   uint64       `json:"version"`
	Members   []MemberView `json:"members"`
}

// Registry 追加角色索引：ownerByGroup 记录每群群主，memberRoleByPeer 记录成员角色。
type Registry struct {
	mu             sync.RWMutex
	base           netip.Prefix
	nextSubnet     int
	groupsByID     map[string]*Group
	groupsByInvite map[string]*Group
	groupByPeer    map[string]string
	announcedAddrs map[string][]string
	memberRole     map[string]string // peerID → owner/member

	// store 为 SQLite 持久化层；为 nil 时退化为纯内存模式（仅测试使用）。
	store *store
}

// NewRegistry 创建纯内存注册表（无持久化，仅供测试与本地实验）。
func NewRegistry() (*Registry, error) {
	return newRegistry(nil)
}

// NewPersistentRegistry 打开（必要时初始化）SQLite 数据库并加载已有群组数据。
// 服务重启后所有群组、成员、邀请码与通告地址均自动恢复。
func NewPersistentRegistry(ctx context.Context, dbPath string) (*Registry, error) {
	st, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	reg, err := newRegistry(st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := reg.restore(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return reg, nil
}

func newRegistry(st *store) (*Registry, error) {
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
		memberRole:     make(map[string]string),
		store:          st,
	}, nil
}

// restore 从 SQLite 加载全部群组、成员与通告地址，重建内存索引。
func (r *Registry) restore(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	rows, err := r.store.loadGroups(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		grp := &Group{
			ID:            row.ID,
			Name:          row.Name,
			CreatorPeerID: row.CreatorPeerID,
			InviteCode:    row.InviteCode,
			InviteExpires: row.InviteExpires,
			CIDR:          row.CIDR,
			CreatedAt:     time.Now(),
			Version:       row.Version,
		}
		members, err := r.store.loadMembersByGroup(ctx, row.ID)
		if err != nil {
			return err
		}
		if err := r.rebuildGroup(grp, members); err != nil {
			return err
		}
		if row.SubnetIndex > r.nextSubnet-1 {
			r.nextSubnet = row.SubnetIndex + 1
		}
	}
	addrs, err := r.store.loadAnnouncedAddrs(ctx)
	if err != nil {
		return err
	}
	for peerID, list := range addrs {
		r.announcedAddrs[peerID] = list
	}
	return nil
}

// rebuildGroup 依据已加载的成员行重建群组的节点注册表（含已用 IP 状态）。
func (r *Registry) rebuildGroup(grp *Group, members []memberRow) error {
	token := "grp-" + randomString(24)
	registry, err := node.NewRegistry(grp.CIDR, []string{token})
	if err != nil {
		return fmt.Errorf("rebuild registry for group %s: %w", grp.ID, err)
	}
	// 直接按已分配的虚拟 IP 恢复成员，避免 IPAM 重新分配导致漂移。
	for _, m := range members {
		if err := registry.RestoreNode(node.Node{
			PeerID: m.PeerID, Name: m.Name, OS: m.OS, VirtualIP: m.VirtualIP,
		}); err != nil {
			return fmt.Errorf("restore member %s of group %s: %w", m.PeerID, grp.ID, err)
		}
		r.groupByPeer[m.PeerID] = grp.ID
		role := m.Role
		if role == "" {
			if m.PeerID == grp.CreatorPeerID {
				role = roleOwner
			} else {
				role = roleMember
			}
		}
		r.memberRole[m.PeerID] = role
	}
	grp.registry = registry
	grp.enrollToken = token
	r.groupsByID[grp.ID] = grp
	r.groupsByInvite[grp.InviteCode] = grp
	return nil
}

// Close 关闭底层 SQLite 连接；纯内存模式为空操作。
func (r *Registry) Close() error {
	if r.store == nil {
		return nil
	}
	return r.store.Close()
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

	creator, err := registry.Enroll(ctx, node.EnrollRequest{
		Token: token, PeerID: input.PeerID, Name: input.Name, OS: input.OS,
	})
	if err != nil {
		return nil, node.Node{}, err
	}

	// 先落库，成功后更新内存索引，保证重启后完整恢复。
	if r.store != nil {
		if err := r.store.insertGroup(ctx, groupRow{
			ID:            grp.ID,
			Name:          grp.Name,
			CreatorPeerID: grp.CreatorPeerID,
			InviteCode:    grp.InviteCode,
			InviteExpires: nil, // 创建时邀请码永久有效
			CIDR:          grp.CIDR,
			SubnetIndex:   r.nextSubnet,
			Version:       grp.Version,
		}); err != nil {
			return nil, node.Node{}, err
		}
		if err := r.store.insertMember(ctx, grp.ID, memberRow{
			PeerID: creator.PeerID, Name: creator.Name, OS: creator.OS,
			VirtualIP: creator.VirtualIP, Role: roleOwner,
		}); err != nil {
			return nil, node.Node{}, err
		}
	}

	r.nextSubnet++
	r.groupsByID[grp.ID] = grp
	r.groupsByInvite[inviteCode] = grp
	r.groupByPeer[input.PeerID] = grp.ID
	r.memberRole[input.PeerID] = roleOwner
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
	// 邀请码过期校验。
	if grp.InviteExpires != nil && time.Now().After(*grp.InviteExpires) {
		return nil, node.Node{}, gerror.New("invite code has expired")
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
	if r.store != nil {
		if err := r.store.insertMember(ctx, grp.ID, memberRow{
			PeerID: member.PeerID, Name: member.Name, OS: member.OS,
			VirtualIP: member.VirtualIP, Role: roleMember,
		}); err != nil {
			return nil, node.Node{}, err
		}
		if err := r.store.bumpGroupVersion(ctx, grp.ID); err != nil {
			return nil, node.Node{}, err
		}
	}
	r.groupByPeer[input.PeerID] = grp.ID
	r.memberRole[input.PeerID] = roleMember
	grp.Version++
	return grp, member, nil
}

// ResetInvite 群主重置邀请码：旧码立即作废，新码可选有效期。
func (r *Registry) ResetInvite(ctx context.Context, input ResetInviteInput) (string, *time.Time, error) {
	if input.OperatorPeerID == "" || input.GroupID == "" {
		return "", nil, gerror.New("operator_peer_id and group_id are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	grp, ok := r.groupsByID[input.GroupID]
	if !ok {
		return "", nil, gerror.New("unknown group")
	}
	if r.memberRole[input.OperatorPeerID] != roleOwner || r.groupByPeer[input.OperatorPeerID] != grp.ID {
		return "", nil, gerror.New("only the group owner can reset the invite code")
	}

	newCode := randomString(inviteCodeLength)
	var expires *time.Time
	// ValidSeconds > 0 设置未来过期点；负值表示立即过期（测试与紧急封禁场景）。
	if input.ValidSeconds != 0 {
		t := time.Now().Add(time.Duration(input.ValidSeconds) * time.Second)
		expires = &t
	}
	if r.store != nil {
		if err := r.store.updateInvite(ctx, grp.ID, newCode, expires); err != nil {
			return "", nil, err
		}
		if err := r.store.bumpGroupVersion(ctx, grp.ID); err != nil {
			return "", nil, err
		}
	}
	delete(r.groupsByInvite, grp.InviteCode) // 旧码立即作废
	grp.InviteCode = newCode
	grp.InviteExpires = expires
	grp.Version++
	r.groupsByInvite[newCode] = grp
	return newCode, expires, nil
}

// Kick 群主将成员移出群组：回收虚拟 IP、清除通告、作废其组内身份。
func (r *Registry) Kick(ctx context.Context, input KickInput) (*node.Node, error) {
	if input.OperatorPeerID == "" || input.GroupID == "" || input.TargetPeerID == "" {
		return nil, gerror.New("operator_peer_id, group_id and target_peer_id are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	grp, ok := r.groupsByID[input.GroupID]
	if !ok {
		return nil, gerror.New("unknown group")
	}
	if r.memberRole[input.OperatorPeerID] != roleOwner || r.groupByPeer[input.OperatorPeerID] != grp.ID {
		return nil, gerror.New("only the group owner can kick members")
	}
	if input.TargetPeerID == input.OperatorPeerID {
		return nil, gerror.New("the owner cannot kick themselves")
	}
	if r.groupByPeer[input.TargetPeerID] != grp.ID {
		return nil, gerror.New("target peer is not a member of this group")
	}

	removed, err := grp.registry.RemoveNode(input.TargetPeerID)
	if err != nil {
		return nil, err
	}
	if r.store != nil {
		if err := r.store.deleteMember(ctx, grp.ID, input.TargetPeerID); err != nil {
			return nil, err
		}
		if err := r.store.bumpGroupVersion(ctx, grp.ID); err != nil {
			return nil, err
		}
	}
	delete(r.groupByPeer, input.TargetPeerID)
	delete(r.memberRole, input.TargetPeerID)
	delete(r.announcedAddrs, input.TargetPeerID)
	grp.Version++
	return &removed, nil
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
	if r.store != nil {
		if err := r.store.replaceAnnouncedAddrs(ctx, input.PeerID, addrs); err != nil {
			return err
		}
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
		role := r.memberRole[item.PeerID]
		if role == "" {
			role = roleMember
		}
		members = append(members, MemberView{
			PeerID:    item.PeerID,
			Name:      item.Name,
			OS:        item.OS,
			VirtualIP: item.VirtualIP,
			Role:      role,
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

// GroupRow 是 Group 的别名，供适配器层引用而避免类型名冲突。
type GroupRow = Group

// MemberRow 是 MemberView 的别名，供适配器层统一引用 model 视图。
type MemberRow = MemberView
