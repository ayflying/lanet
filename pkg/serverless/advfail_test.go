package serverless

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
)

func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	return &buf, func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}
}

// newQuietlessDiscovery 构造仅用于日志降噪测试的 Discovery 零值。
func newQuietlessDiscovery() *Discovery {
	return &Discovery{}
}

func TestLogAdvFailureThrottlesRepeatedError(t *testing.T) {
	d := newQuietlessDiscovery()
	buf, restore := captureLog(t)
	defer restore()

	err := errors.New("context deadline exceeded")
	// 同一错误 40 次：第 1、2、20、40 次各记一条，共 4 条。
	for i := 1; i <= 40; i++ {
		d.logAdvFailure("dht-private", err)
	}
	out := buf.String()
	if got := strings.Count(out, "DHT 广播失败"); got != 4 {
		t.Fatalf("期望 4 条日志（第 1/2/20/40 次），实际 %d:\n%s", got, out)
	}
	for _, want := range []string{"第 1 次", "第 2 次", "第 20 次", "第 40 次"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %s 的日志:\n%s", want, out)
		}
	}
}

func TestLogAdvFailureNewErrorModeResets(t *testing.T) {
	d := newQuietlessDiscovery()
	buf, restore := captureLog(t)
	defer restore()

	d.logAdvFailure("dht-private", errors.New("context deadline exceeded"))
	d.logAdvFailure("dht-private", errors.New("context deadline exceeded"))
	// 错误文案变化 = 新失败模式，立即记录并重新计数。
	d.logAdvFailure("dht-private", errors.New("failed to find any peer in table"))
	d.logAdvFailure("dht-private", errors.New("failed to find any peer in table"))
	d.logAdvFailure("dht-private", errors.New("failed to find any peer in table")) // 第 3 次，不记录

	out := buf.String()
	if got := strings.Count(out, "DHT 广播失败"); got != 4 {
		t.Fatalf("期望 4 条日志（旧模式 2 条 + 新模式前 2 次），实际 %d:\n%s", got, out)
	}
	if !strings.Contains(out, "多为路由表空/无种子的冷启动状态") {
		t.Fatalf("路由表空错误应附带冷启动提示:\n%s", out)
	}
}

// 并发调用不应 data race（-race 下跑）。
func TestLogAdvFailureConcurrent(t *testing.T) {
	d := newQuietlessDiscovery()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				d.logAdvFailure("dht-private", errors.New("boom"))
			}
		}(i)
	}
	wg.Wait()
}
