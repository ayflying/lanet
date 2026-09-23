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
	"sync"
	"time"
)

// fwdDialTimeout 转发拨号超时。
const fwdDialTimeout = 10 * time.Second

// fwdDefaultQuota 单监听器默认并发连接上限（LANForwardMaxConns 为 0 时采用）。
// 单端口承载成百上千连接属极端场景，256 足以覆盖绝大多数容器/服务暴露，
// 同时构成一道防止单端口被打爆的硬护栏。
const fwdDefaultQuota = 256

// fwdListener 一条活跃的本地监听。
type fwdListener struct {
	ln     net.Listener
	target string             // 当前转发目标（与转发表对齐，用于热更新比对）
	listen int                // 监听端口（日志用）
	quota  int                // 最大并发活动连接（<=0 表示不限制）
	cancel context.CancelFunc // 取消 accept 循环与全部活动连接
	mu     sync.Mutex
	conns  map[net.Conn]struct{} // 活动连接集合（配额判定 + 关闭时批量释放）
	closed bool                  // 关闭后禁止接纳已被 accept 的在途连接
}

// addConn 登记一条活动连接；超出配额返回 false（调用方应拒绝该连接）。
func (fl *fwdListener) addConn(conn net.Conn) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed || (fl.quota > 0 && len(fl.conns) >= fl.quota) {
		return false
	}
	if fl.conns == nil {
		fl.conns = make(map[net.Conn]struct{})
	}
	fl.conns[conn] = struct{}{}
	return true
}

// delConn 一条活动连接结束时注销。
func (fl *fwdListener) delConn(conn net.Conn) {
	fl.mu.Lock()
	delete(fl.conns, conn)
	fl.mu.Unlock()
}

// activeConns 当前活动连接数（配额/测试观测用）。
func (fl *fwdListener) activeConns() int {
	fl.mu.Lock()
	n := len(fl.conns)
	fl.mu.Unlock()
	return n
}

// shutdown 停止监听并释放全部活动连接：先 cancel 让 accept 循环与代理
// goroutine 感知退出，再关监听套接字，最后逐个关闭尚在活动的连接
// （io.Copy 随即收到 EOF/错误而结束，目标侧连接也一并回收）。
func (fl *fwdListener) shutdown() {
	if fl.cancel != nil {
		fl.cancel()
	}
	if fl.ln != nil {
		_ = fl.ln.Close()
	}
	fl.mu.Lock()
	fl.closed = true
	for conn := range fl.conns {
		_ = conn.Close()
	}
	fl.conns = nil
	fl.mu.Unlock()
}

// fwdQuota 解析单监听器配额：配置值 >0 用配置；0 或负数回退默认上限。
func (c *Client) fwdQuota() int {
	if c.cfg.LANForwardMaxConns <= 0 {
		return fwdDefaultQuota
	}
	return c.cfg.LANForwardMaxConns
}

// startListenForwards 按当前转发表启动全部本地监听（New 与热更新共用）。
func (c *Client) startListenForwards(ctx context.Context) {
	c.syncListenForwards(ctx)
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
	if ctx.Err() != nil || (c.rootCtx != nil && c.rootCtx.Err() != nil) {
		c.lfMu.Unlock()
		return
	}
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
	// 每条监听派生自己的子 context：目标变更/移除/节点关闭时仅取消本监听器，
	// 不影响其他转发；cancel 同时释放 accept 循环与全部活动连接。
	flCtx, flCancel := context.WithCancel(ctx)
	fl := &fwdListener{ln: ln, target: f.Target, listen: f.Listen, quota: c.fwdQuota(), cancel: flCancel}
	c.lfListeners[key] = fl
	c.lfMu.Unlock()

	c.logf("端口转发监听已启动：:%d → %s（配额 %d）", f.Listen, f.Target, fl.quota)
	go c.acceptLoop(flCtx, fl)
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
		fl.shutdown()
	}
}

// syncListenForwards 对齐监听集合与转发表：新增启动、删除停止、变更重启。
// 关键修复：用监听器「实际」target（fl.target）比对期望 target，
// 旧实现误用期望表填充 current，导致目标变更永远判定相等、从不重启监听。
func (c *Client) syncListenForwards(ctx context.Context) {
	// 在读取期望配置前串行化完整停/起流程，避免旧快照覆盖后到更新。
	// fwMu 与 lfMu 不嵌套；Close 先取消 rootCtx 再取 lfMu，不等待调和锁。
	c.lfReconcileMu.Lock()
	defer c.lfReconcileMu.Unlock()
	c.fwMu.RLock()
	desired := make(map[int]string, len(c.forwards))
	for _, f := range c.forwards {
		if f.Listen > 0 && f.Listen <= 65535 && f.Target != "" {
			desired[f.Listen] = f.Target
		}
	}
	c.fwMu.RUnlock()

	// 现有监听的实际 target 快照。
	c.lfMu.Lock()
	current := make(map[int]string, len(c.lfListeners))
	for port, fl := range c.lfListeners {
		current[port] = fl.target
	}
	c.lfMu.Unlock()

	// 停：现有监听不在期望表，或 target 已变化（先停后起）。
	for port, curTarget := range current {
		if want, ok := desired[port]; !ok || want != curTarget {
			c.stopListenForward(port)
		}
	}
	// 起：期望表里尚未监听，或 target 变化需重启。
	for port, target := range desired {
		c.lfMu.Lock()
		curTarget := ""
		if fl, ok := c.lfListeners[port]; ok {
			curTarget = fl.target
		}
		c.lfMu.Unlock()
		if curTarget != target {
			c.startListenForward(ctx, LANForward{Listen: port, Target: target})
		}
	}
}

// acceptLoop 接受入向连接并逐条代理到目标，受单监听器配额约束。
func (c *Client) acceptLoop(ctx context.Context, fl *fwdListener) {
	stop := context.AfterFunc(ctx, fl.shutdown)
	defer stop()
	defer fl.shutdown()
	for {
		conn, err := fl.ln.Accept()
		if err != nil {
			return // 监听已关闭或不可恢复错误
		}
		if !fl.addConn(conn) {
			// 超出配额：直接拒绝（连接关闭），避免无限堆积拖垮节点。
			c.logf("端口转发 :%d 超出并发配额（%d），拒绝新连接", fl.listen, fl.quota)
			_ = conn.Close()
			continue
		}
		go func() {
			defer fl.delConn(conn)
			c.proxyToTarget(ctx, fl, conn)
		}()
	}
}

// proxyToTarget 建立到目标的 TCP 连接并双向搬运。
// 半关闭语义与 PortFWD pipeBoth 一致：一侧读完先关对侧写端，
// 双向都结束后才彻底关闭（避免掐掉尚未读完的回程数据）。
// ctx 为监听器生命周期 context：节点关闭/监听器移除时取消，拨号与活动
// 连接随之释放（shutdown 同时关闭活动 conn，io.Copy 退出后目标侧亦回收）。
func (c *Client) proxyToTarget(ctx context.Context, fl *fwdListener, conn net.Conn) {
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, fwdDialTimeout)
	defer cancel()
	target, err := dialer.DialContext(dialCtx, "tcp", fl.target)
	if err != nil {
		c.logf("端口转发 :%d → %s 连接失败: %v", fl.listen, fl.target, err)
		_ = conn.Close()
		return
	}
	// 仅关闭入向连接不一定结束回程 io.Copy：目标可能收到 FIN 后仍保持连接。
	// 监听器取消时关闭两端，确保两个复制 goroutine 都能退出。
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		_ = target.Close()
	})
	defer stop()
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
