//go:build !windows

package tundevice

import "errors"

// ensureNeighborNative 非 Windows 平台的桩实现。
// 邻居表写入是 Windows/Wintun（L3 无 ARP）专属处理，Linux/macOS 内核把
// TUN 当点对点链路，无需邻居条目。这里只为满足跨平台的符号引用。
func ensureNeighborNative(_ string, ipStr string) error {
	return errors.New("neighbor entry is windows-only")
}
