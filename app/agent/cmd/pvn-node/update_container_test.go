package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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

// isolateUpdateHelperStartup 避免 go test 参数或父进程的抑制标记改变启动路径。
func isolateUpdateHelperStartup(t *testing.T, args ...string) {
	t.Helper()
	oldArgs := os.Args
	os.Args = append([]string{"pvn-node"}, args...)
	t.Cleanup(func() { os.Args = oldArgs })
	t.Setenv(updateHelperDoneEnv, "")
}

// forbidContainerUpdateNetwork 拦住意外联网，同时记录请求以确保拒绝发生在联网之前。
func forbidContainerUpdateNetwork(t *testing.T) *atomic.Int32 {
	t.Helper()
	oldClient := http.DefaultClient
	var calls atomic.Int32
	http.DefaultClient = &http.Client{Transport: updateTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network request in container update test")
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	return &calls
}

// TestApplyUpdateRejectedInContainer 容器内点更新必须立刻拒绝，不能下载、
// 不能暂存、更不能返回「正在重启」把进程弄死。
func TestApplyUpdateRejectedInContainer(t *testing.T) {
	stubContainer(t, true)
	calls := forbidContainerUpdateNetwork(t)
	t.Cleanup(func() {
		if got := calls.Load(); got != 0 {
			t.Errorf("容器内拒绝更新不应联网，实际 %d 次", got)
		}
	})
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
	isolateUpdateHelperStartup(t)
	stubContainer(t, true)
	origSpawn := spawnUpdateHelperFunc
	spawnUpdateHelperFunc = func() error {
		t.Error("容器启动不应派生 helper")
		return errors.New("unexpected helper spawn")
	}
	t.Cleanup(func() { spawnUpdateHelperFunc = origSpawn })
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
	isolateUpdateHelperStartup(t)
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

// 显式 helper 模式在容器里也必须被消费（返回 true），否则会继续作为普通节点启动。
func TestRunUpdateHelperExplicitInContainerDoesNotSpawn(t *testing.T) {
	isolateUpdateHelperStartup(t, updateHelperArg)
	stubContainer(t, true)
	dir, exe := seedPendingDir(t)
	candidate := filepath.Join(dir, "v2.exe")
	writeFile(t, candidate, "v2-binary")
	if err := stageNewBinary(candidate, exe); err != nil {
		t.Fatal(err)
	}
	origExe := selfExeOverride
	selfExeOverride = func() string { return exe }
	t.Cleanup(func() { selfExeOverride = origExe })
	origSpawn := spawnUpdateHelperFunc
	spawnUpdateHelperFunc = func() error {
		t.Error("容器内显式 helper 不应派生子进程")
		return errors.New("unexpected helper spawn")
	}
	t.Cleanup(func() { spawnUpdateHelperFunc = origSpawn })

	if !runUpdateHelper() {
		t.Fatal("容器内显式 -update-helper 必须返回 true（结束 helper 模式）")
	}
	assertNotExist(t, pendingUpdateMarkerPath(exe))
	assertNotExist(t, pendingUpdatePath(exe))
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("容器内显式 helper 不应替换当前程序：%q", got)
	}
}

// TestUpdateAPIRouteReportsContainer 真实调用路由；控制台据此切换容器更新提示。
func TestUpdateAPIRouteReportsContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   bool
	}{
		{"container", true},
		{"non-container", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubContainer(t, tc.in)
			oldState := upd
			checkedAt := time.Now().Truncate(time.Second)
			upd = &updateState{
				checked: true, hasUpdate: true, current: "1.0.0", latest: "1.0.1",
				notes: "cached release notes", assetName: "lanet-test.zip", checkedAt: checkedAt,
			}
			t.Cleanup(func() { upd = oldState })
			calls := forbidContainerUpdateNetwork(t)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/update", nil)
			updateRoutes(func() {})["GET /api/update"](recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET /api/update status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{
				"container": tc.in, "checked": true, "has_update": true,
				"current": "1.0.0", "latest": "1.0.1", "notes": "cached release notes",
				"asset": "lanet-test.zip", "checked_at": checkedAt.Format(time.RFC3339),
				"timed_out": false,
			} {
				if got, ok := body[field]; !ok || got != want {
					t.Errorf("字段 %s=%v（存在=%v），期望 %v", field, got, ok, want)
				}
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("新鲜更新缓存不应联网，实际 %d 次", got)
			}
		})
	}
}

func TestUpdateAPIApplyRejectedInContainer(t *testing.T) {
	stubContainer(t, true)
	calls := forbidContainerUpdateNetwork(t)
	oldState := upd
	upd = &updateState{current: "1.0.0"} // 故意无缓存：容器拒绝不能触发在线检查。
	t.Cleanup(func() { upd = oldState })
	oldGate := updateGate.busy
	updateGate.busy = false
	t.Cleanup(func() { updateGate.busy = oldGate })

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/update/apply", nil)
	updateRoutes(func() {})["POST /api/update/apply"](recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("容器 apply 应返回 409，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body["error"]; got != errUpdateInContainer.Error() {
		t.Errorf("容器 apply error=%v，期望 %q", got, errUpdateInContainer.Error())
	}
	for _, field := range []string{"restarting", "update_pending"} {
		if _, ok := body[field]; ok {
			t.Errorf("容器拒绝响应不得含有 %s: %s", field, recorder.Body.String())
		}
	}
	if updateInFlight() {
		t.Error("容器拒绝不得占用更新闸门")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("容器 apply 不应联网，实际 %d 次", got)
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
