package main

// 单实例锁：同一「配置目录」同时只允许一个 lanet 节点进程。
//
// 为什么必须锁（本机实测到的故障现场）：
//   - 身份 node.key、地址簿 lanet.db、热更状态 state.json、日志 lanet.log 全部
//     锚定在配置目录。双开等于**同一个 PeerID、同一个虚拟 IP** 的两份进程并发
//     读写同一批文件，地址簿与防火墙/转发规则互相覆盖；
//   - TUN 网卡只有一张：两份进程都认为自己管着 "lanet 1"，先退出的那份会在
//     defer 里拆掉网卡与 NRPT 规则，把还在跑的那份的数据面一起带走；
//   - 控制台 8900 被占用时 startConsole 会向后回退（+1…+10，本是为「端口被
//     无关程序占用」设计的容错），于是双开**不报错**，而是静默变成两台
//     「同名同身份」的节点——控制台上凭空多出一个 8901 端口，用户分不清
//     哪一份在真正干活（实测：8900 与 8901 返回完全相同的 peer_id 与虚拟 IP）。
//
// 锁粒度 = 配置目录（不是「全机唯一」）：同一份身份/数据只许一个进程；不同
// -config 目录仍是不同身份、各写各的文件，一台机器上跑多个节点做打洞/中继
// 验证的用法不受影响。
//
// 实现要点：用操作系统的**建议锁**（Windows 字节区间锁 / Unix flock），而不是
// 「文件存在即占用」——建议锁随进程结束（含被强杀）由内核释放，不存在崩溃后
// 残留锁文件导致再也起不来的经典问题。锁文件里的内容只是给后来者看的
// 「谁在跑、控制台在哪」，不参与判重。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// singletonLockName 锁文件名（与 node.key / lanet.db / lanet.log 同目录）。
const singletonLockName = "lanet.lock"

// errSingletonBusy 目标配置目录已有实例在运行。
var errSingletonBusy = errors.New("已有实例在运行")

// singletonInfo 锁文件内容：仅供「被拒绝启动的第二份进程」向用户交代现状。
type singletonInfo struct {
	PID        int    `json:"pid"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	ConsoleURL string `json:"console_url,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
}

// singletonLock 已持有的配置目录独占锁。
type singletonLock struct {
	f         *os.File
	path      string
	pid       int
	startedAt string
}

// activeSingleton 本进程持有的锁，供退出/重启路径提前释放（见 restartSelf）。
var activeSingleton *singletonLock

// singletonLockPath 锁文件与身份/数据库/日志同目录，锚定配置目录的绝对路径
// （服务由 SCM 拉起时 CWD 不是程序目录，相对路径会锚到别处，同 defaultIdentityPath）。
func singletonLockPath(configPath string) string {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, singletonLockName)
}

// acquireSingleton 获取配置目录独占锁。
//
//   - 成功：返回锁实例、nil holder、nil 错误；
//   - 已被占用：返回 nil 锁 + errSingletonBusy，holder 为对方信息（可能为 nil）；
//   - 锁文件本身不可用（权限/文件系统异常）：返回真实 error，调用方应**降级
//     放行**并打警告——单实例是保护性措施，不该因为目录只读之类的问题把节点
//     彻底挡死。
func acquireSingleton(configPath string) (*singletonLock, *singletonInfo, error) {
	path := singletonLockPath(configPath)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("打开单实例锁文件 %s 失败: %w", path, err)
	}
	if err := tryLockFile(f); err != nil {
		if isLockBusy(err) {
			// 锁被别的进程占着：读一把对方信息再放手，供上层拼提示。
			holder := readSingletonInfo(f)
			_ = f.Close()
			return nil, holder, errSingletonBusy
		}
		_ = f.Close()
		return nil, nil, fmt.Errorf("锁定 %s 失败: %w", path, err)
	}
	l := &singletonLock{f: f, path: path, pid: os.Getpid(), startedAt: time.Now().Format(time.RFC3339)}
	l.write(&singletonInfo{PID: l.pid, Version: version, StartedAt: l.startedAt})
	activeSingleton = l
	return l, nil, nil
}

// refresh 补充写入运行期才拿得到的信息（节点名 / 控制台实际地址）。
// 控制台端口可能向后回退，所以必须用节点回报的**实际**地址而非配置值。
func (l *singletonLock) refresh(name, consoleURL string) {
	if l == nil {
		return
	}
	l.write(&singletonInfo{
		PID: l.pid, Name: name, Version: version,
		ConsoleURL: consoleURL, StartedAt: l.startedAt,
	})
}

// release 释放锁；幂等，nil 接收者安全（被拒绝启动的进程也走同一个 defer）。
// 不解锁文件内容：内容只在「锁被占住」时才被读到，而持有者写入的一定是
// 自己的信息，残留内容永远不会被误读。
func (l *singletonLock) release() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
	activeSingleton = nil
}

// write 覆盖写入锁文件内容。失败不影响锁本身，纯信息，故不返回错误。
// 不做截断：内容只增不减（几百字节），原地覆盖 + 空格补齐即可，避免 SetEndOfFile
// 带来的平台差异。锁区刻意放在数据区之外（见 singleton_windows.go），
// 因此别的进程能读到这里的字节，而不会被强制锁挡住。
func (l *singletonLock) write(info *singletonInfo) {
	if l == nil || l.f == nil {
		return
	}
	buf, err := json.Marshal(info)
	if err != nil {
		return
	}
	if st, err := l.f.Stat(); err == nil && int64(len(buf)) < st.Size() {
		buf = append(buf, bytes.Repeat([]byte{' '}, int(st.Size())-len(buf))...)
	}
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return
	}
	_, _ = l.f.Write(buf)
}

// readSingletonInfo 读取对方写的锁文件内容；读不到（空文件/格式不对）返回 nil。
// 内容只是提示，格式不对就当作「不知道」，绝不影响判重结果。
func readSingletonInfo(f *os.File) *singletonInfo {
	if f == nil {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil
	}
	var info singletonInfo
	if err := json.Unmarshal(bytes.TrimSpace(buf), &info); err != nil || info.PID == 0 {
		return nil
	}
	return &info
}

// describeSingletonHolder 把「谁在跑」拼成一句人话。
func describeSingletonHolder(holder *singletonInfo) string {
	if holder == nil {
		return "已有实例在运行（未能读到对方信息）"
	}
	msg := fmt.Sprintf("已有实例在运行 pid=%d", holder.PID)
	if holder.Name != "" {
		msg += " name=" + holder.Name
	}
	if holder.Version != "" {
		msg += " version=" + holder.Version
	}
	if holder.ConsoleURL != "" {
		msg += " 控制台=" + holder.ConsoleURL
	}
	return msg
}

// refuseSecondInstance 第二份实例的收场。
//
// 交互模式（双击 / 命令行）：Windows 下 exe 是 windowsgui 子系统、没有黑框，
// 只写日志用户看不到任何反馈，所以直接把**已在跑那份**的控制台打开给用户看，
// 再以非零码退出；服务模式：写日志后从 runNode 正常返回，让 SCM 干净地停掉
// 这个多余的实例（os.Exit 会让 SCM 只看到「进程没了」）。
func refuseSecondInstance(holder *singletonInfo, serviceMode bool) {
	msg := describeSingletonHolder(holder)
	if serviceMode {
		log.Printf("[service] %s；本服务实例退出（同一配置目录不允许双开）", msg)
		return
	}
	log.Printf("[node] %s；本次启动被拒绝（同一配置目录不允许双开）", msg)
	if holder != nil && holder.ConsoleURL != "" {
		log.Printf("[node] 已为你打开正在运行的实例控制台：%s", holder.ConsoleURL)
		openBrowser(holder.ConsoleURL)
	}
	os.Exit(1)
}
