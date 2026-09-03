// Package tundevice 抽象虚拟网卡：真实 TUN（Windows Wintun / Linux / macOS）与内存 TUN（测试）。
package tundevice

import (
	"fmt"
	"io"

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
