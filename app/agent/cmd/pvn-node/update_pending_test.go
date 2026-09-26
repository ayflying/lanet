package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件锁住 staged 自更新的状态机：暂存一致性、应用失败时的自愈、
// 以及「helper 应用失败 → 节点再派生 helper」的进程风暴防护。
// 相关实现见 update.go（stageNewBinary / applyPendingUpdate）与 update_helper.go。

func seedPendingDir(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	exe = filepath.Join(dir, "lanet.exe")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", filepath.Base(path), err)
	}
	return string(data)
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func writeMarker(t *testing.T, exe, sha string) {
	t.Helper()
	data, err := json.Marshal(pendingUpdateMarker{SHA256: sha})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, pendingUpdateMarkerPath(exe), string(data))
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s 应不存在，实际 err=%v", filepath.Base(path), err)
	}
}

// TestStageNewBinaryReplacesStaleCandidate 连续暂存两次：后一次必须完整覆盖
// 前一次（候选 + 标记同步更新），且当前程序始终不被触碰。
func TestStageNewBinaryReplacesStaleCandidate(t *testing.T) {
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	writeFile(t, filepath.Join(dir, "v3.exe"), "v3-binary")

	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}
	if err := stageNewBinary(filepath.Join(dir, "v3.exe"), exe); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, pendingUpdatePath(exe)); got != "v3-binary" {
		t.Fatalf("候选未覆盖为最新：%q", got)
	}
	marker := readFile(t, pendingUpdateMarkerPath(exe))
	if !strings.Contains(marker, digestOf("v3-binary")) {
		t.Fatalf("标记摘要未跟随最新候选：%s", marker)
	}
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("暂存过程改动了当前程序：%q", got)
	}
	updated, err := applyPendingUpdate(exe)
	if err != nil || !updated {
		t.Fatalf("应用最新候选失败：updated=%v err=%v", updated, err)
	}
	if got := readFile(t, exe); got != "v3-binary" {
		t.Fatalf("切换结果不是最新候选：%q", got)
	}
}

// TestStageNewBinaryFailureDropsStaleMarker 暂存失败时不得留下旧标记：
// 旧标记会指向已被覆盖/删除的候选，之后每次启动都判为待更新却永远应用失败。
func TestStageNewBinaryFailureDropsStaleMarker(t *testing.T) {
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}

	// 制造发布失败：候选路径被一个目录占住，rename 必定失败。
	candidate := pendingUpdatePath(exe)
	if err := os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(candidate, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "v3.exe"), "v3-binary")

	if err := stageNewBinary(filepath.Join(dir, "v3.exe"), exe); err == nil {
		t.Fatal("发布候选失败时必须报错")
	}
	assertNotExist(t, pendingUpdateMarkerPath(exe))
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("暂存失败改动了当前程序：%q", got)
	}
	if updated, err := applyPendingUpdate(exe); err != nil || updated {
		t.Fatalf("残留标记应已清理：updated=%v err=%v", updated, err)
	}
}

// TestApplyPendingUpdateDropsUnusableState 标记损坏 / 摘要长度非法 / 候选缺失
// 都是「永远应用不了」的残留：必须拒绝切换（fail-closed）并顺手丢弃，避免每次
// 启动都白跑一趟更新辅助进程。
func TestApplyPendingUpdateDropsUnusableState(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		drop   bool
	}{
		{"标记损坏", "{不是 JSON", true},
		{"摘要长度非法", `{"sha256":"abc"}`, true},
		{"候选缺失", `{"sha256":"` + strings.Repeat("0", 64) + `"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, exe := seedPendingDir(t)
			writeFile(t, pendingUpdateMarkerPath(exe), c.marker)

			updated, err := applyPendingUpdate(exe)
			if err == nil {
				t.Fatal("无法应用的状态必须报错")
			}
			if updated {
				t.Fatal("无法应用时不得报告已切换")
			}
			if got := readFile(t, exe); got != "old-binary" {
				t.Fatalf("拒绝切换时改动了当前程序：%q", got)
			}
			assertNotExist(t, pendingUpdateMarkerPath(exe))
			assertNotExist(t, exe+".rollback")
		})
	}
}

// TestApplyPendingUpdateKeepsMarkerOnChecksumMismatch 摘要不匹配是安全事件：
// 拒绝切换，但保留标记与候选现场供排查（下次成功暂存会先清掉它们）。
func TestApplyPendingUpdateKeepsMarkerOnChecksumMismatch(t *testing.T) {
	_, exe := seedPendingDir(t)
	writeFile(t, pendingUpdatePath(exe), "tampered")
	writeMarker(t, exe, strings.Repeat("0", 64))

	updated, err := applyPendingUpdate(exe)
	if err == nil || updated {
		t.Fatalf("摘要不匹配必须拒绝切换：updated=%v err=%v", updated, err)
	}
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("摘要不匹配时改动了当前程序：%q", got)
	}
	if got := readFile(t, pendingUpdateMarkerPath(exe)); !strings.Contains(got, strings.Repeat("0", 64)) {
		t.Fatalf("摘要不匹配应保留现场，标记内容=%q", got)
	}
	assertNotExist(t, exe+".rollback")
}

// TestApplyPendingUpdateNoMarkerIsNoop 没有待更新标记时是空操作，
// helper 与启动入口都依赖这个语义（普通重启不该被误判为升级）。
func TestApplyPendingUpdateNoMarkerIsNoop(t *testing.T) {
	_, exe := seedPendingDir(t)
	updated, err := applyPendingUpdate(exe)
	if err != nil || updated {
		t.Fatalf("无标记时应为空操作：updated=%v err=%v", updated, err)
	}
}

// TestApplyPendingUpdateIsIdempotent 切换成功后再应用一次仍是空操作，
// 且候选、标记都被清理干净（不会留下半套状态）。
func TestApplyPendingUpdateIsIdempotent(t *testing.T) {
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}
	if updated, err := applyPendingUpdate(exe); err != nil || !updated {
		t.Fatalf("首次应用失败：updated=%v err=%v", updated, err)
	}
	assertNotExist(t, pendingUpdatePath(exe))
	assertNotExist(t, pendingUpdateMarkerPath(exe))
	if updated, err := applyPendingUpdate(exe); err != nil || updated {
		t.Fatalf("重复应用应为空操作：updated=%v err=%v", updated, err)
	}
	if got := readFile(t, exe); got != "v2-binary" {
		t.Fatalf("重复应用破坏了程序：%q", got)
	}
}

// TestRestoreBackupRevertsAppliedUpdate 回滚点必须能还原旧版本；
// 没有回滚点时返回错误而不是静默成功（服务模式启动失败要靠它判断）。
func TestRestoreBackupRevertsAppliedUpdate(t *testing.T) {
	dir, exe := seedPendingDir(t)
	writeFile(t, filepath.Join(dir, "v2.exe"), "v2-binary")
	if err := stageNewBinary(filepath.Join(dir, "v2.exe"), exe); err != nil {
		t.Fatal(err)
	}
	if updated, err := applyPendingUpdate(exe); err != nil || !updated {
		t.Fatalf("应用失败：updated=%v err=%v", updated, err)
	}
	if err := restoreBackup(exe); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("回滚结果错误：%q", got)
	}

	_, clean := seedPendingDir(t)
	if err := restoreBackup(clean); err == nil {
		t.Fatal("没有回滚点时应返回错误")
	}
}

// TestUpdateHelperConfigPathResolvesLockDir helper 必须按 -config 定位锁目录：
// 定位错了就等不到旧实例放锁，替换会撞上正在运行的映像。
func TestUpdateHelperConfigPathResolvesLockDir(t *testing.T) {
	exe := filepath.Join("D:", "lanet-node", "lanet.exe")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"等号形式", []string{"-config=D:\\lanet-node\\lanet.json"}, "D:\\lanet-node\\lanet.json"},
		{"双横线等号", []string{"--config=C:\\x\\lanet.json"}, "C:\\x\\lanet.json"},
		{"空格形式", []string{"-config", "C:\\y\\lanet.json"}, "C:\\y\\lanet.json"},
		{"无参数退回 exe 目录", []string{"-tray"}, filepath.Join("D:", "lanet-node", "lanet.json")},
	}
	for _, c := range cases {
		if got := updateHelperConfigPath(exe, c.args); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestUpdateHelperDoneEnvBreaksSpawnStorm 应用失败后拉起的节点必须带上抑制标记，
// 否则它会再次派生 helper，形成无休止的进程风暴；应用成功则不带（标记已清）。
func TestUpdateHelperDoneEnvBreaksSpawnStorm(t *testing.T) {
	t.Setenv(updateHelperDoneEnv, "")
	if updateHelperSuppressed() {
		t.Fatal("未设置抑制标记时不应被判定为抑制")
	}
	if envHas(os.Environ(), updateHelperDoneEnv+"=1") {
		t.Fatalf("测试环境不应预置 %s", updateHelperDoneEnv)
	}
	if !envHas(nodeStartEnv(false), updateHelperDoneEnv+"=1") {
		t.Fatal("应用失败后拉起的节点缺少抑制标记（会形成进程风暴）")
	}
	if envHas(nodeStartEnv(true), updateHelperDoneEnv+"=1") {
		t.Fatal("应用成功后不应给新节点带抑制标记")
	}
	t.Setenv(updateHelperDoneEnv, "1")
	if !updateHelperSuppressed() {
		t.Fatal("设置抑制标记后应判定为抑制")
	}
}

func envHas(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
