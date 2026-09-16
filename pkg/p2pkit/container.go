// 容器环境下的地址治理：识别「本机在容器里」并剔除容器内网地址，
// 同时支持用 LANET_ADVERTISE 显式声明「外界真正能拨到的地址」。
//
// 为什么需要：容器（docker 默认 bridge 等非 host 网络）里的网卡名是 eth0，
// 而宿主机上容器网桥的网卡名是 docker0 / br-* —— reachability.go 里那套
// 「按网卡名识别虚拟网卡」的判据（hostVirtualIfaceHints）在容器内**完全不命中**。
// 于是容器自报的 docker 内网地址（如 192.168.64.2）会按 RFC1918 拿到
// RankPrivateIPv4 这一高位等级，被写进连接种子 / 连接码 / 地址簿，
// 再被同群所有节点学走。
//
// 真机实证（VPS stack 61 的 lanet 容器）：它把 192.168.64.2:4001 当成自己的
// 对外地址发布，其它节点冷启动预热时反复拨它——该网段在宿主与局域网上都
// 不可达，每次都超时，日志可见 `冷启动预热完成：已连上 0，未连上 3`，
// 每轮空耗 30~45 秒。所以这类地址必须**从分享列表里直接剔除**，而不是仅降级：
// 降级过的地址在名额有余量时仍会被分享，脏地址照样扩散。
//
// 要对外发布容器内的服务，正确做法是宿主机做端口映射之后，用 LANET_ADVERTISE
// 声明「外界可达的那个地址」，而不是让容器自报 docker 内网地址。
package p2pkit

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	ma "github.com/multiformats/go-multiaddr"
)

// envContainer / envAdvertise 环境变量名。
const (
	// envContainer 显式覆盖容器判定：1/true 强制按容器处理，0/false 强制不按
	// 容器处理。容器检测是启发式的，host 网络模式等场景需要逃生口——
	// docker 无论网络模式都会创建 /.dockerenv，光靠文件判不出来。
	envContainer = "LANET_CONTAINER"
	// envAdvertise 显式声明的对外可达地址，逗号分隔。每项可以是
	// "IP:端口"（同时展开 TCP 与 QUIC 两个地址）或完整 multiaddr
	// （以 / 开头，原样采用）。典型用途：容器经宿主端口映射后，
	// 声明宿主那侧的 ip:port。
	envAdvertise = "LANET_ADVERTISE"
)

// maxAdvertise 单次接受的自声明地址条目上限（防止误配一长串把种子名额挤空）。
const maxAdvertise = 8

var (
	containerOnce sync.Once
	containerVal  bool

	advertiseOnce sync.Once
	advertiseList []ma.Multiaddr
)

// InContainer 判断本机是否运行在容器内。结果进程内缓存（容器身份不会变）。
//
// 判据依次为：
//  1. LANET_CONTAINER 显式指定（最高优先级，供 host 网络模式等场景纠正）；
//  2. /.dockerenv 存在（docker 会无条件创建，与网络模式无关）；
//  3. /proc/1/cgroup 或 /proc/self/cgroup 含 docker / kubepods / containerd /
//     lxc / podman 关键字（老 cgroup v1 与 v2 下的常见形态）。
//
// 都不命中即视为裸机——误判成裸机的代价是「多分享几条无用地址」，
// 远小于误判成容器时「把真实局域网地址全部剔除」，故判据偏保守。
func InContainer() bool {
	containerOnce.Do(func() {
		containerVal = detectContainer()
	})
	return containerVal
}

func detectContainer() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envContainer))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	for _, p := range []string{"/proc/1/cgroup", "/proc/self/cgroup"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if cgroupSaysContainer(string(b)) {
			return true
		}
	}
	return false
}

// containerCgroupHints cgroup 文件里出现即判定为容器运行时的关键字。
//
// 只收「运行时名」而刻意不收通用词：kubepods 覆盖 k8s，containerd / lxc / podman
// 覆盖各自运行时；libpod 是 podman 实际的 cgroup 目录名（真机上形如
// `libpod-<id>.scope` / `/libpod_parent/…`），只写 podman 会漏判。
// 真机上的 cgroup 形如 `0::/system.slice/docker-<id>.scope`（cgroup v2）或
// `1:name=systemd:/docker/<id>`（cgroup v1）。
var containerCgroupHints = []string{"docker", "kubepods", "containerd", "lxc", "podman", "libpod"}

// cgroupSaysContainer 在 cgroup 文件内容里做关键字匹配（独立成函数便于单测）。
func cgroupSaysContainer(content string) bool {
	lower := strings.ToLower(content)
	for _, hint := range containerCgroupHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// AdvertiseAddrs 返回 LANET_ADVERTISE 声明的对外可达地址（进程内缓存，未配置时为空）。
//
// 这些地址会被顶到分享列表最前面——名额有限（连接种子 / 连接码会截断），
// 显式声明的地址永远比自动枚举出来的更可信。
func AdvertiseAddrs() []ma.Multiaddr {
	advertiseOnce.Do(func() {
		advertiseList = parseAdvertiseSpec(os.Getenv(envAdvertise))
	})
	return advertiseList
}

// parseAdvertiseSpec 解析 LANET_ADVERTISE 的值：逗号分隔，每项两种形式。
//   - "1.2.3.4:4001" / "[2408:824e::1]:4001"：展开成 TCP 与 QUIC 两个地址
//     （一个 ip:port 派生两条，与连接码的做法一致）；
//   - "/ip4/1.2.3.4/tcp/4001"：完整 multiaddr，原样采用。
//
// 非法项跳过而不报错：这是运行期配置，一条写错不该让整个节点起不来。
func parseAdvertiseSpec(spec string) []ma.Multiaddr {
	var out []ma.Multiaddr
	for _, raw := range strings.Split(spec, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if strings.HasPrefix(item, "/") {
			if a, err := ma.NewMultiaddr(item); err == nil {
				out = append(out, a)
			}
			continue
		}
		host, port, err := net.SplitHostPort(item)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		out = append(out, hostPortAddrs(ip, port)...)
		if len(out) >= maxAdvertise {
			break
		}
	}
	if len(out) > maxAdvertise {
		out = out[:maxAdvertise]
	}
	return out
}

// hostPortAddrs 由「IP + 端口」派生 TCP 与 QUIC 两个 multiaddr。
// IP 文本经 netip 规范化（IPv6 压缩写法、v4-mapped 归一为 v4）。
func hostPortAddrs(ip netip.Addr, port string) []ma.Multiaddr {
	proto := "ip4"
	if ip.Is6() && !ip.Is4In6() {
		proto = "ip6"
	}
	out := make([]ma.Multiaddr, 0, 2)
	if a, err := ma.NewMultiaddr("/" + proto + "/" + ip.String() + "/tcp/" + port); err == nil {
		out = append(out, a)
	}
	if a, err := ma.NewMultiaddr("/" + proto + "/" + ip.String() + "/udp/" + port + "/quic-v1"); err == nil {
		out = append(out, a)
	}
	return out
}

// collectContainerInternalAddrs 收集「容器内网地址」：本机在容器里时，
// 所有启用中、非回环网卡上的私网地址（RFC1918 / ULA）。
//
// 只收私网：容器上若有公网 IPv6（或 host 网络下的公网 IPv4），那是真正
// 可路由的地址，不该剔除。链路本地不在此列——它本来就被排到最后。
func collectContainerInternalAddrs() map[string]bool {
	out := map[string]bool{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			if ip = ip.Unmap(); ip.IsPrivate() {
				out[ip.String()] = true
			}
		}
	}
	return out
}
