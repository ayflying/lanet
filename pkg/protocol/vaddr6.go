// 虚拟 IPv6 地址方案（lanet 内部 ULA）。
//
// 节点侧 TUN 只接受 fd00:6c61:6e65::/48 内的地址（见 tundevice.ConfigureTUNIPv6），
// 控制面分配与 Standalone 派生都必须落在其中。这里把「前缀 + 群组/成员地址怎么算」
// 收敛成唯一来源：控制面（app/ctl）与节点侧（pkg/tundevice）都引用本文件，
// 避免两处各写一份而悄悄漂移。
//
// 地址形状（与 IPv4 一一对应，排障时一眼能对上）：
//
//	群组：fd00:6c61:6e65:<subnetIndex>::/64   ←→ 10.7.<subnetIndex>.0/24
//	成员：fd00:6c61:6e65:<subnetIndex>::<host> ←→ 10.7.<subnetIndex>.<host>
//
// host 取 2~254，与 IPv4 主机号同值（::0 是子网任播、::1 保留，都不分配），
// 因此每群容量与 IPv4 一致（253 人），且 10.7.7.5 ↔ fd00:6c61:6e65:7::5。
package protocol

import (
	"fmt"
	"net/netip"
)

// LanetULAIPv6Prefix 是 lanet 虚拟 IPv6 的 ULA 前缀：fd00:6c61:6e65::/48
// （6c61:6e65 即 "lane"）。节点 TUN 配地址与成员路由写入都会校验该前缀。
var LanetULAIPv6Prefix = netip.MustParsePrefix("fd00:6c61:6e65::/48")

// GroupIPv6Prefix 返回第 subnetIndex 个群组的虚拟 IPv6 /64：
// fd00:6c61:6e65:<subnetIndex>::/64。控制面里同一个群组同时拥有
// 10.7.<subnetIndex>.0/24 与这个 /64，子网序号是同一个。
//
// /48 有 65536 个 /64，因此可容纳 0~65535；控制面当前只用到 0~255
// （与 IPv4 的 /24 池一致），上限留足余量。
func GroupIPv6Prefix(subnetIndex int) (netip.Prefix, error) {
	if subnetIndex < 0 || subnetIndex > 0xffff {
		return netip.Prefix{}, fmt.Errorf("群组子网序号 %d 超出 %s 可容纳范围（0~65535）", subnetIndex, LanetULAIPv6Prefix)
	}
	raw := LanetULAIPv6Prefix.Addr().As16()
	raw[6] = byte(subnetIndex >> 8) // /48 之后的第一个 16 位组
	raw[7] = byte(subnetIndex)
	return netip.PrefixFrom(netip.AddrFrom16(raw), 64), nil
}

// MemberIPv6 返回某群组内某成员的虚拟 IPv6 /128：
// fd00:6c61:6e65:<subnetIndex>::<host>。host 与 IPv4 主机号同值。
func MemberIPv6(subnetIndex, host int) (netip.Addr, error) {
	prefix, err := GroupIPv6Prefix(subnetIndex)
	if err != nil {
		return netip.Addr{}, err
	}
	if host < 2 || host > 254 {
		return netip.Addr{}, fmt.Errorf("成员主机号 %d 超出可分配范围（2~254，与 IPv4 主机号同值）", host)
	}
	raw := prefix.Addr().As16()
	raw[14] = byte(host >> 8) // 最后一个 16 位组
	raw[15] = byte(host)
	return netip.AddrFrom16(raw), nil
}
