// 本地端口监听转发：把转发表（listen → target）里的每个端口在节点虚拟 IP
// 上监听起来并代理到真实目标。TUN 场景下成员发往「本节点虚拟 IP:端口」的
// TCP 会进入本机协议栈；若该端口没有进程监听，内核直接 RST（表现为
// ping 通但服务连不上）。这里补上监听侧，让 `http://<虚拟IP>:端口` 直接
// 可达——典型用途：容器节点把宿主上其他容器发布的服务暴露给群内成员。
package lanet

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

// fwdDialTimeout 转发拨号超时。
const fwdDialTimeout = 10 * time.Second

// fwdListener 一条活跃的本地监听。
type fwdListener struct {
	ln net.Listener
}

// startListenForwards 按当前转发表启动全部本地监听（New 与热更新共用）。
func (c *Client) startListenForwards(ctx context.Context) {
	c.fwMu.RLock()
	fs := append([]LANForward(nil), c.forwards...)
	c.fwMu.RUnlock()
	for _, f := range fs {
		c.startListenForward(ctx, f)
	}
}

// startListenForward 为单条映射启动监听；端口占用等失败仅记日志降级。
// ctx 必须是节点生命周期 context（rootCtx），不能用请求作用域 context，
// 否则请求结束后监听 goroutine 会被取消。
func (c *Client) startListenForward(ctx context.Context, f LANForward) {
	if f.Listen <= 0 || f.Listen > 65535 || f.Target == "" {
		return
	}
	key := listenKey(f.Listen)
	c.lfMu.Lock()
	if _, ok := c.lfListeners[key]; ok {
		c.lfMu.Unlock()
		return // 已在监听
	}
	// 监听所有接口：群内成员经 TUN 用虚拟 IP 访问；本机用 127.0.0.1 也可达，
	// 便于验证。是否对外暴露仍由部署环境的网络边界决定（容器/防火墙）。
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", f.Listen))
	if err != nil {
		c.lfMu.Unlock()
		c.logf("端口转发监听 :%d 失败（不影响其他转发）: %v", f.Listen, err)
		return
	}
	fl := &fwdListener{ln: ln}
	c.lfListeners[key] = fl
	c.lfMu.Unlock()

	c.logf("端口转发监听已启动：:%d → %s", f.Listen, f.Target)
	go func() {
		<-ctx.Done()
		_ = fl.ln.Close()
	}()
	go c.acceptLoop(ctx, f, fl)
}

// stopListenForward 停止并移除某端口的监听（映射表删除该项时调用）。
func (c *Client) stopListenForward(listen int) {
	key := listenKey(listen)
	c.lfMu.Lock()
	fl, ok := c.lfListeners[key]
	if ok {
		delete(c.lfListeners, key)
	}
	c.lfMu.Unlock()
	if ok {
		c.logf("端口转发监听已停止：:%d", listen)
		_ = fl.ln.Close()
	}
}

// syncListenForwards 对齐监听集合与转发表：新增启动、删除停止、变更重启。
func (c *Client) syncListenForwards(ctx context.Context) {
	c.fwMu.RLock()
	desired := make(map[int]string, len(c.forwards))
	for _, f := range c.forwards {
		if f.Listen > 0 && f.Listen <= 65535 && f.Target != "" {
			desired[f.Listen] = f.Target
		}
	}
	c.fwMu.RUnlock()

	c.lfMu.Lock()
	current := make(map[int]string, len(c.lfListeners))
	for port := range c.lfListeners {
		if t, ok := desired[port]; ok {
			current[port] = t
		}
	}
	c.lfMu.Unlock()

	// 停：现有监听里不在期望表中的（含 target 变化的，先停后起）。
	for port := range current {
		if t, ok := desired[port]; !ok || t != current[port] {
			c.stopListenForward(port)
		}
	}
	// 起：期望表里尚未监听的。
	for port, target := range desired {
		if cur, ok := current[port]; !ok || cur != target {
			c.startListenForward(ctx, LANForward{Listen: port, Target: target})
		}
	}
}

// acceptLoop 接受入向连接并逐条代理到目标。
func (c *Client) acceptLoop(ctx context.Context, f LANForward, fl *fwdListener) {
	for {
		conn, err := fl.ln.Accept()
		if err != nil {
			return // 监听已关闭或不可恢复错误
		}
		go c.proxyToTarget(ctx, f, conn)
	}
}

// proxyToTarget 建立到目标的 TCP 连接并双向搬运。
// 半关闭语义与 PortFWD pipeBoth 一致：一侧读完先关对侧写端，
// 双向都结束后才彻底关闭（避免掐掉尚未读完的回程数据）。
func (c *Client) proxyToTarget(ctx context.Context, f LANForward, conn net.Conn) {
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, fwdDialTimeout)
	defer cancel()
	target, err := dialer.DialContext(dialCtx, "tcp", f.Target)
	if err != nil {
		c.logf("端口转发 :%d → %s 连接失败: %v", f.Listen, f.Target, err)
		_ = conn.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, conn)
		if hc, ok := target.(halfCloseWriter); ok {
			_ = hc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, target)
		if hc, ok := conn.(halfCloseWriter); ok {
			_ = hc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = conn.Close()
	_ = target.Close()
}

func listenKey(port int) int { return port }
