# =================================================================================
# Lanet Standalone 节点镜像（pvn-node）：无服务器模式长驻节点，
# 用于跨网络联调与部署验证（DHT + mDNS 自动发现，节点即服务端）。
# 由 .github/workflows/docker.yml 构建并推送 ghcr.io/ayflying/lanet-node。
# 身份密钥写在 /data/node.key（建议挂卷持久化，保证虚拟 IP 恒定）。
# =================================================================================

FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION 由 CI 传入（docker.yml build-args），本地构建退回 dev。
# 版本号经 -ldflags 注入 main.version：P2P 自更新的版本比较、
# 成员表版本列、控制台版本显示全部依赖它，缺失会导致节点互不识别版本。
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/pvn-node ./app/agent/cmd/pvn-node

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata iproute2 \
    && addgroup -S lanet && adduser -S lanet -G lanet \
    && mkdir -p /data && chown lanet:lanet /data

COPY --from=builder /out/pvn-node /usr/local/bin/pvn-node

# 保持 root 运行：TUN 虚拟网卡需要 CAP_NET_ADMIN。注意 Docker 的
# --cap-add NET_ADMIN 只对 root 用户生效——若以 USER lanet（非 root）
# 运行，cap 被清空，TUN 创建必报 operation not permitted（实测踩坑）。
# 隔离边界由容器命名卷 /data + 显式 --cap-add 控制，不在镜像内降权。
VOLUME ["/data"]

# 环境变量：LANET_NAME / LANET_NETWORK_KEY / LANET_CONSOLE / LANET_CONSOLE_PASSWORD 等，
# 也可直接用命令行参数覆盖。
# 容器内控制台必须监听 0.0.0.0（宿主端口映射才能到达）；本地双击运行默认仅 127.0.0.1。
# 远程访问时务必设置 LANET_CONSOLE_PASSWORD 启用控制台登录密码。
# 配置文件固定到 /data：lanet.json / state.json / lanet.log 与 node.key 同卷持久化，
# 否则默认落在 /usr/local/bin（非 root 不可写，控制台「节点配置」保存会静默失败）。
ENV LANET_CONSOLE=0.0.0.0:8900
ENV LANET_CONFIG=/data/lanet.json
ENTRYPOINT ["pvn-node"]
