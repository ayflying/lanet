package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
)

// captureProbeLog 重定向 std log 收集输出，返回恢复函数与缓冲。
func captureProbeLog(t *testing.T) (*bytes.Buffer, func()) {
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

func TestProbeLogLogsStateChanges(t *testing.T) {
	p := newProbeLogThrottle()
	buf, restore := captureProbeLog(t)
	defer restore()

	// 首次 OK：记录。
	p.report("a", "10.7.0.1", "direct", true, 10*time.Millisecond, nil)
	// 同状态重复：静默。
	p.report("a", "10.7.0.1", "direct", true, 12*time.Millisecond, nil)
	p.report("a", "10.7.0.1", "direct", true, 12*time.Millisecond, nil)
	// 路径变化 direct→relay：记录。
	p.report("a", "10.7.0.1", "relay", true, 50*time.Millisecond, nil)
	// OK→FAIL：记录。
	p.report("a", "10.7.0.1", "", false, 0, errors.New("dial failed"))
	// 持续 FAIL：静默。
	p.report("a", "10.7.0.1", "", false, 0, errors.New("dial failed"))

	out := buf.String()
	if got := strings.Count(out, "[probe]"); got != 3 {
		t.Fatalf("期望 3 条日志，实际 %d:\n%s", got, out)
	}
	if !strings.Contains(out, "OK a(10.7.0.1) via=direct rtt=10ms") ||
		!strings.Contains(out, "via=relay rtt=50ms") ||
		!strings.Contains(out, "FAIL a(10.7.0.1): dial failed") {
		t.Fatalf("日志内容不符合预期:\n%s", out)
	}
}

func TestProbeLogHeartbeat(t *testing.T) {
	p := newProbeLogThrottle()
	buf, restore := captureProbeLog(t)
	defer restore()

	p.report("b", "10.7.0.2", "relay", true, 20*time.Millisecond, nil)
	p.report("b", "10.7.0.2", "relay", true, 21*time.Millisecond, nil) // 静默
	// 人为把上次输出时间拨回 11 分钟前，触发心跳。
	p.mu.Lock()
	st := p.state["10.7.0.2"]
	st.lastLog = time.Now().Add(-11 * time.Minute)
	p.mu.Unlock()
	p.report("b", "10.7.0.2", "relay", true, 22*time.Millisecond, nil)

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 条日志（首条+心跳），实际 %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[1], "已持续") || !strings.Contains(lines[1], "OK b(10.7.0.2) via=relay") {
		t.Fatalf("心跳行内容不符合预期: %s", lines[1])
	}
}

func TestProbeLogRetain(t *testing.T) {
	p := newProbeLogThrottle()
	buf, restore := captureProbeLog(t)
	defer restore()

	p.report("c", "10.7.0.3", "direct", true, 5*time.Millisecond, nil)
	p.report("d", "10.7.0.4", "direct", true, 5*time.Millisecond, nil)
	p.retain([]netmapclient.Member{{VirtualIP: "10.7.0.3"}})
	if _, ok := p.state["10.7.0.3"]; !ok {
		t.Fatal("存活成员的状态不应被清理")
	}
	if _, ok := p.state["10.7.0.4"]; ok {
		t.Fatal("已退出成员的状态应被清理")
	}
	_ = buf
}
