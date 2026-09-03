# =================================================================================
# Lanet 客户端（agent）镜像：内置 Linux Wintun 等价物依赖（/dev/net/tun）。
# 由 .github/workflows/docker.yml 构建并推送 ghcr.io/ayflying/lanet-agent。
# 运行需 NET_ADMIN + /dev/net/tun（见 deploy/docker-compose.agent.yml）。
# =================================================================================

FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/pvn-agent ./app/agent/cmd/pvn-agent

FROM alpine:3.20

# ip 命令用于配置 TUN 网卡地址（configureTUN linux 分支）。
RUN apk add --no-cache ca-certificates tzdata iproute2 \
    && addgroup -S lanet && adduser -S lanet -G lanet

COPY --from=builder /out/pvn-agent /usr/local/bin/pvn-agent

# 注意：实际运行需要 NET_ADMIN 权限（compose 已声明 cap_add），
# 这里保持非 root，由 run 时授权。
USER lanet

# 默认参数全部由环境变量（LANET_AGENT_XXX）或 compose 覆盖。
ENTRYPOINT ["pvn-agent"]
