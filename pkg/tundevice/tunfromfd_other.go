//go:build !android

package tundevice

import (
	"fmt"
	"runtime"
)

// NewFromFD 仅在 Android 上可用。其他平台不存在「由外部宿主建立虚拟网卡、
// 本进程只接管 fd」的合法途径：Windows 走 Wintun HANDLE、Linux 走 /dev/net/tun，
// 都由 NewNative 直接建卡并自行配置地址与路由。
//
// 桌面端保留此桩函数有两个用处：让 mobile 门面包能在本机做类型检查与
// 单元测试；把误用变成一条明确的错误信息，而不是编译期平台报错。
func NewFromFD(fd int) (Device, error) {
	return nil, fmt.Errorf("create TUN from fd: 平台 %s 不支持（该能力仅 Android VpnService 场景可用）", runtime.GOOS)
}
