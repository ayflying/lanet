package main

// 单实例锁的回归防护。
//
// 背景：控制台在 8900 被占用时会向后回退端口（本是为「端口被无关程序占用」
// 设计的容错），于是同一配置目录双开**不报错**，而是静默变成两台
//「同 PeerID / 同虚拟 IP」的节点——实测 8900 与 8901 返回完全相同的
// peer_id，两个进程并发写同一份 lanet.db / state.json / lanet.log，
// 还共用同一张 TUN 网卡与 NRPT 规则。这里把「同目录第二份必须被拒、
// 不同目录仍可并存、持有者退出后可以重启」三件事固定下来。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// singletonChildEnv 子进程占锁模式的开关（值 = 要占住的配置目录）。
const singletonChildEnv = "LANET_SINGLETON_CHILD_DIR"

// 锁文件必须与 node.key / lanet.db / lanet.log 同锚点，且是绝对路径。
// 相对路径会随 CWD 解析——服务由 SCM 拉起时 CWD=System32，判重就会失效。
func TestSingletonLockPathAnchoredToConfigDir(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "lanet.json")
	got := singletonLockPath(cfg)
	if !filepath.IsAbs(got) {
		t.Fatalf("锁文件路径必须是绝对路径（不依赖 CWD），实际 %q", got)
	}
	if want := filepath.Join(filepath.Dir(cfg), singletonLockName); got != want {
		t.Fatalf("锁文件应锚定配置目录：want %q, got %q", want, got)
	}
}

// 真·跨进程判重：用测试二进制自身再起一个进程占住锁，验证同一个配置目录的
// 第二份实例被拒（且能读到持有者信息）、不同配置目录不受影响、持有者退出后
// 可以重新启动。
//
// 之所以要另起进程而不是在本进程里开第二个句柄：判重的语义是「两个进程」，
// 同一进程内重复加锁的行为各平台不一致（Windows 字节区间锁对同句柄放行），
// 只有跨进程才测得出真实行为。
func TestSingletonSecondInstanceRejected(t *testing.T) {
	if dir := os.Getenv(singletonChildEnv); dir != "" {
		// ---- 子进程分支：占住锁并一直不放手，直到父进程关掉 stdin ----
		if _, _, err := acquireSingleton(filepath.Join(dir, "lanet.json")); err != nil {
			fmt.Fprintf(os.Stderr, "子进程获取锁失败: %v\n", err)
			os.Exit(2)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}

	dir := t.TempDir()
	cfg := filepath.Join(dir, "lanet.json")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSingletonSecondInstanceRejected")
	child.Env = append(os.Environ(), singletonChildEnv+"="+dir)
	child.Stderr = os.Stderr
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("创建子进程 stdin 失败: %v", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("创建子进程 stdout 失败: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("启动占锁子进程失败: %v", err)
	}
	waited := false
	waitChild := func() {
		if waited {
			return
		}
		waited = true
		_ = stdin.Close()
		_ = child.Wait()
	}
	defer waitChild()

	// 等子进程明确报出「已占锁」再往下走，避免父进程抢在子进程之前加锁。
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("占锁子进程未就绪就退出了（读到 %q, err=%v）", line, err)
		}
		if strings.Contains(line, "locked") {
			break
		}
	}

	// ① 同一配置目录：第二份实例必须被拒绝，并带上持有者信息（供提示用）。
	lock, holder, err := acquireSingleton(cfg)
	if lock != nil {
		lock.release()
	}
	if !errors.Is(err, errSingletonBusy) {
		t.Fatalf("同配置目录的第二份实例应被拒绝，实际 lock=%v err=%v", lock, err)
	}
	if holder == nil || holder.PID == 0 {
		t.Fatalf("应能读到持有者 pid，实际 %+v", holder)
	}
	if holder.PID == os.Getpid() {
		t.Fatalf("持有者 pid 不应是本进程：%d", holder.PID)
	}
	if got := describeSingletonHolder(holder); !strings.Contains(got, "已有实例在运行") {
		t.Fatalf("提示文案异常: %q", got)
	}

	// ② 不同配置目录 = 不同身份，仍是合法的「一台机器跑多个节点」。
	otherLock, _, err := acquireSingleton(filepath.Join(t.TempDir(), "lanet.json"))
	if err != nil {
		t.Fatalf("不同配置目录应可照常启动，实际 err=%v", err)
	}
	otherLock.release()

	// ③ 持有者退出后（锁随进程释放，不需要人工清理锁文件）应能重新启动。
	waitChild()
	reLock, _, err := acquireSingleton(cfg)
	if err != nil {
		t.Fatalf("持有者退出后应能重新启动，实际 err=%v", err)
	}
	reLock.release()
}
