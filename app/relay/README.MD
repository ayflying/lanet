# Lanet 独立中继

`app/relay` 是托管模式使用的 libp2p Circuit Relay v2 服务。它同时监听 TCP、
WebSocket 和 QUIC，为无法直接打洞的 agent、Go SDK 和 Web SDK 节点转发流量。

官方 `lanet` Standalone 节点内置 relay service，通常不需要单独部署本程序。

## 启动

```bash
PVN_RELAY_CTL=http://127.0.0.1:8000 \
PVN_RELAY_REGION=default \
go run ./app/relay
```

默认监听：

```text
/ip4/0.0.0.0/tcp/4001
/ip4/0.0.0.0/tcp/4001/ws
/ip4/0.0.0.0/udp/4001/quic-v1
```

可用 `-listen` 传入逗号分隔的 multiaddr 覆盖监听地址，`-ctl` 覆盖
`PVN_RELAY_CTL`。未配置控制面时 relay 仍会提供转发，但不会进入候选目录。

## 注册与通告

relay 启动后向 `ctl` 注册，成功后每 30 秒发送心跳；心跳失败会重新注册。
首次注册最多重试 30 次，每次间隔 3 秒。`ctl` 会淘汰超过 150 秒未续期的候选。

容器或 NAT 环境应显式设置宿主可达地址：

```bash
PVN_RELAY_ADVERTISE='/ip4/203.0.113.10/tcp/4001,/ip4/203.0.113.10/tcp/4001/ws,/ip4/203.0.113.10/udp/4001/quic-v1' \
PVN_RELAY_CTL='http://ctl:8000' \
go run ./app/relay
```

未设置 `PVN_RELAY_ADVERTISE` 时，程序从实际监听地址中过滤回环和链路本地地址，
再追加 `/p2p/<PeerID>` 后注册。务必放行 `4001/tcp` 与 `4001/udp`。

## 自检

```bash
go run ./app/relay check
```

`check` 在本机创建 relay 与两端节点，验证 Reservation 和经
`/p2p-circuit` 建链，不依赖已部署的控制面。
