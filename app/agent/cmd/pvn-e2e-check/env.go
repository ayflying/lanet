package main

import (
	"os"
	"path/filepath"
	"runtime"
)

func osEnviron() []string {
	return os.Environ()
}

// repoRoot 返回仓库根目录的绝对路径。
// 以本文件所在源码位置为基准向上回溯（app/agent/cmd/pvn-e2e-check → 仓库根），
// 不依赖进程工作目录，避免程序移动后相对路径失效。
func repoRoot() string {
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		// thisFile = <repo>/app/agent/cmd/pvn-e2e-check/env.go，向上五级即仓库根。
		dir := thisFile
		for i := 0; i < 5; i++ {
			dir = filepath.Dir(dir)
		}
		return dir
	}
	return "../../.."
}
