package gateway

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Godot headless 端到端测试 ----
//
// 用法：设置环境变量 LANET_GODOT 指向 Godot 4.x 控制台 exe 后运行：
//
//	LANET_GODOT=/path/to/Godot_v4.3-stable_win64_console.exe go test ./app/gateway/... -run TestGodot
//
// 未设置时跳过。测试组装一个临时 Godot 工程（SDK 源码 + 测试脚本），
// 以 headless 模式运行 1 个帧协议单测 + 3 个联机进程（host + 2 guest），
// 全部连接本测试起动的真实网关。

// TestGodotFrameCodec 帧协议编解码（Godot ↔ Go 字节级一致性）。
func TestGodotFrameCodec(t *testing.T) {
	godot, proj := assembleGodotProject(t)
	out := runGodot(t, godot, proj, "--script", "res://tests/test_frame.gd")
	if !strings.Contains(out, "FRAME-PASS") {
		t.Fatalf("帧协议单测未通过:\n%s", out)
	}
}

// TestGodotE2E 三进程联机：房间 + @rpc（含房主 relay）+ JSON 消息。
func TestGodotE2E(t *testing.T) {
	godot, proj := assembleGodotProject(t)
	g := newBridgeGW(t)

	scriptURL := strings.Replace(g.srv.URL, "http", "ws", 1) + "/gateway"

	type runner struct {
		name string
		cmd  *exec.Cmd
		out  strings.Builder
		done chan struct{}
	}
	start := func(name string, args ...string) *runner {
		full := append([]string{"--path", proj, "--script", "res://tests/test_e2e.gd", "--"}, args...)
		cmd := exec.Command(godot, full...)
		r := &runner{name: name, cmd: cmd, done: make(chan struct{})}
		cmd.Stdout = &r.out
		cmd.Stderr = &r.out
		go func() {
			_ = cmd.Run()
			close(r.done)
		}()
		return r
	}

	host := start("host", "--mode=host", "--url="+scriptURL)
	time.Sleep(500 * time.Millisecond) // 让 host 先就绪
	ga := start("guestA", "--mode=guestA", "--url="+scriptURL)
	gb := start("guestB", "--mode=guestB", "--url="+scriptURL)

	deadline := time.After(120 * time.Second)
	waiters := map[string]*runner{"host": host, "guestA": ga, "guestB": gb}
	var failMsg string
	for len(waiters) > 0 {
		select {
		case <-host.done:
			if strings.Contains(host.out.String(), "E2E-PASS") {
				delete(waiters, "host")
			} else if failMsg == "" {
				failMsg = "host 未通过:\n" + host.out.String()
				delete(waiters, "host")
			}
		case <-ga.done:
			if strings.Contains(ga.out.String(), "E2E-PASS") {
				delete(waiters, "guestA")
			} else if failMsg == "" {
				failMsg = "guestA 未通过:\n" + ga.out.String()
				delete(waiters, "guestA")
			}
		case <-gb.done:
			if strings.Contains(gb.out.String(), "E2E-PASS") {
				delete(waiters, "guestB")
			} else if failMsg == "" {
				failMsg = "guestB 未通过:\n" + gb.out.String()
				delete(waiters, "guestB")
			}
		case <-deadline:
			for name, r := range waiters {
				failMsg += fmt.Sprintf("=== 超时进程 %s 输出 ===\n%s\n", name, r.out.String())
				_ = r.cmd.Process.Kill()
			}
			waiters = nil
		}
	}
	if failMsg != "" {
		// 打印全部进程输出，便于交叉诊断。
		t.Logf("=== host ===\n%s", host.out.String())
		t.Logf("=== guestA ===\n%s", ga.out.String())
		t.Logf("=== guestB ===\n%s", gb.out.String())
		t.Fatal(failMsg)
	}
}

// ---- 基建 ----

// assembleGodotProject 组装临时 Godot 工程：SDK 源码 + 测试脚本。
func assembleGodotProject(t *testing.T) (godotExe, projectDir string) {
	t.Helper()
	godot := os.Getenv("LANET_GODOT")
	if godot == "" {
		t.Skip("未设置 LANET_GODOT，跳过 Godot headless 测试")
	}
	if _, err := os.Stat(godot); err != nil {
		t.Skipf("LANET_GODOT 不可用: %v", err)
	}

	proj := t.TempDir()
	mustCopy := func(src, dst string) {
		t.Helper()
		if err := copyPath(src, dst); err != nil {
			t.Fatalf("组装工程失败: %v", err)
		}
	}
	root := findRepoRoot(t)
	mustCopy(filepath.Join(root, "sdk", "godot", "addons"), filepath.Join(proj, "addons"))
	mustCopy(filepath.Join(root, "sdk", "godot", "tests"), filepath.Join(proj, "tests"))

	projectFile := `config_version=5

[application]

config/name="Lanet SDK Test"
config/features=PackedStringArray("4.3")
`
	// 测试脚本手动实例化 LanetManager（--script 模式下 autoload 全局名
	// 不可用），故工程不注册 autoload。
	if err := os.WriteFile(filepath.Join(proj, "project.godot"), []byte(projectFile), 0o644); err != nil {
		t.Fatalf("写 project.godot 失败: %v", err)
	}
	return godot, proj
}

// runGodot headless 运行 Godot 脚本，返回标准输出+错误合并文本。
func runGodot(t *testing.T, godot, proj string, args ...string) string {
	t.Helper()
	full := append([]string{"--path", proj}, args...)
	cmd := exec.Command(godot, full...)
	out, err := cmd.CombinedOutput()
	// Godot 无工程主场景时会以非零退出——测试脚本自行 quit(code)，
	// 这里以输出为准判断，错误仅附注。
	if err != nil && !strings.Contains(string(out), "PASS") {
		t.Logf("godot 退出码: %v", err)
	}
	return string(out)
}

// findRepoRoot 向上找 go.mod 定位仓库根。
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到仓库根")
		}
		dir = parent
	}
}

// copyPath 递归拷贝目录/文件。
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
