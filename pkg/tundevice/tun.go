// Package tundevice 抽象虚拟网卡：真实 TUN（Windows Wintun / Linux / macOS）与内存 TUN（测试）。
package tundevice

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"golang.zx2c4.com/wireguard/tun"
)

// Device 是 TUN 网卡的最小接口，便于用内存实现做单元测试。
type Device interface {
	// Read 读取一个 IP 包（不含以太帧头）。
	Read(bufs [][]byte, sizes []int, offset int) (n int, err error)
	// Write 向网卡写入 IP 包。
	Write(bufs [][]byte, offset int) (n int, err error)
	MTU() (int, error)
	Name() (string, error)
	Close() error
}

// Events 返回设备事件通道的封装（真实设备才有意义，内存实现返回空通道）。
func Events(d Device) <-chan tun.Event {
	if real, ok := d.(tun.Device); ok {
		return real.Events()
	}
	ch := make(chan tun.Event)
	close(ch)
	return ch
}

// NewNative 创建真实 TUN 设备。Windows 需要 Wintun，且进程需管理员权限。
func NewNative(name string, mtu int) (Device, error) {
	device, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("create TUN %q (Windows 需要 Wintun 与管理员权限): %w", name, err)
	}
	return device, nil
}

// NewMemory 创建内存 TUN 设备，返回设备与注入器。
// 语义（路由器视角）：
//   - Read：取包（真实设备=本机协议栈发出的包）
//   - Write：收包（真实设备=交给本机协议栈的包）
//
// 内存实现里 Read/Write 共用一个通道（回环）：Write 进的包会被 Read 读出，
// inject 与 Write 等价，方便测试从"网络侧"注入包。
func NewMemory(mtu int) (Device, func([]byte) error, error) {
	device := &memoryDevice{
		mtu:     mtu,
		packets: make(chan []byte, 256),
		closed:  make(chan struct{}),
	}
	return device, device.inject, nil
}

type memoryDevice struct {
	mtu     int
	packets chan []byte
	closed  chan struct{}
}

// inject 注入一个包，后续会被 Read 返回。
func (m *memoryDevice) inject(packet []byte) error {
	select {
	case <-m.closed:
		return io.ErrClosedPipe
	case m.packets <- append([]byte(nil), packet...):
		return nil
	}
}

func (m *memoryDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var packet []byte
	select {
	case <-m.closed:
		return 0, io.ErrClosedPipe
	case packet = <-m.packets:
	}
	if len(packet) > len(bufs[0])-offset {
		return 0, fmt.Errorf("packet %d bytes exceeds buffer", len(packet))
	}
	copy(bufs[0][offset:], packet)
	sizes[0] = len(packet)
	return 1, nil
}

func (m *memoryDevice) Write(bufs [][]byte, offset int) (int, error) {
	if err := m.inject(bufs[0][offset:]); err != nil {
		return 0, err
	}
	return 1, nil
}

func (m *memoryDevice) MTU() (int, error)     { return m.mtu, nil }
func (m *memoryDevice) Name() (string, error) { return "pvn-mem0", nil }
func (m *memoryDevice) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

// IsRecoverableReadError 判断 TUN 读取错误是否为「单包级、可忽略」的瞬时错误。
//
// 回归背景（2026-09-14 生产实测）：wireguard-go 的 tun 在解析 virtio 头 /
// GSO 分段时，若单个包的分段数超过其内部上限（MaxSegments=40），会返回
// ErrTooManySegments。这是**某一个包**的问题，下一个包完全可能正常；
// 但 router.Run 的老实现把它当成致命错误直接 return，于是整个 TUN 读循环
// 永久退出——虚拟网数据面彻底停摆，而控制面（libp2p / probe / 控制台状态）
// 完全正常，对外表现为「控制台显示成员在线、直连、rtt 42ms，但 ping 与所有
// TCP 端口全部超时」。Container 里 MTU 1400 时，内核发出的 64KB GSO 包
// 会算出 47 段 > 40，因此该错误会稳定复现。
//
// 判定为可恢复时，调用方应丢弃该包并继续读取，绝不能终止读循环。
func IsRecoverableReadError(err error) bool {
	return errors.Is(err, tun.ErrTooManySegments)
}

// IsDeviceClosed 判断读错误是否表示设备已被关闭（此时应正常退出读循环）。
func IsDeviceClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}
