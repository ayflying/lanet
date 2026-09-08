//go:build windows

package tundevice

import (
	"fmt"
	"net"
	"unsafe"
)

// Windows 邻居表原生写入：netsh 对 L3 设备（Wintun）会静默丢弃链路层地址，
// 留下"永久但无 MAC"的无效条目（包照样卡死在 Unreachable 状态）。
// 必须走 IP Helper API 写入带真实 PhysicalAddress 的 Permanent 条目。
// 结构体布局严格对齐 netioapi.h 的 MIB_IPNET_ROW2（x64）。

const (
	afInet      = 2
	sockAddrLen = 28

	// NL_NEIGHBOR_STATE：0=Unreachable 1=Incomplete 2=Probe 3=Delay
	// 4=Stale 5=Reachable 6=Permanent
	//
	// 关键：只有 Unreachable 会让内核直接丢包。Wintun 是 L3 无 ARP，一旦
	// 内核发起邻居解析必然失败并把条目打成 Unreachable，于是包全部丢在
	// 邻居层（ping 100% 丢、用户态完全读不到包）。
	//
	// Stale 不行：Stale 状态下内核仍会边发包边解析，几秒内就退化成
	// Unreachable（实测 ping 后立刻变 Unreachable）。
	// Permanent 状态下条目不会因 ReachableTime 老化；周期刷新只用于适配器
	// 重建或系统清理邻居项后的自愈。
	nlnsPermanent = 6

	// ERROR_OBJECT_ALREADY_EXISTS：内核已存在该邻居条目（通常是 ARP 失败
	// 留下的 Unreachable/Probe），Create 会失败，必须 Delete 重建或 Set 改写。
	errObjectAlreadyExists = 0x490
	errNotFound            = 1168 // ERROR_NOT_FOUND
)

type sockaddrInet struct {
	Family uint16
	Port   uint16
	Addr   uint32 // 网络序
	Zero   [8]byte
}

// mibIPNetRow2 对齐 netioapi.h MIB_IPNET_ROW2（x64，总 88 字节）。
// 注意：SOCKADDR_INET 占 28 字节后 InterfaceIndex 紧接排布（offset 28），
// InterfaceLuid 自然落到 offset 32（8 字节对齐）——中间没有填充。多插一个
// uint32 会令 Index/Luid/PhysicalAddress/State 全部错位，API 报 0x57/0x490。
type mibIPNetRow2 struct {
	Address            [sockAddrLen]byte // 00..27
	InterfaceIndex     uint32            // 28..31
	InterfaceLuid      uint64            // 32..39
	PhysicalAddress    [32]byte          // 40..71
	PhysicalAddressLen uint32            // 72..75
	State              uint32            // 76..79
	Flags              uint8             // 80
	_                  [3]byte           // 81..83 对齐填充
	ReachabilityTime   uint32            // 84..87
}

var (
	iphlpapi                  = newLazyDLL("iphlpapi.dll")
	procCreateIpNetEntry2     = iphlpapi.NewProc("CreateIpNetEntry2")
	procSetIpNetEntry2        = iphlpapi.NewProc("SetIpNetEntry2")
	procDeleteIpNetEntry2     = iphlpapi.NewProc("DeleteIpNetEntry2")
	procConvertIfaceIdxToLuid = iphlpapi.NewProc("ConvertInterfaceIndexToLuid")
)

// fakeMAC Windows Wintun 邻居假 MAC（不出网，仅内核邻居表占位）。
var fakeMAC = [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}

// ensureNeighborNative 写入 Permanent 假 MAC 邻居条目。
//
// 处理顺序：先 Create；若条目已存在先 Set 更新物理地址，只有 Set 失败时
// 才 Delete 后重建，避免周期刷新时制造邻居解析窗口。内核在有 on-link
// 路由但无 Permanent 条目时会先自行创建 Incomplete/Unreachable 条目，
// 此时 Set 通常会把条目恢复为可发送状态；重建则确保状态从 Permanent 开始。
func ensureNeighborNative(name, ipStr string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	ifIdx := uint32(iface.Index)

	var luid uint64
	ret, _, _ := procConvertIfaceIdxToLuid.Call(
		uintptr(ifIdx),
		uintptr(unsafe.Pointer(&luid)),
	)
	if ret != 0 {
		return fmt.Errorf("ConvertInterfaceIndexToLuid: status 0x%x", ret)
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("bad ip %q", ipStr)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("not ipv4 %q", ipStr)
	}

	var row mibIPNetRow2
	// Address = SOCKADDR_INET (v4)。sin_addr 必须按网络序字节直接写入；不能把
	// BigEndian.Uint32 的数值赋给 uint32 字段，否则在 Windows 小端内存中会反转
	// 成 31.89.7.10（目标 10.7.89.31），API 虽返回成功却写错邻居。
	sa := sockaddrInet{Family: afInet}
	copy((*[4]byte)(unsafe.Pointer(&sa.Addr))[:], ip4)
	copy(row.Address[:], (*[sockAddrLen]byte)(unsafe.Pointer(&sa))[:])
	row.InterfaceIndex = ifIdx
	row.InterfaceLuid = luid
	copy(row.PhysicalAddress[:], fakeMAC[:])
	row.PhysicalAddressLen = uint32(len(fakeMAC))
	row.State = nlnsPermanent

	rowPtr := uintptr(unsafe.Pointer(&row))

	// 1) 创建（条目通常已存在：内核 ARP 留下的 Unreachable/Probe）。
	retCreate, _, _ := procCreateIpNetEntry2.Call(rowPtr)
	if retCreate == 0 {
		return nil
	}
	if retCreate != errObjectAlreadyExists {
		return fmt.Errorf("CreateIpNetEntry2(%s): status 0x%x", ipStr, retCreate)
	}

	// 2) 已有条目先更新物理地址，避免删除期间包丢失。
	if ret, _, _ := procSetIpNetEntry2.Call(rowPtr); ret == 0 {
		return nil
	}

	// 3) 只有更新失败才强制重建，处理内核残留的不可更新条目。
	if ret, _, _ := procDeleteIpNetEntry2.Call(rowPtr); ret != 0 && ret != errNotFound {
		return fmt.Errorf("DeleteIpNetEntry2(%s): status 0x%x", ipStr, ret)
	}
	if ret, _, _ := procCreateIpNetEntry2.Call(rowPtr); ret != 0 && ret != errObjectAlreadyExists {
		return fmt.Errorf("CreateIpNetEntry2(%s) after delete: status 0x%x", ipStr, ret)
	}
	if ret, _, _ := procSetIpNetEntry2.Call(rowPtr); ret != 0 {
		return fmt.Errorf("SetIpNetEntry2(%s): status 0x%x", ipStr, ret)
	}
	return nil
}
