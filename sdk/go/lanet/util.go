package lanet

import "runtime"

// defaultOS 返回默认操作系统标识。
func defaultOS() string { return runtime.GOOS }
