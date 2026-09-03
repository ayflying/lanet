package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/ayflying/pvn/app/ctl/internal/service"
	groupservice "github.com/ayflying/pvn/app/ctl/internal/service/group"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gcmd"
)

var (
	relayDirectory = service.NewRelayDirectory()
	groupRegistry  = mustNewGroupRegistry()

	Main = gcmd.Command{
		Name:  "main",
		Usage: "main",
		Brief: "start PVN control plane",
		Func: func(ctx context.Context, parser *gcmd.Parser) error {
			server := g.Server()
			// PVN_CTL_ADDR 允许部署时覆盖监听地址（如 127.0.0.1:18080），
			// 优先级高于 config.yaml 的 server.address。
			if addr := os.Getenv("PVN_CTL_ADDR"); addr != "" {
				server.SetAddr(addr)
			}
			server.Group("/", func(group *ghttp.RouterGroup) {
				group.Middleware(ghttp.MiddlewareHandlerResponse)
				group.GET("/healthz", func(request *ghttp.Request) {
					request.Response.WriteJson(g.Map{"status": "ok", "service": "pvn-ctl"})
				})

				// 群组网络：创建群组，创建者自动成为第一个成员。
				group.POST("/v1/groups/create", func(request *ghttp.Request) {
					var input groupservice.CreateInput
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					grp, creator, err := groupRegistry.Create(request.Context(), input)
					if err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{
						"group":       publicGroup(grp),
						"creator":     creator,
						"invite_code": grp.InviteCode,
					})
				})

				// 凭邀请码加入群组。
				group.POST("/v1/groups/join", func(request *ghttp.Request) {
					var input groupservice.JoinInput
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					grp, member, err := groupRegistry.Join(request.Context(), input)
					if err != nil {
						writeError(request, http.StatusUnauthorized, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{
						"group":  publicGroup(grp),
						"member": member,
					})
				})

				// 成员通告自己的可达地址（libp2p multiaddr），供组内其他成员直连。
				group.POST("/v1/groups/announce", func(request *ghttp.Request) {
					var input groupservice.AnnounceInput
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					if err := groupRegistry.Announce(request.Context(), input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{"status": "announced"})
				})

				// 群主重置邀请码：旧码立即作废，新码可选有效期（秒）。
				group.POST("/v1/groups/invite/reset", func(request *ghttp.Request) {
					var input groupservice.ResetInviteInput
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					code, expires, err := groupRegistry.ResetInvite(request.Context(), input)
					if err != nil {
						writeError(request, http.StatusForbidden, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{
						"invite_code":        code,
						"invite_expires_at": expires,
					})
				})

				// 群主踢出成员：回收虚拟 IP 并清除其通告地址。
				group.POST("/v1/groups/kick", func(request *ghttp.Request) {
					var input groupservice.KickInput
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					removed, err := groupRegistry.Kick(request.Context(), input)
					if err != nil {
						writeError(request, http.StatusForbidden, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{
						"kicked":     input.TargetPeerID,
						"virtual_ip": removed.VirtualIP,
						"status":     "removed",
					})
				})

				// NetMap：仅返回同一群组的成员（虚拟 IP + PeerID + 可达地址）。
				group.GET("/v1/groups/netmap", func(request *ghttp.Request) {
					peerID := request.GetQuery("peer_id").String()
					netmap, err := groupRegistry.NetMapFor(request.Context(), peerID)
					if err != nil {
						writeError(request, http.StatusNotFound, err.Error())
						return
					}
					request.Response.WriteJson(netmap)
				})

				group.GET("/v1/groups", func(request *ghttp.Request) {
					request.Response.WriteJson(g.Map{"groups": groupRegistry.ListGroups(request.Context())})
				})

				group.POST("/v1/relays/register", func(request *ghttp.Request) {
					var input service.RelayRegistration
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					if err := relayDirectory.Register(request.Context(), input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{"status": "registered"})
				})
				group.POST("/v1/relays/heartbeat", func(request *ghttp.Request) {
					var input service.RelayHeartbeat
					if err := request.Parse(&input); err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					if err := relayDirectory.Heartbeat(request.Context(), input); err != nil {
						writeError(request, http.StatusNotFound, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{"status": "healthy"})
				})
				group.GET("/v1/relays/candidates", func(request *ghttp.Request) {
					limit := 2
					if rawLimit := request.GetQuery("limit").String(); rawLimit != "" {
						parsedLimit, err := strconv.Atoi(rawLimit)
						if err != nil {
							writeError(request, http.StatusBadRequest, "limit must be an integer")
							return
						}
						limit = parsedLimit
					}
					candidates, err := relayDirectory.List(request.Context(), limit)
					if err != nil {
						writeError(request, http.StatusBadRequest, err.Error())
						return
					}
					request.Response.WriteJson(g.Map{"candidates": candidates, "updated_at": relayDirectory.UpdatedAt()})
				})
			})
			server.Run()
			return nil
		},
	}
)

// mustNewGroupRegistry 初始化群组注册表：
// 优先使用 PVN_CTL_DB 指定的 SQLite 文件（重启不丢数据）；
// 未设置时退化为纯内存模式（本地快速实验）。
func mustNewGroupRegistry() *groupservice.Registry {
	if dbPath := os.Getenv("PVN_CTL_DB"); dbPath != "" {
		registry, err := groupservice.NewPersistentRegistry(context.Background(), dbPath)
		if err != nil {
			panic(fmt.Sprintf("open group database %s: %v", dbPath, err))
		}
		g.Log().Infof(context.Background(), "group registry backed by SQLite: %s", dbPath)
		return registry
	}
	registry, err := groupservice.NewRegistry()
	if err != nil {
		panic(err)
	}
	return registry
}

// publicGroup 隐藏内部字段，仅输出对客户端可见的群组信息。
func publicGroup(grp *groupservice.Group) g.Map {
	return g.Map{
		"id":              grp.ID,
		"name":            grp.Name,
		"creator_peer_id": grp.CreatorPeerID,
		"cidr":            grp.CIDR,
		"created_at":      grp.CreatedAt,
		"version":         grp.Version,
	}
}

func writeError(request *ghttp.Request, status int, message string) {
	request.Response.WriteStatus(status)
	request.Response.WriteJson(g.Map{"error": message})
}
