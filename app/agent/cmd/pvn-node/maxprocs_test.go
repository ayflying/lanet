package main

import (
	"runtime"
	"testing"
)

// TestApplyMaxProcsExplicit 显式配置优先且不超过核数。
func TestApplyMaxProcsExplicit(t *testing.T) {
	cores := runtime.NumCPU()
	orig := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(orig) })

	got := applyMaxProcs(2)
	if got != 2 || runtime.GOMAXPROCS(0) != 2 {
		t.Fatalf("applyMaxProcs(2) 应生效为 2，实际 %d / %d", got, runtime.GOMAXPROCS(0))
	}
	// 超过核数收敛到核数。
	got = applyMaxProcs(cores + 100)
	if got != cores {
		t.Fatalf("applyMaxProcs(超核数) 应收敛到核数 %d，实际 %d", cores, got)
	}
}

// TestApplyMaxProcsAutoCap 无显式配置时，高于上限的 GOMAXPROCS 被压到默认上限。
func TestApplyMaxProcsAutoCap(t *testing.T) {
	cores := runtime.NumCPU()
	orig := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(orig) })

	if cores <= maxProcsDefaultCap {
		t.Skipf("本机核数 %d <= 上限 %d，自动封顶不触发", cores, maxProcsDefaultCap)
	}
	runtime.GOMAXPROCS(cores) // 模拟「按宿主核数开满」的默认态
	got := applyMaxProcs(0)
	if got != maxProcsDefaultCap || runtime.GOMAXPROCS(0) != maxProcsDefaultCap {
		t.Fatalf("applyMaxProcs(0) 应封顶到 %d，实际 %d / %d", maxProcsDefaultCap, got, runtime.GOMAXPROCS(0))
	}
}

// TestApplyMaxProcsAutoRespectQuota GOMAXPROCS 已低于核数（容器配额已生效）时不干预。
func TestApplyMaxProcsAutoRespectQuota(t *testing.T) {
	cores := runtime.NumCPU()
	orig := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(orig) })

	if cores < 2 {
		t.Skipf("本机核数 %d < 2，无法模拟配额态", cores)
	}
	runtime.GOMAXPROCS(1) // 模拟 cgroup 配额 1 核
	got := applyMaxProcs(0)
	if got != 1 || runtime.GOMAXPROCS(0) != 1 {
		t.Fatalf("配额已生效时应尊重现状 1，实际 %d / %d", got, runtime.GOMAXPROCS(0))
	}
}
