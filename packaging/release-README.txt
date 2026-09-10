Lanet
=====

Lanet 是无中心服务器的 P2P 虚拟局域网。使用相同网络密钥的设备经 mDNS 与
DHT 自动发现，优先打洞直连，失败时经网络内可达成员的 Circuit Relay v2 中继。

【发行包内容】
  lanet / lanet.exe  单一主程序
  VERSION            当前版本
  manifest.json      P2P 更新签名清单（签名版发行包提供）
  README.txt         本文件
  wintun.dll         仅 Windows 包附带，必须与 lanet.exe 同目录

【Windows】
  1. 支持 Windows 10 1809+ / Windows Server 2019+ x64。
  2. 双击 lanet.exe 并接受 UAC 提示。程序内置管理员清单，不需要右键提权。
  3. 首次启动自动打开 http://127.0.0.1:8900，在“节点配置”填写名称和网络
     密钥，保存后重启。
  4. 以后通过系统托盘打开控制台或退出。只有首次生成配置时自动打开浏览器。

【Linux】
  1. TUN 需要 root，或 /dev/net/tun + CAP_NET_ADMIN：
       sudo ./lanet -name server-1 -key 'our-network-key'
  2. 默认在可执行文件目录读写 lanet.json、state.json 和 lanet.log；身份文件
     默认是 /data/node.key。请保证路径可写并持久化。
  3. Linux 没有托盘和自动打开浏览器，控制台默认仍为 127.0.0.1:8900。

【网络与安全】
  - 相同非空网络密钥组成私有网络；密钥留空会加入官方公共网络。
  - 默认启用 TUN 和 allow-all，组内成员可按 10.7.x.x 访问本机。生产使用请
    根据边界在 Web 控制台配置 deny-all 或 allow-list。
  - 控制台远程开放为 0.0.0.0:8900 时，必须同时设置强密码并限制防火墙来源。
  - node.key 决定 PeerID 与稳定虚拟 IP，应备份，但不能由多个在线节点共用。
  - TUN 创建失败时自动降级，应用流和端口转发仍能继续工作；查看 lanet.log。

【常用参数】
  -config            配置文件路径
  -name              节点名称
  -key               网络密钥
  -bootstrap         none / 成员连接种子 multiaddr（推荐，纯私有无公共流量）
  -public-dht        启用公共 DHT 临时引导（默认关闭以省流量）
  -public-dht-minutes 公共 DHT 最长运行分钟数（默认 10，连上同群成员或超时自动退出）
  -listen            逗号分隔的 libp2p 监听地址
  -console           控制台地址，传 - 关闭
  -console-password  控制台密码
  -fw                deny-all / allow-list / allow-all
  -tun               true / false

【更新】
  官方裸机程序会从同网络、同平台成员获取 Ed25519 签名的新版本清单，验签并
  校验 SHA-256 后更新。容器和 dev 构建自动禁用，容器请通过镜像编排升级。

完整文档：https://github.com/ayflying/lanet
