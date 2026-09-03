// =================================================================================
// 群组控制器：实现 api/group/v1.IGroupV1，业务委托给 service.IGroup。
// =================================================================================

package group

import (
	"context"

	v1 "github.com/ayflying/pvn/app/ctl/api/group/v1"
	"github.com/ayflying/pvn/app/ctl/internal/service"
	"github.com/gogf/gf/v2/frame/g"
)

type ControllerV1 struct{}

func NewV1() v1.IGroupV1 {
	return &ControllerV1{}
}

// groupView 把 service 层群组信息转成 api 视图。
func groupView(info service.GroupInfo) v1.GroupView {
	return v1.GroupView{
		ID:            info.ID,
		Name:          info.Name,
		CreatorPeerID: info.CreatorPeerID,
		CIDR:          info.CIDR,
		CreatedAt:     info.CreatedAt,
		Version:       info.Version,
		InviteExpires: info.InviteExpires,
	}
}

// Create 创建群组。
func (c *ControllerV1) Create(ctx context.Context, req *v1.CreateReq) (res *v1.CreateRes, err error) {
	info, creator, inviteCode, err := service.Group().Create(ctx, req.PeerID, req.Name, req.OS, req.GroupName)
	if err != nil {
		return nil, err
	}
	return &v1.CreateRes{
		Group:      groupView(info),
		Creator:    creator,
		InviteCode: inviteCode,
	}, nil
}

// Join 凭邀请码加入群组。
func (c *ControllerV1) Join(ctx context.Context, req *v1.JoinReq) (res *v1.JoinRes, err error) {
	info, member, err := service.Group().Join(ctx, req.InviteCode, req.PeerID, req.Name, req.OS)
	if err != nil {
		return nil, err
	}
	return &v1.JoinRes{
		Group:  groupView(info),
		Member: member,
	}, nil
}

// Announce 通告可达地址。
func (c *ControllerV1) Announce(ctx context.Context, req *v1.AnnounceReq) (res *v1.AnnounceRes, err error) {
	if err = service.Group().Announce(ctx, req.PeerID, req.Addrs); err != nil {
		return nil, err
	}
	return &v1.AnnounceRes{Status: "announced"}, nil
}

// ResetInvite 群主重置邀请码。
func (c *ControllerV1) ResetInvite(ctx context.Context, req *v1.ResetInviteReq) (res *v1.ResetInviteRes, err error) {
	code, expires, err := service.Group().ResetInvite(ctx, req.OperatorPeerID, req.GroupID, req.ValidSeconds)
	if err != nil {
		return nil, err
	}
	return &v1.ResetInviteRes{
		InviteCode:      code,
		InviteExpiresAt: expires,
	}, nil
}

// Kick 群主踢出成员。
func (c *ControllerV1) Kick(ctx context.Context, req *v1.KickReq) (res *v1.KickRes, err error) {
	removed, err := service.Group().Kick(ctx, req.OperatorPeerID, req.GroupID, req.TargetPeerID)
	if err != nil {
		return nil, err
	}
	return &v1.KickRes{
		Kicked:    req.TargetPeerID,
		VirtualIP: removed.VirtualIP,
		Status:    "removed",
	}, nil
}

// NetMap 查询所在群组成员目录。
func (c *ControllerV1) NetMap(ctx context.Context, req *v1.NetMapReq) (res *v1.NetMapRes, err error) {
	info, err := service.Group().NetMapFor(ctx, req.PeerID)
	if err != nil {
		return nil, err
	}
	return &v1.NetMapRes{
		GroupID:   info.GroupID,
		GroupName: info.GroupName,
		CIDR:      info.CIDR,
		Version:   info.Version,
		Members:   info.Members,
	}, nil
}

// ListGroups 列出全部群组。
func (c *ControllerV1) ListGroups(ctx context.Context, req *v1.ListGroupsReq) (res *v1.ListGroupsRes, err error) {
	items, err := service.Group().ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	groups := make([]v1.GroupView, 0, len(items))
	for _, info := range items {
		groups = append(groups, groupView(info))
	}
	g.Log().Debugf(ctx, "list groups: %d", len(groups))
	return &v1.ListGroupsRes{Groups: groups}, nil
}
