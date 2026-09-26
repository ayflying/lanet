package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const updateHelperArg = "-update-helper"

// updateHelperDoneEnv 标记「本进程是更新辅助进程在应用失败后拉起的实例」。
// 没有它就会出现进程风暴：helper 应用失败 → 拉起节点 → 节点启动时又看到标记
// → 再派生 helper → 再失败……每轮多出两个进程。带上这个环境变量后，被拉起的
// 那一代只按当前版本正常运行，暂存更新留给下一次显式重启处理。
const updateHelperDoneEnv = "LANET_UPDATE_HELPER_DONE"

func updateHelperSuppressed() bool { return os.Getenv(updateHelperDoneEnv) != "" }

// nodeStartEnv 返回拉起节点进程用的环境变量。applied=false 表示这一轮暂存更新
// 没能应用（或没东西可应用），此时追加抑制标记，打断进程风暴。
func nodeStartEnv(applied bool) []string {
	env := os.Environ()
	if applied {
		return env
	}
	return append(env, updateHelperDoneEnv+"=1")
}

func runUpdateHelper() bool {
	exe := selfExe()
	if len(os.Args) >= 2 && os.Args[1] == updateHelperArg {
		args := append([]string(nil), os.Args[2:]...)
		return runUpdateHelperProcess(exe, args)
	}
	// 普通交互模式直接启动时也消费之前暂存的更新；服务相关的内部命令
	// （-service / -service-restart）必须排除——它们要么由 SCM 托管、要么本身就是
	// 服务重启辅助进程，若在这里被转入普通 helper，服务就不会走 stop/start，
	// 而 helper 会在服务仍在运行时替换映像。
	for _, arg := range os.Args[1:] {
		lower := strings.ToLower(arg)
		if strings.HasPrefix(lower, "-service") || strings.HasPrefix(lower, "--service") {
			return false
		}
	}
	if updateHelperSuppressed() {
		return false
	}
	if _, err := os.Stat(pendingUpdateMarkerPath(exe)); err != nil {
		return false
	}
	if err := spawnUpdateHelper(); err != nil {
		// 派生失败不能连累节点：继续以当前版本启动。这条日志在 windowsgui 子系统
		// 下没有 stdio（此时日志重定向还没建立）可能看不到，因此以「不影响运行」为准。
		log.Printf("[update-helper] 启动更新辅助进程失败，继续以当前版本启动: %v", err)
		return false
	}
	return true
}

func runUpdateHelperProcess(exe string, args []string) bool {
	setupUpdateHelperLog(exe, args)
	changed := false
	var lastErr error
	var updateLock *singletonLock
	for i := 0; i < 200; i++ {
		if _, statErr := os.Stat(pendingUpdateMarkerPath(exe)); statErr != nil {
			lastErr = statErr
			break
		}
		// helper 必须持有单实例锁覆盖检查与替换的整个窗口，避免“检查通过后
		// 另一实例抢先启动”。替换完成后才释放，随后启动的新实例才能取得锁。
		if updateLock, _, lastErr = acquireSingleton(updateHelperConfigPath(exe, args)); lastErr == nil {
			changed, lastErr = applyPendingUpdate(exe)
			updateLock.release()
			updateLock = nil
			if changed || lastErr != nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !changed {
		if lastErr == nil {
			lastErr = fmt.Errorf("等待旧实例退出或待更新文件超时")
		}
		log.Printf("[update-helper] 暂存更新未应用，继续启动当前版本: %v", lastErr)
	}
	// startNode 拉起节点进程。应用没成功时给这一代带上 updateHelperDoneEnv：
	// 标记还在，若不打标记，它会把自己当成「有待更新待消费」而再派生 helper，
	// 应用失败（摘要不符/候选损坏/等锁超时）就变成无休止的进程风暴。
	startNode := func() error {
		cmd := exec.Command(exe, args...)
		cmd.Dir = exeDir()
		cmd.SysProcAttr = spawnSysProcAttr
		cmd.Env = nodeStartEnv(changed)
		return cmd.Start()
	}
	if err := startNode(); err != nil {
		if changed {
			_ = restoreBackup(exe)
			log.Printf("[update-helper] 新版本启动失败，已尝试恢复旧版本: %v", err)
			if oldErr := startNode(); oldErr != nil {
				log.Printf("[update-helper] 恢复旧版本启动失败: %v", oldErr)
			}
		} else {
			log.Printf("[update-helper] 当前版本启动失败: %v", err)
		}
	}
	return true
}

// setupUpdateHelperLog 把 helper 的日志接到配置目录的 lanet.log。
// helper 由 windowsgui 子系统的主程序派生、没有 stdio，不重定向的话它替换失败
// 的原因会彻底消失——现场表现就是「点了更新，程序没了，日志里一条线索都没有」。
func setupUpdateHelperLog(exe string, args []string) {
	dir := filepath.Dir(updateHelperConfigPath(exe, args))
	if lf, err := newRotatingFile(filepath.Join(dir, "lanet.log"), logMaxSize, logMaxBackups); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{lf}})
	}
}

func updateHelperConfigPath(exe string, args []string) string {
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "-config="); ok {
			return value
		}
		if value, ok := strings.CutPrefix(arg, "--config="); ok {
			return value
		}
		if (arg == "-config" || arg == "--config") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return filepath.Join(filepath.Dir(exe), "lanet.json")
}

func spawnUpdateHelper() error {
	args := append([]string{updateHelperArg}, os.Args[1:]...)
	cmd := exec.Command(selfExe(), args...)
	cmd.Dir = exeDir()
	cmd.SysProcAttr = spawnSysProcAttr
	return cmd.Start()
}
