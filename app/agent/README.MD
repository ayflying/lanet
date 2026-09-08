# Lanet 节点与客户端

本目录包含两条运行路径：

| 入口 | 模式 | 用途 |
|---|---|---|
| `cmd/pvn-node` | Standalone | 官方发行版入口；无需 `ctl`/独立 `relay`，带 TUN、控制台、托盘和 P2P 更新 |
| `cmd/pvn-agent` | 托管模式 | 通过 `ctl` 建群或凭邀请码入群，使用独立 `relay` 兜底 |

新部署优先使用 `pvn-node`。`pvn-agent` 主要用于托管模式兼容和端到端验证。

## pvn-node

```bash
go run ./app/agent/cmd/pvn-node -name node-a -key 'private-network'
```

相同网络密钥的节点经 mDNS 和 DHT 自动发现。节点默认启用 TUN 与 `allow-all`
防火墙，获得 `10.7.x.x` 虚拟 IP；Windows 需要管理员权限，Linux 需要
`/dev/net/tun` 与 `CAP_NET_ADMIN`。TUN 初始化失败只会降级为应用层模式。

常用参数和对应环境变量：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `-config` | `LANET_CONFIG` | 配置路径；默认可执行文件同目录 `lanet.json` |
| `-name` | `LANET_NAME` | 节点名称 |
| `-key` | `LANET_NETWORK_KEY` | 网络密钥；空值为官方公共网络 |
| `-bootstrap` | `LANET_BOOTSTRAP` | `public`、`none` 或逗号分隔的成员 multiaddr |
| `-listen` | `LANET_LISTEN` | 逗号分隔的 libp2p 监听地址 |
| `-console` | `LANET_CONSOLE` | 控制台地址；默认 `127.0.0.1:8900`，`-` 为关闭 |
| `-console-password` | `LANET_CONSOLE_PASSWORD` | 控制台密码 |
| `-fw` | `LANET_FW` | `deny-all`、`allow-list`、`allow-all` |
| `-no-public-dht` | `LANET_NO_PUBLIC_DHT` | 关闭公共 DHT 兜底 |
| `-tun true/false` | - | 是否启用 TUN；默认读取配置，缺省为 `true` |
| `-probe` | - | 成员探测间隔 |

优先级是“显式命令行 > 环境变量 > `lanet.json` > 内置默认值”。节点配置保存后
重启生效，防火墙和端口转发在控制台中热更新。

默认配置结构：

```json
{
  "name": "node-a",
  "network_key": "private-network",
  "bootstrap": "public",
  "console": "127.0.0.1:8900",
  "console_password": "",
  "firewall": "allow-all",
  "listen": "",
  "no_public_dht": false,
  "probe_seconds": 20,
  "tun": true,
  "github_token": ""
}
```

Windows 的 `node.key` 与程序目录一起迁移；其他平台身份路径固定为
`/data/node.key`。`state.json` 和 `lanet.log` 位于配置文件目录。

## pvn-agent

先启动 [`../ctl`](../ctl/README.MD) 和 [`../relay`](../relay/README.MD)，再创建群组：

```bash
go run ./app/agent/cmd/pvn-agent \
  -ctl http://127.0.0.1:8000 \
  -mode create -name alpha -group demo -real-tun -fw allow-all
```

日志会打印邀请码。其他节点凭码加入：

```bash
go run ./app/agent/cmd/pvn-agent \
  -ctl http://127.0.0.1:8000 \
  -mode join -invite '<邀请码>' -name beta -real-tun -fw allow-all
```

`pvn-agent` 默认使用内存 TUN，只适合协议联调。要操作真实系统网卡必须传
`-real-tun` 并授予对应系统权限。默认防火墙是 `deny-all`；验证 `ping` 或端口
访问时需明确改为 `allow-all`。

## 验证工具

| 入口 | 覆盖内容 |
|---|---|
| `cmd/pvn-e2e-check` | 本地启动 ctl、relay 和双 agent，验证托管模式 |
| `cmd/pvn-serverless-check` | Standalone 双节点发现和隧道往返 |
| `cmd/pvn-firewall-check` | 入向防火墙与端口规则 |
| `cmd/pvn-identity-check` | 身份持久化与虚拟地址 |
| `cmd/pvn-web-echo` | Web SDK 与 Go 节点互通 |

```bash
go run ./app/agent/cmd/pvn-e2e-check
go run ./app/agent/cmd/pvn-serverless-check
```
