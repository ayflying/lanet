// =================================================================================
// 群组业务接口定义（gf 规范：service 层仅声明接口，实现在 internal/logic/group）。
// =================================================================================

package service

import (
	"context"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/model"
)

// GroupInput/Output 借用 api 侧结构会导致循环依赖，
// 因此 logic 层的输入输出统一在 logic 包内定义，service 层只声明方法签名。

type IGroup interface {
	// Create 创建群组，创建者自动成为第一个成员（owner）。
	Create(ctx context.Context, peerID, name, osName, groupName string) (group GroupInfo, creator model.NodeView, inviteCode string, err error)
	// Join 凭邀请码加入群组。
	Join(ctx context.Context, inviteCode, peerID, name, osName string) (group GroupInfo, member model.NodeView, err error)
	// Announce 成员通告可达地址。
	Announce(ctx context.Context, peerID string, addrs []string) error
	// ResetInvite 群主重置邀请码，返回新码与过期时间。
	ResetInvite(ctx context.Context, operatorPeerID, groupID string, validSeconds int) (code string, expires *time.Time, err error)
	// Kick 群主踢出成员，返回被移除节点。
	Kick(ctx context.Context, operatorPeerID, groupID, targetPeerID string) (removed model.NodeView, err error)
	// NetMapFor 查询节点所在群组的成员目录。
	NetMapFor(ctx context.Context, peerID string) (netmap NetMapInfo, err error)
	// ListGroups 列出全部群组。
	ListGroups(ctx context.Context) ([]GroupInfo, error)
	// Close 关闭底层资源（SQLite 连接等）。
	Close() error
}

// GroupInfo 群组信息。
type GroupInfo struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	CreatorPeerID string     `json:"creator_peer_id"`
	CIDR          string     `json:"cidr"`
	CreatedAt     time.Time  `json:"created_at"`
	Version       uint64     `json:"version"`
	InviteExpires *time.Time `json:"invite_expires_at,omitempty"`
}

// NetMapInfo 群组成员目录。
type NetMapInfo struct {
	GroupID   string             `json:"group_id"`
	GroupName string             `json:"group_name"`
	CIDR      string             `json:"cidr"`
	Version   uint64             `json:"version"`
	Members   []model.MemberView `json:"members"`
}

// 本地注册的接口实例（gf 规范：logic 层实现后调用 Register 注册）。
var (
	localGroup IGroup
)

// RegisterGroup 注册群组服务实例（ctl 启动时调用一次）。
func RegisterGroup(svc IGroup) {
	localGroup = svc
}

// Group 返回已注册的群组服务实例。
func Group() IGroup {
	return localGroup
}
