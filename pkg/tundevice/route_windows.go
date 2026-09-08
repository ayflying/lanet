//go:build windows

package tundevice

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procInitializeIpForwardEntry = iphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateIpForwardEntry2    = iphlpapi.NewProc("CreateIpForwardEntry2")
)

func ensureRouteNative(name, ipStr string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("not ipv4 %q", ipStr)
	}

	var row windows.MibIpForwardRow2
	procInitializeIpForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
	if ret, _, _ := procConvertIfaceIdxToLuid.Call(
		uintptr(iface.Index),
		uintptr(unsafe.Pointer(&row.InterfaceLuid)),
	); ret != 0 {
		return fmt.Errorf("ConvertInterfaceIndexToLuid: status 0x%x", ret)
	}

	destination := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix))
	destination.Family = windows.AF_INET
	copy(destination.Addr[:], ip)
	row.DestinationPrefix.PrefixLength = 32

	nextHop := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.NextHop))
	nextHop.Family = windows.AF_INET
	row.Metric = 0

	if ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row))); ret != 0 && ret != errObjectAlreadyExists {
		return fmt.Errorf("CreateIpForwardEntry2(%s): status 0x%x", ipStr, ret)
	}
	return nil
}
