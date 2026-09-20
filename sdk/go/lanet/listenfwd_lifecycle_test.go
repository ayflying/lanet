package lanet

import (
	"net"
	"testing"
	"time"
)

func TestForwardShutdownRejectsInflightConnection(t *testing.T) {
	fl := &fwdListener{quota: 1}
	a, b := net.Pipe()
	defer b.Close()
	if !fl.addConn(a) {
		t.Fatal("首条连接应被接纳")
	}
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	if fl.addConn(c) {
		t.Fatal("超过配额仍接纳连接")
	}
	fl.shutdown()
	fl.shutdown()
	if fl.activeConns() != 0 {
		t.Fatal("关闭后活动连接未清空")
	}
	if fl.addConn(c) {
		t.Fatal("关闭后重新接纳在途连接")
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := b.Read(buf[:]); err == nil {
		t.Fatal("活动连接未关闭")
	}
}
