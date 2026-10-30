package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 本文件锁住「容器环境不得在程序内自更新」这条边界。
//
// 现场事故（0.5.98 容器节点）：用户在控制台点「立即更新并重启」→ 新程序
// 暂存到 /usr/local/bin/pvn-node.pending → 进程自杀让运行时重启容器 →
// 启动入口见标记就派生 helper 后立刻 os.Exit(0) → 容器在 PID 1 退出瞬间
// 杀掉 helper，标记永远消费不掉 → 每次启动只活 50ms 就退出，容器陷入
// 无限重启（RestartCount 一路涨到 15），版本永远停在 0.5.98。
//
// 三条路径都必须挡住：applyUpdate / runUpdateHelper / restartSelf。

// stubContainer 把 containerized 临时替换成固定返回值。
func stubContainer(t *testing.T, in bool) {
	t.Helper()
	orig := containerized
	containerized = func() bool { return in }
	t.Cleanup(func() { containerized = orig })
}

// TestApplyUpdateRejectedInContainer 容器内点更新必须立刻拒绝，不能下载、
// 不能暂存、更不能返回「正在重启」把进程弄死。
func TestApplyUpdateRejectedInContainer(t *testing.T) {
	stubContainer(t, true)
	oldGate := updateGate.busy
	updateGate.busy = false
	t.Cleanup(func() { updateGate.busy = oldGate })

	err := applyUpdate()
	if !errors.Is(err, errUpdateInContainer) {
		t.Fatalf("容器内 applyUpdate 应返回 errUpdateInContainer，实际: %v", err)
	}
	// 关键：拒绝发生在抢闸门之前，闸门不能被占住（否则之后连正常更新都进不来）。
	if updateGate.busy {
		t.Fatal("容器内拒绝更新时不应占用更新闸门")
	}
}

// TestRunUpdateHelperSkipsAndDropsMarkerInContainer 容器启动时看到遗留标记，
// 必须丢弃标记并继续运行，绝不能「派生 helper 后自杀」。
func TestRunUpdateHelperSkipsAndDropsMarkerInContainer(t *testing.T) {
	stubContainer(t, true)
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}

	// runUpdateHelper 用 selfExe() 定位；用一个 exe 同名文件覆盖该路径。
	origExe := selfExeOverride
	selfExeOverride = func() string { return exe }
	t.Cleanup(func() { selfExeOverride = origExe })

	if runUpdateHelper() {
		t.Fatal("容器内 runUpdateHelper 必须返回 false（应继续启动当前版本）")
	}
	assertNotExist(t, pendingUpdateMarkerPath(exe))
	assertNotExist(t, pendingUpdatePath(exe))
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("容器内丢弃标记时改动了当前程序：%q", got)
	}
}

// TestRunUpdateHelperNonContainerKeepsMarker 非容器（裸机/服务）行为不变：
// 见到标记就要派生 helper 并把控制权交出去。
func TestRunUpdateHelperNonContainerKeepsMarker(t *testing.T) {
	stubContainer(t, false)
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}
	origExe := selfExeOverride
	selfExeOverride = func() string { return exe }
	t.Cleanup(func() { selfExeOverride = origExe })
	// 派生 helper 会真的起进程：换成只记录调用的桩。
	origSpawn := spawnUpdateHelperFunc
	spawned := 0
	spawnUpdateHelperFunc = func() error { spawned++; return nil }
	t.Cleanup(func() { spawnUpdateHelperFunc = origSpawn })

	if !runUpdateHelper() {
		t.Fatal("非容器环境见到待更新标记应返回 true（转交 helper）")
	}
	if spawned != 1 {
		t.Fatalf("应派生 1 次 helper，实际 %d", spawned)
	}
}

// TestUpdateAPIRouteReportsContainer 控制台据此隐藏「立即更新并重启」按钮。
func TestUpdateAPIRouteReportsContainer(t *testing.T) {
	stubContainer(t, true)
	if !containerized() {
		t.Fatal("桩失效：containerized 应为 true")
	}
	if runningInContainer() {
		// 本机是 Windows 开发机，不应被误判成容器；若此处为真说明探测逻辑有问题。
		t.Fatal("Windows 开发机不应被判定为容器环境")
	}
}

// TestSelfExeOverrideDefaultsToRealBinary 防止 override 忘记复位导致其他测试串味。
func TestSelfExeOverrideDefaultsToRealBinary(t *testing.T) {
	if path := selfExe(); path == "" {
		t.Fatal("selfExe 应始终返回非空路径")
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("selfExe 返回了不可访问的路径 %q: %v", path, err)
	}
}
