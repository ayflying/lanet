//go:build windows

package tundevice

import "syscall"

// lazyProcPair 简化 syscall.LazyProc 的类型引用。
type lazyProcPair = syscall.LazyProc

func newLazyDLL(name string) *syscall.LazyDLL {
	return syscall.NewLazyDLL(name)
}
