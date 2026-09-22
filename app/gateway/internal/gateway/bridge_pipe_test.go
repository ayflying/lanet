package gateway

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// TestBridgePipeRoundTrip 双向数据往返。
func TestBridgePipeRoundTrip(t *testing.T) {
	a, b := newBridgePipePair()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := a.Write([]byte("ping")); err != nil {
			t.Errorf("a 写失败: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 16)
		n, err := b.Read(buf)
		if err != nil || string(buf[:n]) != "ping" {
			t.Errorf("b 读失败: n=%d err=%v data=%q", n, err, buf[:n])
		}
	}()
	wg.Wait()
}

// TestBridgePipeHalfClose 半关闭：对端读到 EOF；本端读不受影响；对端写报错。
func TestBridgePipeHalfClose(t *testing.T) {
	a, b := newBridgePipePair()
	// 先写数据再半关：缓冲中的数据必须仍可读。
	if _, err := a.Write([]byte("last")); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if err := a.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite 失败: %v", err)
	}
	buf := make([]byte, 16)
	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != "last" {
		t.Fatalf("半关后剩余数据应可读: n=%d err=%v", n, err)
	}
	// 数据耗尽后 EOF。
	if _, err := b.Read(buf); err != io.EOF {
		t.Fatalf("期望 EOF，得到 %v", err)
	}
	// a 的写端已半关：再写报错。
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("半关后写应报错")
	}
	// a 的读端不受自己半关影响。
	go func() {
		_, _ = b.Write([]byte("back"))
	}()
	n, err = a.Read(buf)
	if err != nil || string(buf[:n]) != "back" {
		t.Fatalf("半关后本端读应正常: n=%d err=%v", n, err)
	}
}

// TestBridgePipeClose 全关：两端读写均报错，重复 Close 幂等。
func TestBridgePipeClose(t *testing.T) {
	a, b := newBridgePipePair()
	if _, err := a.Write([]byte("x")); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	// 已入队的残留数据先可读（全关不丢已送达数据），排空后再报错。
	buf := make([]byte, 8)
	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != "x" {
		t.Fatalf("全关后残留数据应可读: n=%d err=%v data=%q", n, err, buf[:n])
	}
	if _, err := b.Read(buf); err == nil {
		t.Fatal("排空后应报错")
	}
	if _, err := b.Write([]byte("y")); err == nil {
		t.Fatal("对端全关后写应报错")
	}
}

// TestBridgePipeWriteAfterPeerClose 对端全关后写必报错。
// 回归：Write 的 select 在「入队成功」与「对端 done」同时就绪时随机选择，
// 曾以约 50% 概率让全关后的写静默成功（Linux CI 间歇失败）。高重复次数
// 压住这个概率窗口；若实现退化，count=50 下几乎必然复现。
func TestBridgePipeWriteAfterPeerClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		a, b := newBridgePipePair()
		if _, err := a.Write([]byte("x")); err != nil {
			t.Fatalf("第 %d 次: 写失败: %v", i, err)
		}
		if err := a.Close(); err != nil {
			t.Fatalf("第 %d 次: Close 失败: %v", i, err)
		}
		// 排空残留数据，进入「对端已关且队列空」的稳态。
		buf := make([]byte, 8)
		for {
			if _, err := b.Read(buf); err != nil {
				break
			}
		}
		if _, err := b.Write([]byte("y")); err == nil {
			t.Fatalf("第 %d 次: 对端全关后写应报错", i)
		}
	}
}

// TestBridgePipeBigFrame 单帧大于读缓冲：pending 机制保证数据完整。
func TestBridgePipeBigFrame(t *testing.T) {
	a, b := newBridgePipePair()
	payload := bytes.Repeat([]byte{0xAB}, 64*1024) // 64KB > pump 16KB 缓冲
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Write(payload)
	}()
	got, err := io.ReadAll(io.LimitReader(b, int64(len(payload))))
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	<-done
	if !bytes.Equal(got, payload) {
		t.Fatalf("大帧数据损坏: got %d bytes", len(got))
	}
}

// TestBridgePipeWriteBlockedByDeadPeer 对端全关后阻塞中的写立即返回错误。
func TestBridgePipeWriteBlockedByDeadPeer(t *testing.T) {
	a, b := newBridgePipePair()
	// 填满对端缓冲（256 帧），让下一次写阻塞。
	full := make([]byte, 16)
	for i := 0; i < 256; i++ {
		if _, err := a.Write(full); err != nil {
			t.Fatalf("填充失败: %v", err)
		}
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = b.Close() // 对端全关，解除 a 的写阻塞
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := a.Write(full)
		if err == nil {
			t.Error("对端全关后阻塞写应报错")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("写未解除阻塞")
	}
}
