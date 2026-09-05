Lanet 客户端（Windows）

【文件说明】
  lanet-agent-windows-amd64.exe  客户端主程序
  wintun.dll                     虚拟网卡驱动（必须与 exe 同目录）
  README.txt                     本文件

【运行要求】
  1. Windows 10 1809+ / Windows Server 2019+（x64）
  2. 必须以管理员身份运行（Wintun 网卡需要）
  3. wintun.dll 必须与 exe 放在同一目录

【使用方法（管理员 PowerShell / CMD）】

  # 创建群组（第一个节点）
  .\lanet-agent-windows-amd64.exe -ctl http://<服务端IP>:8000 -mode create -name my-pc -group "我的局域网"

  # 凭邀请码加入（其他节点）
  .\lanet-agent-windows-amd64.exe -ctl http://<服务端IP>:8000 -mode join -invite <邀请码> -name pc2

  # 常用参数
  #   -ctl       控制面地址（默认 http://127.0.0.1:8000）
  #   -mode      create=创建群组 / join=凭邀请加入（默认 join）
  #   -invite    邀请码（join 模式必填）
  #   -name      节点名称（必填）
  #   -group     群组名称（create 模式，默认 default）
  #   -tun       TUN 网卡名（默认 pvn0）
  #   -mtu       TUN MTU（默认 1400）
  #   -real-tun  Windows 恒为 true（真实网卡），无需显式传

【验证】
  入网成功后日志会打印虚拟 IP（如 10.7.0.2），
  同群成员可用该 IP 互相 ping / 访问服务。

【提示】
  首次运行 Wintun 会创建虚拟网卡，可能弹出驱动安装确认。
