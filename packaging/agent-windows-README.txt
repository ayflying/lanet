Lanet Windows 版
================

Lanet 是无中心服务器的 P2P 虚拟局域网。使用相同网络密钥的设备自动发现，
优先打洞直连，失败时经网络内可达成员中继。

【文件】
  lanet.exe    主程序（客户端、DHT server、relay service 一体）
  wintun.dll   Wintun 驱动，必须与 lanet.exe 同目录
  VERSION      当前版本
  manifest.json  P2P 更新签名清单（签名版发行包提供）
  README.txt   本文件

【系统要求】
  - Windows 10 1809+ 或 Windows Server 2019+，x64
  - 接受启动时的 UAC 管理员授权（创建和配置 TUN 虚拟网卡需要）

【首次使用】
  1. 完整解压发行包，直接双击 lanet.exe，在 UAC 提示中选择“是”。程序已
     内置管理员清单，不需要右键“以管理员身份运行”。
  2. 首次启动会自动打开 http://127.0.0.1:8900。
  3. 在“节点配置”填写节点名称和网络密钥，保存后通过页面重启或退出再启动。
  4. 其他机器使用相同网络密钥启动；成员出现后，可按 10.7.x.x 虚拟 IP
     ping 或访问 TCP/UDP 服务。

只有首次生成配置时自动打开浏览器。以后从系统托盘菜单打开控制台或退出。

【程序生成的文件】
  lanet.json  节点配置，保存后重启生效
  node.key    Ed25519 身份，决定 PeerID 和稳定虚拟 IP，务必保留
  state.json  防火墙与局域网端口转发配置，保存后立即生效
  lanet.log   运行日志、成员发现和链路探测结果

默认情况下这些文件与 lanet.exe 位于同一目录，可随整个目录迁移。不要把同一份
node.key 同时复制给多个在线节点。

【Web 控制台】
  - 成员与链路：名称、<节点名>.lanet、虚拟 IP、版本、直连/中继状态；
  - 防火墙：deny-all、allow-list、allow-all 及来源/协议/端口规则；
  - 局域网转发：把本机或同一物理局域网内的服务暴露给同网络成员；
  - 节点配置：网络密钥、引导方式、监听地址、控制台、TUN 等；
  - 程序操作：检查更新、重启和退出。

控制台默认仅监听 127.0.0.1:8900。如改为 0.0.0.0:8900，必须同时设置强密码，
并在 Windows 防火墙中限制访问来源。登录会话有效期为 7 天。

【常用命令行参数】
  -config            配置文件路径，默认程序目录 lanet.json
  -name              节点名称
  -key               网络密钥；留空加入官方公共网络
  -bootstrap         public（默认）/ none / 逗号分隔的成员 multiaddr
  -no-public-dht     关闭公共 DHT 兜底
  -listen            逗号分隔的 libp2p 监听地址
  -console           控制台地址，传 - 关闭
  -console-password  控制台密码
  -fw                deny-all / allow-list / allow-all（官方程序默认 allow-all）
  -tun               true / false，默认 true
  -probe             成员链路探测间隔，如 5s

示例：
  .\lanet.exe -name pc1 -key "our-network-key"
  .\lanet.exe -name pc2 -key "our-network-key"

【发现、路由和防火墙】
  - public 引导使用公共 DHT 完成跨网冷启动；none 仅使用局域网 mDNS。
  - Windows 会为每个成员维护 /32 on-link 路由和邻居项。日志出现“TUN 网卡
    lanet 已就绪”后，虚拟 IP 才能由系统应用直接使用。
  - TUN 初始化失败会自动降级；应用协议和端口转发仍可用，原因写入 lanet.log。
  - ICMP 没有端口。deny-all 或未放行相应协议的 allow-list 会导致 ping 不通。

【更新】
  官方裸机程序自动参与签名 P2P 更新：只接受内置 Ed25519 公钥验证通过、且
  SHA-256 一致的新版本。下载完成后会随机延迟 1 到 8 分钟重启。容器和 dev
  构建不启用此机制。

完整文档：https://github.com/ayflying/lanet
