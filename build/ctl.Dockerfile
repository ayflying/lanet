# =================================================================================
# Lanet 控制面（服务端）镜像。
# 由 .github/workflows/docker.yml 构建并推送 ghcr.io/ayflying/lanet-ctl。
# =================================================================================

FROM golang:1.25-alpine AS builder

WORKDIR /build

# 先拷贝依赖清单利用构建缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/lanet-ctl ./app/ctl

FROM alpine:3.20

# 预建数据目录并赋属主：命名卷首次挂载会继承该目录的属主，
# 避免 bind mount 时容器内非 root 用户无写权限（SQLite 报错 14）。
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S lanet && adduser -S lanet -G lanet \
    && mkdir -p /data && chown -R lanet:lanet /data

COPY --from=builder /out/lanet-ctl /usr/local/bin/lanet-ctl

USER lanet
WORKDIR /data
EXPOSE 8000

ENTRYPOINT ["lanet-ctl"]
