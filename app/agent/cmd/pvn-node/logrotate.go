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
	inBackup   bool                       // 重建失败时暂时写入 .1，不再移动该备份。
	warn       func(string)               // 轮转出错时的告警回调；nil 时静默
	renameHook func(string, string) error // 测试可注入改名失败；nil 时用 os.Rename
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
	if r.warn == nil {
		// 不经全局 log 输出，避免其输出恰好是当前 writer 时重入死锁。
		r.warn = func(s string) { _, _ = fmt.Fprintln(os.Stderr, s) }
	}
	// 上次退出时可能已超限（或历史遗留的巨型日志）：先轮转再写。
	if r.size >= r.maxSize {
		if err := r.rotateLocked(); err != nil {
			return nil, err
		}
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
	if len(p) == 0 {
		return 0, nil
	}
	if r.size > 0 && int64(len(p)) > r.maxSize-r.size {
		if err := r.rotateLocked(); err != nil {
			// 轮转彻底失败（改名与回退重开都失败）：保持「日志不丢」的底线——
			// 不截断、绝不解引用 nil 句柄 panic，以 ErrClosed 显式暴露，交由调用方决定。
			if r.f == nil {
				return 0, os.ErrClosed
			}
			r.warnf("logrotate: 轮转失败，退回当前文件继续写: %v", err)
		}
	}
	if r.f == nil {
		return 0, os.ErrClosed
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

// warnf 触发轮转告警（回调为 nil 时静默）。
func (r *rotatingFile) warnf(format string, args ...interface{}) {
	if r.warn != nil {
		r.warn(fmt.Sprintf(format, args...))
	}
}

// doRename 执行「当前日志 → .1」改名；测试可注入失败以验证失败回退路径。
func (r *rotatingFile) doRename(old, new string) error {
	if r.renameHook != nil {
		return r.renameHook(old, new)
	}
	return os.Rename(old, new)
}

// rotateLocked 执行一次轮转，调用方须持锁。
//
// 失败安全原则（长期运行审计 item 7）：任何一步失败都「绝不截断仍在用的日志」。
//   - 改名失败（磁盘满/权限/被占用）：以追加方式重新打开原路径继续写，旧日志不丢；
//   - 改名成功但重建新文件失败（极少）：退回以追加方式复用已改名的 .1，旧日志仍在；
//   - 仅当两步都失败时才返回错误，此时 r.f 为 nil，后续 Write 会报 ErrClosed
//     而非静默丢日志。
func (r *rotatingFile) rotateLocked() error {
	// 上次重建失败时可能仍写在 .1；不可把仍在使用的备份移位或删除。
	if r.f != nil && r.inBackup {
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil // 原路径仍不可用，继续保留备份句柄。
		}
		_ = r.f.Close()
		r.f = f
		r.inBackup = false
		r.size = 0
		if st, err := f.Stat(); err == nil {
			r.size = st.Size()
		}
		return nil
	}
	// 改名前必须先关闭当前句柄（Windows 下被占用则无法改名）。
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
	if err := r.doRename(r.path, r.path+".1"); err != nil {
		// 改名失败：不截断原日志，退回追加写原文件，保证日志不丢。
		r.warnf("logrotate: 轮转改名失败（%v），继续写入当前文件", err)
		reopen, oerr := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if oerr != nil {
			return fmt.Errorf("logrotate: 改名失败且无法回退打开原文件: %w", err)
		}
		if st, serr := reopen.Stat(); serr == nil {
			r.size = st.Size()
		}
		r.f = reopen
		return nil
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		// 改名已成功但重建新文件失败（极少）：退回以追加方式复用 .1（含全部旧日志）。
		r.warnf("logrotate: 轮转重建新文件失败（%v），继续写入备份 .1", err)
		alt, aerr := os.OpenFile(r.path+".1", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if aerr != nil {
			return fmt.Errorf("logrotate: 重建新文件失败且无可用句柄: %w", err)
		}
		if st, serr := alt.Stat(); serr == nil {
			r.size = st.Size()
		}
		r.f = alt
		r.inBackup = true
		return nil
	}
	r.f = f
	r.size = 0
	return nil
}
