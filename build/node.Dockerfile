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
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/pvn-node ./app/agent/cmd/pvn-node

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S lanet && adduser -S lanet -G lanet \
    && mkdir -p /data && chown lanet:lanet /data

COPY --from=builder /out/pvn-node /usr/local/bin/pvn-node

USER lanet
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
