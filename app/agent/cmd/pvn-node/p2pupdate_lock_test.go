package main

// 更新锁测试：GitHub 在线更新（控制台点击）与 P2P 自动更新共用同一把闸门，
// 保证「一轮更新开始后就锁到进程重启」，中间不会重复下载/替换/排重启。

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestUpdateGateExclusive 闸门互斥：同一时刻只有一轮更新能开始；释放
// （更新没落地）后可以重新开始。
func TestUpdateGateExclusive(t *testing.T) {
	defer func() {
		// 收尾：别把闸门留给同包其他测试。
		if updateInFlight() {
			releaseUpdate()
		}
	}()
	if updateInFlight() {
		t.Fatal("初始状态不应有更新在途")
	}
	if !acquireUpdate() {
		t.Fatal("首次应能抢占闸门")
	}
	if !updateInFlight() {
		t.Fatal("抢占后应报告更新在途")
	}
	if acquireUpdate() {
		t.Fatal("已在更新中时不应再次抢占（否则会重复下载/替换/重启）")
	}
	releaseUpdate()
	if updateInFlight() {
		t.Fatal("释放后不应再报告更新在途")
	}
	if !acquireUpdate() {
		t.Fatal("释放后应能重新抢占（失败要允许重试）")
	}
	releaseUpdate()
}

// TestUpdateGateHeldAfterLanded 替换成功后闸门保持占住、不释放：模拟
// 「已替换、待重启」窗口——此时巡检必须继续跳过，否则本进程仍把自己判成
// 落后（CurrentVersion 是编译期常量），会反复更新、反复排重启。
func TestUpdateGateHeldAfterLanded(t *testing.T) {
	if !acquireUpdate() {
		t.Skip("闸门被同包其他测试占用，跳过")
	}
	// 模拟替换成功路径：不调用 releaseUpdate。
	if !updateInFlight() {
		t.Fatal("替换成功后闸门应保持占住")
	}
	if acquireUpdate() {
		t.Fatal("等待重启期间不应允许开始新一轮更新")
	}
	releaseUpdate() // 收尾
}

// TestUpdateGateConcurrent 并发抢占只有一个成功——真实场景是 P2P 下载回调
// 与控制台点击几乎同时到达。
func TestUpdateGateConcurrent(t *testing.T) {
	const n = 16
	var wg sync.WaitGroup
	var won int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acquireUpdate() {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("并发抢占应只有 1 个成功，实际 %d", won)
	}
	releaseUpdate()
	if updateInFlight() {
		t.Fatal("收尾后闸门应回到空闲")
	}
}
