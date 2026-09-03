// =================================================================================
// 群组 API 版本 v1 定义。
// =================================================================================

package v1

import (
	"context"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/model"
	"github.com/gogf/gf/v2/frame/g"
)

// GroupView 对客户端可见的群组信息（隐藏邀请码、内部注册表等字段）。
type GroupView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	CreatorPeerID string     `json:"creator_peer_id"`
	CIDR          string     `json:"cidr"`
	CreatedAt     time.Time  `json:"created_at"`
	Version       uint64     `json:"version"`
	InviteExpires *time.Time `json:"invite_expires_at,omitempty"`
}

type IGroupV1 interface {
	Create(ctx context.Context, req *CreateReq) (res *CreateRes, err error)
	Join(ctx context.Context, req *JoinReq) (res *JoinRes, err error)
	Announce(ctx context.Context, req *AnnounceReq) (res *AnnounceRes, err error)
	ResetInvite(ctx context.Context, req *ResetInviteReq) (res *ResetInviteRes, err error)
	Kick(ctx context.Context, req *KickReq) (res *KickRes, err error)
	NetMap(ctx context.Context, req *NetMapReq) (res *NetMapRes, err error)
	ListGroups(ctx context.Context, req *ListGroupsReq) (res *ListGroupsRes, err error)
}

// CreateReq 创建群组：创建者自动成为第一个成员（owner）。
type CreateReq struct {
	g.Meta    `path:"/v1/groups/create" method:"post" tags:"Group" summary:"创建群组"`
	PeerID    string `json:"peer_id" v:"required" dc:"节点 PeerID"`
	Name      string `json:"name" v:"required" dc:"节点名称"`
	OS        string `json:"os" dc:"操作系统标识"`
	GroupName string `json:"group_name" v:"required" dc:"群组名称"`
}

// CreateRes 创建群组响应：返回群组信息与邀请码。
type CreateRes struct {
	Group      GroupView      `json:"group"`
	Creator    model.NodeView `json:"creator"`
	InviteCode string         `json:"invite_code"`
}

// JoinReq 凭邀请码加入群组。
type JoinReq struct {
	g.Meta     `path:"/v1/groups/join" method:"post" tags:"Group" summary:"凭邀请码加入群组"`
	InviteCode string `json:"invite_code" v:"required" dc:"邀请码"`
	PeerID     string `json:"peer_id" v:"required" dc:"节点 PeerID"`
	Name       string `json:"name" v:"required" dc:"节点名称"`
	OS         string `json:"os" dc:"操作系统标识"`
}

// JoinRes 加入群组响应。
type JoinRes struct {
	Group  GroupView      `json:"group"`
	Member model.NodeView `json:"member"`
}

// AnnounceReq 成员通告自己的可达地址（libp2p multiaddr）。
type AnnounceReq struct {
	g.Meta `path:"/v1/groups/announce" method:"post" tags:"Group" summary:"通告可达地址"`
	PeerID string   `json:"peer_id" v:"required" dc:"节点 PeerID"`
	Addrs  []string `json:"addrs" v:"required" dc:"可达地址列表"`
}

// AnnounceRes 通告响应。
type AnnounceRes struct {
	Status string `json:"status"`
}

// ResetInviteReq 群主重置邀请码：旧码立即作废，新码可选有效期。
type ResetInviteReq struct {
	g.Meta         `path:"/v1/groups/invite/reset" method:"post" tags:"Group" summary:"群主重置邀请码"`
	OperatorPeerID string `json:"operator_peer_id" v:"required" dc:"操作者 PeerID（须为群主）"`
	GroupID        string `json:"group_id" v:"required" dc:"群组 ID"`
	ValidSeconds   int    `json:"valid_seconds" dc:"新邀请码有效期（秒），0 表示永久"`
}

// ResetInviteRes 重置邀请码响应。
type ResetInviteRes struct {
	InviteCode      string     `json:"invite_code"`
	InviteExpiresAt *time.Time `json:"invite_expires_at"`
}

// KickReq 群主踢出成员。
type KickReq struct {
	g.Meta         `path:"/v1/groups/kick" method:"post" tags:"Group" summary:"群主踢出成员"`
	OperatorPeerID string `json:"operator_peer_id" v:"required" dc:"操作者 PeerID（须为群主）"`
	GroupID        string `json:"group_id" v:"required" dc:"群组 ID"`
	TargetPeerID   string `json:"target_peer_id" v:"required" dc:"被踢成员 PeerID"`
}

// KickRes 踢出响应。
type KickRes struct {
	Kicked    string `json:"kicked" dc:"被踢成员 PeerID"`
	VirtualIP string `json:"virtual_ip" dc:"被回收的虚拟 IP"`
	Status    string `json:"status"`
}

// NetMapReq 查询自己所在群组的成员目录。
type NetMapReq struct {
	g.Meta `path:"/v1/groups/netmap" method:"get" tags:"Group" summary:"查询群组 NetMap"`
	PeerID string `json:"peer_id" v:"required" dc:"节点 PeerID"`
}

// NetMapRes NetMap 响应：仅包含同群成员。
type NetMapRes struct {
	GroupID   string            `json:"group_id"`
	GroupName string            `json:"group_name"`
	CIDR      string            `json:"cidr"`
	Version   uint64            `json:"version"`
	Members   []model.MemberView `json:"members"`
}

// ListGroupsReq 列出全部群组。
type ListGroupsReq struct {
	g.Meta `path:"/v1/groups" method:"get" tags:"Group" summary:"列出全部群组"`
}

// ListGroupsRes 群组列表响应。
type ListGroupsRes struct {
	Groups []GroupView `json:"groups"`
}
