# Lanet Godot SDK — 玩家 P2P 联机插件

Godot 4.x GDScript 插件：让游戏玩家之间经 Lanet ws-gateway 互相连接，
无需自建游戏服务器、无需端口映射、无需玩家配置网络。

```
玩家 A（Godot 游戏）──ws──→ ws-gateway ←──ws── 玩家 B（Godot 游戏）
                              桥接两条连接，数据实时转发不落盘
```

- 网关部署在任意一台有公网 IP 的服务器上（复用 Lanet 现有 ws-gateway）；
- 玩家客户端只需能访问网关（出站 WebSocket，NAT 友好）；
- 玩家之间按「鉴权名」寻址：A 开流到 B，网关在两条连接间桥接。

## 目录结构

```
addons/lanet/
├── plugin.cfg              插件清单
├── lanet.gd                EditorPlugin 入口（注册单例）
├── gateway/
│   ├── gateway_frame.gd    帧协议编解码（与 Go/C#/JS 字节级一致）
│   ├── gateway_client.gd   WebSocket 客户端：鉴权/心跳/开流/收流入向流
│   └── gateway_stream.gd   桥接流对象（读写/半关闭/中止）
├── multiplayer/
│   └── lanet_multiplayer_peer.gd  MultiplayerPeer 实现（接入 @rpc）
├── lanet_manager.gd        高层 API：创建/加入房间、玩家事件
└── README.md
examples/two_players/       双人位置同步最小示例
```

## 快速开始（高层 API）

```gdscript
# 1. 启用插件后，用 LanetManager 单例：
Lanet.player_joined.connect(_on_player_joined)
Lanet.player_left.connect(_on_player_left)

# 2. 创建房间（房主）：
Lanet.create_room("ws://your-server:8700/gateway", "INVITE_CODE", "alice")

# 3. 加入房间（其他玩家）：
Lanet.join_room("ws://your-server:8700/gateway", "INVITE_CODE", "bob", "alice")

# 4. 收发消息：
Lanet.send_to("alice", {"pos": my_pos})
Lanet.broadcast({"pos": my_pos})
```

## MultiplayerAPI 接入（@rpc）

```gdscript
# LanetManager 建连成功后已自动把房间接入 MultiplayerAPI（@rpc 可直接用）；
# 如需手动重新挂接（幂等）：
multiplayer.multiplayer_peer = Lanet.make_multiplayer_peer()

@rpc("any_peer", "call_local", "reliable")
func sync_position(pos: Vector2) -> void:
    $Player.position = pos
```

联机拓扑与 Godot 官方 server relay 一致：房主即 server（uid=1），加入者
随机分配 uid；guest 间 RPC 经房主自动转发，玩家列表由引擎 SYS/ADD_PEER
机制同步，业务代码无需关心。

## 帧协议

与 Go/C#/JS 三端共用一套二进制帧（见 `pkg/gatewayproto`）：

```
[type:1][streamID:4 大端][payload 长度:4 大端][payload]
```

玩家互连是本 SDK 的扩展：Dial 帧 payload 带 `peer` 字段（目标玩家鉴权名），
网关桥接两条 WS 连接；对端收到 `StreamOpen` 帧（protocol + 发起方名字）。

## 能力边界（如实声明）

- ✅ Windows / Android / Linux / macOS（WebSocket 出站连接，无需管理员/VPN 权限）
- ✅ 可靠流（TCP 语义）：适合回合制、卡牌、房间制、聊天、状态同步
- ✅ Godot MultiplayerAPI / @rpc 接入
- ❌ iOS / WebGL：未验证，不承诺支持
- ❌ 不可靠/无序通道：帧协议为可靠流，实时动作类高频位置同步需自行做冗余/插值
- ❌ 端到端延迟保证：数据经网关转发，延迟 = 两段出站链路之和
- 玩家名字在网关内全局唯一；断线后名字自动释放，重连用同一名字即可

## 网关部署

```bash
go run ./app/gateway/cmd/pvn-gateway -ctl http://127.0.0.1:8000 -listen :8700 -invite <邀请码>
```

生产建议：wss 反代（nginx/caddy 终结 TLS）；客户端每 30s 心跳保活（SDK 已内置）。
