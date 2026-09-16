//go:build android

package tundevice

import (
	"fmt"
	"log"

	"golang.zx2c4.com/wireguard/tun"
)

// NewFromFD 用宿主已建立的虚拟网卡文件描述符创建 TUN 设备（Android 专用）。
//
// 场景：Android 上非 root 进程无法自行打开 /dev/net/tun，唯一合法途径是
// 由 VpnService.establish() 建立虚拟网卡，再把 ParcelFileDescriptor 的 fd
// 交给本进程接管（wireguard-android 同款做法）。因此这里**不做建卡动作**，
// 也不配置地址与路由——那两件事已由 VpnService.Builder 在 Java 侧完成：
// pkg/tundevice 的 configureAddressNative / ensureRouteNative 在非 Windows
// 平台本就是空实现，语义天然吻合。
//
// fd 所有权：调用方交出 fd 后不得再 close，设备生命周期由返回的 Device 接管
// （Device.Close 会关闭底层 fd）。
func NewFromFD(fd int) (Device, error) {
	if fd < 0 {
		return nil, fmt.Errorf("create TUN from fd: 非法描述符 %d", fd)
	}
	device, name, err := tun.CreateUnmonitoredTUNFromFD(fd)
	if err != nil {
		return nil, fmt.Errorf("create TUN from fd %d: %w", fd, err)
	}
	// 网卡名与 fd 必须落日志：Android 上「授权成功但数据面不通」的第一现场
	// 就是这里——fd 拿到了、名字为空或异常，说明 VpnService 侧建卡没成功。
	log.Printf("[tun] 已接管宿主虚拟网卡 %s（fd=%d）", name, fd)
	return device, nil
}
