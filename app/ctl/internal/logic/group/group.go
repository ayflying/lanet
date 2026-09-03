// =================================================================================
// 群组 logic 实现：适配 service.IGroup 接口。
// 核心逻辑在 registry.go 的 Registry（内存索引 + SQLite 落库），
// 本文件负责把接口的按参数签名桥接到 Registry 的 Input 结构体签名，
// 并提供 gf 规范的单例注册（供 controller 通过 service.Group() 获取）。
// =================================================================================

package group

import (
	"context"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/logic/node"
	"github.com/ayflying/pvn/app/ctl/internal/model"
	"github.com/ayflying/pvn/app/ctl/internal/service"
)

// GroupService 实现 service.IGroup 接口的适配器，内部持有真正的 Registry。
type GroupService struct {
	reg *Registry
}

// New 构造服务适配器（内存模式）。
func New() (*GroupService, error) {
	reg, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	return &GroupService{reg: reg}, nil
}

// NewPersistent 构造服务适配器（SQLite 持久化模式）。
func NewPersistent(ctx context.Context, dbPath string) (*GroupService, error) {
	reg, err := NewPersistentRegistry(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	return &GroupService{reg: reg}, nil
}

// infoOf 把内部 Group 转成对外信息结构。
func (g *GroupService) infoOf(grp *GroupRow) service.GroupInfo {
	return service.GroupInfo{
		ID:            grp.ID,
		Name:          grp.Name,
		CreatorPeerID: grp.CreatorPeerID,
		CIDR:          grp.CIDR,
		CreatedAt:     grp.CreatedAt,
		Version:       grp.Version,
		InviteExpires: grp.InviteExpires,
	}
}

func (g *GroupService) Create(ctx context.Context, peerID, name, osName, groupName string) (service.GroupInfo, model.NodeView, string, error) {
	grp, creator, err := g.reg.Create(ctx, CreateInput{
		PeerID: peerID, Name: name, OS: osName, GroupName: groupName,
	})
	if err != nil {
		return service.GroupInfo{}, model.NodeView{}, "", err
	}
	return g.infoOf(grp), nodeView(creator), grp.InviteCode, nil
}

func (g *GroupService) Join(ctx context.Context, inviteCode, peerID, name, osName string) (service.GroupInfo, model.NodeView, error) {
	grp, member, err := g.reg.Join(ctx, JoinInput{
		InviteCode: inviteCode, PeerID: peerID, Name: name, OS: osName,
	})
	if err != nil {
		return service.GroupInfo{}, model.NodeView{}, err
	}
	return g.infoOf(grp), nodeView(member), nil
}

func (g *GroupService) Announce(ctx context.Context, peerID string, addrs []string) error {
	return g.reg.Announce(ctx, AnnounceInput{PeerID: peerID, Addrs: addrs})
}

func (g *GroupService) ResetInvite(ctx context.Context, operatorPeerID, groupID string, validSeconds int) (string, *time.Time, error) {
	return g.reg.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: operatorPeerID, GroupID: groupID, ValidSeconds: validSeconds,
	})
}

func (g *GroupService) Kick(ctx context.Context, operatorPeerID, groupID, targetPeerID string) (model.NodeView, error) {
	removed, err := g.reg.Kick(ctx, KickInput{
		OperatorPeerID: operatorPeerID, GroupID: groupID, TargetPeerID: targetPeerID,
	})
	if err != nil {
		return model.NodeView{}, err
	}
	return nodeView(*removed), nil
}

func (g *GroupService) NetMapFor(ctx context.Context, peerID string) (service.NetMapInfo, error) {
	nm, err := g.reg.NetMapFor(ctx, peerID)
	if err != nil {
		return service.NetMapInfo{}, err
	}
	return service.NetMapInfo{
		GroupID:   nm.GroupID,
		GroupName: nm.GroupName,
		CIDR:      nm.CIDR,
		Version:   nm.Version,
		Members:   memberViews(nm.Members),
	}, nil
}

func (g *GroupService) ListGroups(ctx context.Context) ([]service.GroupInfo, error) {
	items := g.reg.ListGroups(ctx)
	out := make([]service.GroupInfo, 0, len(items))
	for i := range items {
		out = append(out, g.infoOf(&items[i]))
	}
	return out, nil
}

func (g *GroupService) Close() error {
	return g.reg.Close()
}

// MemberView 转换：内部 MemberView 与 model.MemberView 字段一致，逐字段拷贝。
func memberViews(items []MemberRow) []model.MemberView {
	out := make([]model.MemberView, 0, len(items))
	for _, m := range items {
		out = append(out, model.MemberView{
			PeerID:    m.PeerID,
			Name:      m.Name,
			OS:        m.OS,
			VirtualIP: m.VirtualIP,
			Role:      m.Role,
			Addrs:     append([]string(nil), m.Addrs...),
		})
	}
	return out
}

// nodeView 把内部 node.Node 转成对外视图。
func nodeView(n node.Node) model.NodeView {
	return model.NodeView{
		PeerID:     n.PeerID,
		Name:       n.Name,
		OS:         n.OS,
		VirtualIP:  n.VirtualIP,
		EnrolledAt: n.EnrolledAt,
	}
}

// 本地注册与获取（gf 规范：logic 实现后注册到 service 接口）。
var (
	localService service.IGroup
)

// Register 注册服务实例（ctl 启动时调用一次）。
func Register(svc service.IGroup) {
	localService = svc
}

// Service 返回已注册的服务实例。
func Service() service.IGroup {
	return localService
}
