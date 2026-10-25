package lanet

import (
	"context"
	"sync"
	"testing"
)

// Discovery 的通知回调不受 Client.Close 等待；关闭后的迟到回调必须
// 访问稳定的已关闭句柄并返回错误，不能重读到 nil 或把关闭当作禁用审批。
func TestUnfriendCallbackAfterClientClose(t *testing.T) {
	c, db := newSeedTestClient(t, "self")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.peers != db {
		t.Fatal("Close changed the published database pointer")
	}
	if _, err := db.IsTrusted(context.Background(), "remote"); err == nil {
		t.Fatalf("database was not closed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.onUnfriendReceived("remote")
	}()
	<-done
	if c.isTrustedPeer("remote") {
		t.Fatal("closed address book must not disable approval")
	}
}

// 同步起跑并重复回调，使 -race 同时覆盖 Close 与回调中多次读取 peers。
func TestUnfriendCallbackConcurrentClientClose(t *testing.T) {
	c, db := newSeedTestClient(t, "self")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 8; j++ {
				c.onUnfriendReceived("remote")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_ = c.Close()
	}()
	close(start)
	wg.Wait()
	if c.peers != db {
		t.Fatal("Close changed the published database pointer")
	}
	c.onUnfriendReceived("remote")
}
