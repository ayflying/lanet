package tundevice

import (
	"io"
	"sync"
	"testing"
)

// Router取消回调、defer和测试清理可并发关闭，检查再close不是原子操作。
func TestMemoryDeviceConcurrentClose(t *testing.T) {
	for round := 0; round < 100; round++ {
		device, _, err := NewMemory(1400)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; _ = device.Close() }()
		}
		close(start)
		wg.Wait()
		if _, err := device.Read([][]byte{make([]byte, 1500)}, make([]int, 1), 0); err != io.ErrClosedPipe {
			t.Fatalf("关闭后读取: %v", err)
		}
	}
}
