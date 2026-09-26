//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName        = "Lanet"
	windowsServiceDisplayName = "Lanet 虚拟局域网"
)

// runAsSystemService 在命令行含 -service 时由 Windows SCM 接管进程。
// 返回 handled=true 表示当前进程是服务入口，调用方不再进入交互模式。
func runAsSystemService(run func(context.Context)) (handled bool, err error) {
	if !hasServiceArg(os.Args[1:]) {
		return false, nil
	}
	// flag.Parse 不认识内部参数 -service；在 SCM handler 启动普通节点逻辑前剔除。
	clean := os.Args[:1]
	for _, arg := range os.Args[1:] {
		if !strings.EqualFold(arg, "-service") && !strings.EqualFold(arg, "--service") {
			clean = append(clean, arg)
		}
	}
	os.Args = clean
	_ = os.Setenv("LANET_SERVICE", "1")
	return true, svc.Run(windowsServiceName, &lanetService{run: run})
}

func hasServiceArg(args []string) bool {
	for _, arg := range args {
		if strings.EqualFold(arg, "-service") || strings.EqualFold(arg, "--service") {
			return true
		}
	}
	return false
}

// handleWindowsServiceCommand 执行内部的服务重启辅助命令。
func handleWindowsServiceCommand() (bool, error) {
	if len(os.Args) != 2 || !strings.EqualFold(os.Args[1], "-service-restart") {
		return false, nil
	}
	// 辅助进程由服务以 DETACHED_PROCESS 派生、无 stdio（Go 默认接到
	// os.DevNull），不重定向日志的话它的失败原因会彻底消失——现场表现
	// 就是「更新后没重启，日志里一条线索都没有，只能靠猜」。
	setupServiceHelperLog()
	log.Printf("[service-restart] 收到重启请求（pid=%d），通过服务管理器 stop/start", os.Getpid())
	m, s, err := openService()
	if err != nil {
		log.Printf("[service-restart] 打开服务失败: %v", err)
		return true, err
	}
	defer m.Disconnect()
	defer s.Close()
	if status, qerr := s.Query(); qerr == nil && status.State != svc.Stopped {
		log.Printf("[service-restart] 停止服务（当前状态=0x%x）", status.State)
		if _, err = s.Control(svc.Stop); err != nil {
			log.Printf("[service-restart] 停止服务失败: %v", err)
			return true, fmt.Errorf("停止服务失败: %w", err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			status, qerr = s.Query()
			if qerr == nil && status.State == svc.Stopped {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if status.State != svc.Stopped {
			log.Printf("[service-restart] 等待服务停止超时（状态=0x%x）", status.State)
			return true, errors.New("等待服务停止超时")
		}
	}
	// applyPendingUpdate 自己会在替换前把当前程序备份到 lanet.exe.rollback，
	// 这里不再另做一次备份：多出来的那份在应用失败（标记损坏/候选缺失/校验不过）
	// 时不会被清理，会留下指向「当前仍在运行的版本」的假回滚点，污染后续状态判断。
	exePath := selfExe()
	updated, updateErr := applyPendingUpdate(exePath)
	if updateErr != nil {
		log.Printf("[service-restart] 待更新程序无法应用，保留当前版本并继续启动: %v", updateErr)
	} else if updated {
		log.Printf("[service-restart] 已在服务启动前切换到暂存版本")
	}
	if err = s.Start(); err != nil {
		log.Printf("[service-restart] 启动服务失败: %v", err)
		if updated {
			if rollbackErr := restoreBackup(selfExe()); rollbackErr != nil {
				return true, fmt.Errorf("启动新版本服务失败: %w（恢复旧版本失败: %v）", err, rollbackErr)
			}
			log.Printf("[service-restart] 已恢复旧版本，重试启动服务")
			if retryErr := s.Start(); retryErr != nil {
				return true, fmt.Errorf("启动新版本服务失败: %w；恢复旧版本后启动仍失败: %v", err, retryErr)
			}
			log.Printf("[service-restart] 旧版本服务已启动")
			return true, nil
		}
		return true, fmt.Errorf("启动服务失败: %w", err)
	}
	log.Printf("[service-restart] 服务已重新启动")
	return true, nil
}

// setupServiceHelperLog 把辅助进程的标准日志接到节点日志文件，锚点与主进程
// 一致（配置文件目录；无 -config 参数时退回 exe 目录——服务场景下二者相同）。
func setupServiceHelperLog() {
	dir := filepath.Dir(selfExe())
	for i, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "-config="); ok {
			dir = filepath.Dir(v)
			break
		}
		if (a == "-config" || a == "--config") && i+2 <= len(os.Args[1:]) {
			dir = filepath.Dir(os.Args[1:][i+1])
			break
		}
	}
	if lf, err := newRotatingFile(filepath.Join(dir, "lanet.log"), logMaxSize, logMaxBackups); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{lf}})
	}
}

func isServiceProcess() bool { return os.Getenv("LANET_SERVICE") == "1" }

func restartWindowsService() error {
	cmd := exec.Command(selfExe(), "-service-restart")
	cmd.Dir = filepath.Dir(selfExe())
	cmd.SysProcAttr = spawnSysProcAttr
	return cmd.Start()
}

type lanetService struct {
	run func(context.Context)
}

func (s *lanetService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					log.Printf("[service] 等待节点退出超时，交由 SCM 结束进程")
				}
				return false, 0
			}
		case <-done:
			cancel()
			return false, 0
		}
	}
}

func serviceConfigPath() string {
	for i, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "--config=") {
			return absServicePath(strings.SplitN(arg, "=", 2)[1])
		}
		if (arg == "-config" || arg == "--config") && i+2 <= len(os.Args[1:]) {
			return absServicePath(os.Args[1:][i+1])
		}
	}
	return filepath.Join(filepath.Dir(selfExe()), "lanet.json")
}

func absServicePath(path string) string {
	if path == "" {
		return filepath.Join(filepath.Dir(selfExe()), "lanet.json")
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func serviceBinaryPath() string {
	return fmt.Sprintf("%q -service -config %q", selfExe(), serviceConfigPath())
}

func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, s, nil
}

func isWindowsServiceInstalled() bool {
	m, s, err := openService()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	defer s.Close()
	cfg, err := s.Config()
	return err == nil && cfg.StartType == mgr.StartAutomatic
}

// installWindowsService 注册 LocalSystem 自动启动服务，并配套注册「用户登录时
// 启动托盘图标」的任务。
//
// 两者是一套：服务保证**不登录也组网**（开机即跑），而服务跑在 Session 0，
// 那边没有任务栏、图标画不出来（详见 tray_mode_windows.go），所以用户会话里
// 的托盘必须由登录时任务补上。当前交互实例继续运行，不立即启动第二份服务实例，
// 避免两个节点抢占 TUN、控制台端口和数据库。
func installWindowsService() error {
	if err := upsertWindowsService(); err != nil {
		return err
	}
	// 托盘任务注册失败不算自启失败：节点已经能开机自启了，坏的只是那个图标。
	// 这里若返回错误，控制台会显示「开机自启设置失败」，反而让人误以为要重来。
	if err := installTrayAutostart(); err != nil {
		log.Printf("[tray] 托盘任务注册失败（服务已装好，开机自启不受影响；登录后不会有托盘图标）: %v", err)
		return nil
	}
	startTrayTaskNow()
	return nil
}

// upsertWindowsService 只做服务注册本身：已存在则更新配置（含 exe/config 路径
// 变化），不存在则创建。
func upsertWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接 Windows 服务管理器失败（请以管理员权限运行）: %w", err)
	}
	defer m.Disconnect()

	if old, err := m.OpenService(windowsServiceName); err == nil {
		defer old.Close()
		cfg, qerr := old.Config()
		if qerr != nil {
			return fmt.Errorf("读取现有服务配置失败: %w", qerr)
		}
		cfg.DisplayName = windowsServiceDisplayName
		cfg.Description = "Lanet 无服务器 P2P 虚拟局域网节点（无需用户登录即可启动）"
		cfg.StartType = mgr.StartAutomatic
		// UpdateConfig 会根据 BinaryPathName 更新服务命令行；用同一转义规则确保
		// exe/config 路径中有空格时仍能由 SCM 正确解析。
		cfg.BinaryPathName = serviceBinaryPath()
		if err = old.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("更新 Windows 服务失败: %w", err)
		}
		return nil
	}

	s, err := m.CreateService(windowsServiceName, selfExe(), mgr.Config{
		DisplayName: windowsServiceDisplayName,
		Description: "Lanet 无服务器 P2P 虚拟局域网节点（无需用户登录即可启动）",
		StartType:   mgr.StartAutomatic,
	}, "-service", "-config", serviceConfigPath())
	if err != nil {
		return fmt.Errorf("安装 Windows 服务失败（请以管理员权限运行）: %w", err)
	}
	return s.Close()
}

// removeWindowsService 删除服务注册。若当前进程本身由该服务启动，Windows 会
// 将服务标记为删除，当前节点继续运行；本进程退出后注册消失，下次开机不再启动。
func removeWindowsService() error {
	// 托盘任务与「开机自启」同生命周期：先拆掉它，否则关了自启之后重新登录
	// 还会冒出一个图标（节点却没在跑），比没有图标更让人困惑。
	if err := removeTrayAutostart(); err != nil {
		log.Printf("[tray] %v", err)
	}
	m, s, err := openService()
	if err != nil {
		// 服务本来就不存在视为成功，保证开关操作幂等。
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return fmt.Errorf("打开 Windows 服务失败: %w", err)
	}
	defer m.Disconnect()
	defer s.Close()
	if err = s.Delete(); err != nil {
		return fmt.Errorf("卸载 Windows 服务失败: %w", err)
	}
	return nil
}
