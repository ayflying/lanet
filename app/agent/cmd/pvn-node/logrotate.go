// 日志文件按大小轮转。
//
// 背景：lanet.log 原先只追加不轮转。节点默认每 5s 一条 probe 日志，
// 长期运行会持续膨胀——现场见过容器内 738MB / 本机 78MB，叠加在
// 磁盘告急的机器上会进一步挤压空间。这里给出一个零依赖的按大小轮转
// 写入器：超过阈值就把 lanet.log 依次改名为 lanet.log.1 / .2 / …，
// 只保留最近 maxBackups 份，最旧的一份直接删除。
package main

import (
	"fmt"
	"os"
	"sync"
)

// 日志轮转默认参数：单文件 10MB、保留 3 份备份（最多约 40MB）。
const (
	logMaxSize    = 10 << 20 // 10 MiB
	logMaxBackups = 3
)

// rotatingFile 是一个按大小轮转的文件写入器。
// 并发安全：log 包本身对单条日志串行化，但轮转可能被显式调用，故加锁。
type rotatingFile struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	f          *os.File
	size       int64
}

// newRotatingFile 打开（或创建）日志文件；若当前文件已超过阈值，
// 立即先轮转一次，避免「一启动就写爆」。
func newRotatingFile(path string, maxSize int64, maxBackups int) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var size int64
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}
	r := &rotatingFile{
		path:       path,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		f:          f,
		size:       size,
	}
	// 上次退出时可能已超限（或历史遗留的巨型日志）：先轮转再写。
	if r.size >= r.maxSize {
		_ = r.rotateLocked()
	}
	return r, nil
}

// Write 实现 io.Writer。写入前检查是否会越过阈值；越过则先轮转。
// 轮转失败不阻断写入——日志宁可能继续写旧文件，也不能把程序日志打挂。
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		_ = r.rotateLocked()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close 关闭底层文件。
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// rotateLocked 执行一次轮转，调用方须持锁。
func (r *rotatingFile) rotateLocked() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	// 最旧一份直接丢弃（保留 .1 … .maxBackups）。
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.maxBackups))
	for i := r.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", r.path, i)
		dst := fmt.Sprintf("%s.%d", r.path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	_ = os.Rename(r.path, r.path+".1")

	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}