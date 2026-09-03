# =================================================================================
# Lanet P2P 中继（服务端组件）镜像。
# 由 .github/workflows/docker.yml 构建并推送 ghcr.io/ayflying/lanet-relay。
# =================================================================================

FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/lanet-relay ./app/relay

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S lanet && adduser -S lanet -G lanet

COPY --from=builder /out/lanet-relay /usr/local/bin/lanet-relay

# 中继只做转发，非 root 即可运行。
USER lanet
EXPOSE 4001/tcp 4001/udp

# 默认监听 TCP+UDP 4001，可用 --listen 覆盖。
ENTRYPOINT ["lanet-relay"]
