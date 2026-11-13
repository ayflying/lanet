# Lanet 容器镜像

**Lanet** 将不同网络中的设备接入同一张加密的 P2P 虚拟局域网，优先打洞直连，必要时通过中继兜底。官方 Standalone 节点无需部署中心控制服务器。

- [GitHub 源码与完整说明](https://github.com/ayflying/lanet)
- [当前版本](https://github.com/ayflying/lanet/blob/main/VERSION)
- [发行包](https://github.com/ayflying/lanet/releases)

## 官方 P2P 镜像

官方只发布 **`lanet-node`** 一个 P2P 镜像，同一个程序兼具普通节点、引导和中继能力，内置 Web 控制台，无需另行部署控制面或独立中继。支持 **linux/amd64** 和 **linux/arm64**，同步发布到 Docker Hub 与 GHCR。

| Docker Hub | 对应 GHCR 镜像 | 用途 |
|---|---|---|
| [ayflying/lanet-node](https://hub.docker.com/r/ayflying/lanet-node) | `ghcr.io/ayflying/lanet-node` | Standalone P2P 节点，内置 Web 控制台，推荐用于自建节点 |

两个镜像仓库接收同一次构建的相同标签：分支名、语义化版本（完整版本、主次版本、主版本）、根目录 `VERSION` 与短 Git SHA。`latest` 显式跟随 `main`；语义化版本发布也保留 metadata-action 自动生成的 `latest`。生产部署建议固定版本标签。

```bash
docker pull ayflying/lanet-node:latest
# GHCR 等价镜像
docker pull ghcr.io/ayflying/lanet-node:latest
```

## 运行 Standalone 节点

使用项目维护的 [Docker Compose 配置](https://github.com/ayflying/lanet/blob/main/docker-compose.yml)，并参考 [Linux 容器部署说明](https://github.com/ayflying/lanet/blob/main/README.md#公网节点容器)。Compose 默认使用 Docker Hub 镜像 `docker.io/ayflying/lanet-node:latest`，无需再替换地址；也可按需固定版本标签。

```bash
git clone https://github.com/ayflying/lanet.git
cd lanet
# 修改 docker-compose.yml：镜像地址、节点名称、网络密钥和控制台密码
docker compose up -d
docker compose logs -f node
```

- 需要互连的设备使用相同网络密钥。
- 持久化 `/data`，保留节点身份、配置和本地地址簿。
- 提供的 Compose 配置在 Linux 上使用 host 网络；公网节点放行 `4001/tcp` 与 `4001/udp`。
- TUN 需要 `/dev/net/tun` 与 `NET_ADMIN` 权限，Compose 已包含这些设置；宿主机必要时加载 `tun` 内核模块。
- 将 Web 控制台 `8900` 端口对外开放前，务必设置强 `LANET_CONSOLE_PASSWORD`，并通过宿主机防火墙限制访问。
- 默认需要连接审批。仅在明确接受自动信任风险时，为无人值守节点启用 `LANET_AUTO_ACCEPT`。

## 发布与支持

[容器发布工作流](https://github.com/ayflying/lanet/blob/main/.github/workflows/docker.yml) 在稳定性测试通过后构建并推送镜像。独立任务将本说明同步到 Docker Hub 的 `ayflying/lanet-node` 仓库；说明同步不阻塞镜像构建，文档变更也会触发同步。

问题反馈请前往 [GitHub Issues](https://github.com/ayflying/lanet/issues)。最新配置、安全边界及排障信息以 [项目说明](https://github.com/ayflying/lanet/blob/main/README.md) 为准。
