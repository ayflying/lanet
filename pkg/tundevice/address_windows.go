//go:build windows

package tundevice

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsTUNMTU = 1400

var (
	procInitializeUnicastIPAddressEntry = iphlpapi.NewProc("InitializeUnicastIpAddressEntry")
	procCreateUnicastIPAddressEntry     = iphlpapi.NewProc("CreateUnicastIpAddressEntry")
	procDeleteUnicastIPAddressEntry     = iphlpapi.NewProc("DeleteUnicastIpAddressEntry")
	procInitializeIPInterfaceEntry      = iphlpapi.NewProc("InitializeIpInterfaceEntry")
	procSetIPInterfaceEntry             = iphlpapi.NewProc("SetIpInterfaceEntry")
)

// configureAddressNative configures the Wintun address through the same IP
// Helper API used by native Windows tunnel clients.
func configureAddressNative(name, ipStr string, prefixBits int) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("not ipv4 %q", ipStr)
	}

	ifIndex := uint32(iface.Index)
	var luid uint64
	if ret, _, _ := procConvertIfaceIdxToLuid.Call(
		uintptr(ifIndex),
		uintptr(unsafe.Pointer(&luid)),
	); ret != 0 {
		return fmt.Errorf("ConvertInterfaceIndexToLuid: status 0x%x", ret)
	}

	var deleteRow windows.MibUnicastIpAddressRow
	procInitializeUnicastIPAddressEntry.Call(uintptr(unsafe.Pointer(&deleteRow)))
	deleteRow.InterfaceLuid = luid
	deleteAddress := (*windows.RawSockaddrInet4)(unsafe.Pointer(&deleteRow.Address))
	deleteAddress.Family = windows.AF_INET
	copy(deleteAddress.Addr[:], ip)
	deleteRow.OnLinkPrefixLength = uint8(prefixBits)
	if ret, _, _ := procDeleteUnicastIPAddressEntry.Call(uintptr(unsafe.Pointer(&deleteRow))); ret != 0 && ret != errNotFound && ret != uintptr(windows.ERROR_INVALID_PARAMETER) {
		return fmt.Errorf("DeleteUnicastIpAddressEntry(%s): status 0x%x", ipStr, ret)
	}

	var createRow windows.MibUnicastIpAddressRow
	procInitializeUnicastIPAddressEntry.Call(uintptr(unsafe.Pointer(&createRow)))
	createRow.InterfaceLuid = luid
	createAddress := (*windows.RawSockaddrInet4)(unsafe.Pointer(&createRow.Address))
	createAddress.Family = windows.AF_INET
	copy(createAddress.Addr[:], ip)
	createRow.OnLinkPrefixLength = uint8(prefixBits)
	createRow.DadState = 4 // IpDadStatePreferred
	createRow.ValidLifetime = ^uint32(0)
	createRow.PreferredLifetime = ^uint32(0)
	if ret, _, _ := procCreateUnicastIPAddressEntry.Call(uintptr(unsafe.Pointer(&createRow))); ret != 0 && ret != errObjectAlreadyExists {
		return fmt.Errorf("CreateUnicastIpAddressEntry(%s): status 0x%x", ipStr, ret)
	}
	return nil
}

// configureInterfaceNative sets the kernel-side MTU and metric. The Windows
// wireguard/tun MTU is otherwise only an in-process value.
func configureInterfaceNative(name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	ifIndex := uint32(iface.Index)
	var luid uint64
	if ret, _, _ := procConvertIfaceIdxToLuid.Call(
		uintptr(ifIndex),
		uintptr(unsafe.Pointer(&luid)),
	); ret != 0 {
		return fmt.Errorf("ConvertInterfaceIndexToLuid: status 0x%x", ret)
	}
	var interfaceRow windows.MibIpInterfaceRow
	procInitializeIPInterfaceEntry.Call(uintptr(unsafe.Pointer(&interfaceRow)))
	interfaceRow.Family = windows.AF_INET
	interfaceRow.InterfaceLuid = luid
	if err := windows.GetIpInterfaceEntry(&interfaceRow); err != nil {
		return fmt.Errorf("GetIpInterfaceEntry: %w", err)
	}
	// Some Windows versions return UINT32_MAX here. SetIpInterfaceEntry rejects
	// that read-back value with ERROR_INVALID_PARAMETER.
	if interfaceRow.SitePrefixLength > 32 {
		interfaceRow.SitePrefixLength = 0
	}
	interfaceRow.NlMtu = windowsTUNMTU
	interfaceRow.UseAutomaticMetric = 0
	interfaceRow.Metric = 50
	if ret, _, _ := procSetIPInterfaceEntry.Call(uintptr(unsafe.Pointer(&interfaceRow))); ret != 0 {
		return fmt.Errorf("SetIpInterfaceEntry: status 0x%x", ret)
	}
	return nil
}
