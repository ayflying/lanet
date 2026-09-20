package main

import (
	"testing"
	"time"
)

const testProbeBase = 5 * time.Second

// 首次失败不延迟：单次失败常是瞬时抖动，而 probe 还承担 P2P 连接保温职责，
// 不该因一次抖动就打乱已连通成员的探测节奏。
func TestProbeBackoffFirstFailureNoDelay(t *testing.T) {
	b := newProbeBackoff()
	b.record("10.7.0.1", false, testProbeBase)
	if !b.due("10.7.0.1", time.Now()) {
		t.Fatal("首次失败不应延迟")
	}
}

// 连续失败按 base × 2^(n-1) 拉长，并封顶在 probeBackoffMax。
func TestProbeBackoffGrowsAndCaps(t *testing.T) {
	b := newProbeBackoff()
	b.record("k", false, testProbeBase) // n=1
	b.record("k", false, testProbeBase) // n=2 → 延迟 10s

	if b.due("k", time.Now().Add(5*time.Second)) {
		t.Fatal("第 2 次失败后 5s 内不应到期")
	}
	if !b.due("k", time.Now().Add(11*time.Second)) {
		t.Fatal("第 2 次失败后 11s 应到期")
	}

	for i := 0; i < 12; i++ {
		b.record("k", false, testProbeBase)
	}
	if b.due("k", time.Now().Add(probeBackoffMax-time.Second)) {
		t.Fatal("封顶后不应提前到期")
	}
	if !b.due("k", time.Now().Add(probeBackoffMax+time.Second)) {
		t.Fatal("超过上限后应到期")
	}
}

// 探测成功立即清零退避——已连通的成员必须回到每轮探测（保温）。
func TestProbeBackoffResetsOnSuccess(t *testing.T) {
	b := newProbeBackoff()
	b.record("k", false, testProbeBase)
	b.record("k", false, testProbeBase)
	b.record("k", true, testProbeBase)
	if !b.due("k", time.Now()) {
		t.Fatal("成功后应清零退避")
	}
	b.mu.Lock()
	left := len(b.fails) + len(b.nextAt)
	b.mu.Unlock()
	if left != 0 {
		t.Fatalf("成功后残留状态 %d 项", left)
	}
}

// 成员表里已消失的目标不再保留退避状态，避免 map 随成员更替无限增长。
func TestProbeBackoffRetainDropsStale(t *testing.T) {
	b := newProbeBackoff()
	b.record("gone", false, testProbeBase)
	b.record("gone", false, testProbeBase)
	b.retain(nil)
	b.mu.Lock()
	left := len(b.fails) + len(b.nextAt)
	b.mu.Unlock()
	if left != 0 {
		t.Fatalf("已消失的目标不应保留状态，实际残留 %d 项", left)
	}
}
