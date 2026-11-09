//go:build linux

package main

// The image binary remains the supervisor for its entire lifetime. Updated
// binaries are immutable, content-addressed children, never replacements of PID 1.
import (
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

const containerRestartExitCode = 42

type containerSupervisorOptions struct {
	ReadyTimeout time.Duration
	StableWindow time.Duration
	StopTimeout  time.Duration
	PollInterval time.Duration
	PublicKey    string
}

func runContainerSupervisor() bool {
	if !containerized() || os.Getenv("LANET_CONTAINER_CHILD") == "1" {
		return false
	}
	dir := containerRuntimeDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("[container] cannot create runtime directory: %v", err)
		return true
	}
	lock, err := os.OpenFile(filepath.Join(dir, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Printf("[container] runtime lock: %v", err)
		return true
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Printf("[container] runtime volume is already supervised: %v", err)
		return true
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	opts := containerSupervisorOptions{60 * time.Second, 30 * time.Second, 10 * time.Second, 100 * time.Millisecond, selfupdate.ReleasePublicKey}
	containerSupervisorLoop(dir, selfExe(), version, os.Args[1:], signals, opts)
	return true
}

func containerSupervisorBackoff(failures int) time.Duration {
	if failures < 1 {
		return time.Second
	}
	if failures > 6 {
		return 30 * time.Second
	}
	delay := time.Second << (failures - 1)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func containerSupervisorLoop(dir, image, imageVersion string, args []string, signals <-chan os.Signal, opts containerSupervisorOptions) {
	imageSource := containerSupervisorSource{Path: image}
	fallback := imageSource
	failed := make(map[string]containerSupervisorFailure)
	if err := containerSupervisorReadJSON(filepath.Join(dir, "failed.json"), &failed); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Fail closed for candidate execution: do not lose the do-not-retry ledger.
		log.Printf("[container] unreadable failure ledger; candidates disabled: %v", err)
		failed = nil
	}
	if failed == nil { // A literal JSON null is also an invalid ledger.
		log.Printf("[container] candidate trials disabled until failure ledger is repaired")
	}
	failures := 0
	for {
		select {
		case <-signals:
			return
		default:
		}
		if fallback.Manifest == nil {
			var previous selfupdate.Manifest
			if containerSupervisorReadJSON(filepath.Join(dir, "last-good.json"), &previous) == nil && versionLess(imageVersion, previous.Version) && failed[previous.SHA256].Reason == "" {
				if path, err := containerSupervisorValidate(dir, previous, opts.PublicKey, "linux/"+runtime.GOARCH); err == nil {
					fallback = containerSupervisorSource{path, &previous}
				}
			}
		}
		// Re-verify active on every launch. A corrupt new pointer must not displace
		// the last known-good launch source, but a corrupt binary is never executed.
		var active selfupdate.Manifest
		if err := containerSupervisorReadJSON(filepath.Join(dir, "active.json"), &active); err == nil && versionLess(imageVersion, active.Version) {
			// active仅在稳定窗口完成后写入，优先于未清理的trial隔离记录。
			record, blocked := failed[active.SHA256]
			if (!blocked || record.Reason == "trial interrupted before confirmation") && (fallback.Manifest == nil || !versionLess(active.Version, fallback.Manifest.Version)) {
				if path, err := containerSupervisorValidate(dir, active, opts.PublicKey, "linux/"+runtime.GOARCH); err == nil {
					fallback = containerSupervisorSource{path, &active}
				} else {
					log.Printf("[container] ignoring invalid active release: %v", err)
				}
			}
		}
		if fallback.Manifest != nil {
			if _, err := containerSupervisorValidate(dir, *fallback.Manifest, opts.PublicKey, "linux/"+runtime.GOARCH); err != nil {
				log.Printf("[container] previous active binary invalid, using image: %v", err)
				fallback = imageSource
			}
		}
		source := fallback
		trial := false
		var candidate selfupdate.Manifest
		if err := containerSupervisorReadJSON(filepath.Join(dir, "candidate.json"), &candidate); err == nil && failed != nil {
			if _, bad := failed[candidate.SHA256]; !bad && containerSupervisorNewer(candidate, imageVersion, fallback.Manifest) {
				if path, err := containerSupervisorValidate(dir, candidate, opts.PublicKey, "linux/"+runtime.GOARCH); err == nil {
					source = containerSupervisorSource{path, &candidate}
					trial = true
				} else {
					log.Printf("[container] ignoring invalid candidate (keeping launch source): %v", err)
				}
			}
		}
		if trial {
			// 先写失败隔离，再试运行。父进程被强杀或容器重建时不会无限重试坏版。
			failed[candidate.SHA256] = containerSupervisorFailure{"trial interrupted before confirmation", time.Now().UTC()}
			if err := containerSupervisorWriteJSON(filepath.Join(dir, "failed.json"), failed); err != nil {
				log.Printf("[container] cannot persist trial intent: %v", err)
				source, trial = fallback, false
			}
		}
		result := containerSupervisorRunChild(dir, source, args, trial, signals, opts, func() error {
			if fallback.Manifest != nil {
				if err := containerSupervisorWriteJSON(filepath.Join(dir, "last-good.json"), fallback.Manifest); err != nil {
					return err
				}
			}
			if err := containerSupervisorWriteJSON(filepath.Join(dir, "active.json"), candidate); err != nil {
				return err
			}
			// active 是健康确认提交点，先提交再移除试运行隔离。
			// 崩溃在二者之间时，启动扫描可识别已确认active而不会重复试运行。
			delete(failed, candidate.SHA256)
			if err := containerSupervisorWriteJSON(filepath.Join(dir, "failed.json"), failed); err != nil {
				log.Printf("[container] active committed; ledger cleanup deferred: %v", err)
			}
			fallback = source
			// A newer candidate could have been published while this trial was running.
			var current selfupdate.Manifest
			if err := containerSupervisorReadJSON(filepath.Join(dir, "candidate.json"), &current); err == nil && current == candidate {
				if err := os.Remove(filepath.Join(dir, "candidate.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("[container] clear candidate: %v", err)
				}
			}
			return nil
		})
		if result.Stopped {
			return
		}
		if result.FailedTrial {
			failed[candidate.SHA256] = containerSupervisorFailure{result.Reason, time.Now().UTC()}
			if err := containerSupervisorWriteJSON(filepath.Join(dir, "failed.json"), failed); err != nil {
				// In-memory exclusion still prevents a crash loop during this parent life.
				log.Printf("[container] cannot persist failed candidate: %v", err)
			}
			log.Printf("[container] candidate %s failed (%s); falling back", candidate.SHA256, result.Reason)
			continue
		}
		if result.ExitCode == 0 {
			return
		}
		if result.ExitCode == containerRestartExitCode {
			failures = 0
			// Bound even repeated planned-restart exits (e.g. a broken restart flag).
			if !containerSupervisorWait(time.Second, signals) {
				return
			}
			continue
		}
		if result.Uptime >= time.Minute {
			failures = 0
		}
		failures++
		if failures >= 3 && source.Manifest != nil {
			// 已确认版若仍连续早退，隔离该摘要并回退，不无限崩溃重启。
			if failed == nil {
				failed = make(map[string]containerSupervisorFailure)
			}
			failed[source.Manifest.SHA256] = containerSupervisorFailure{"confirmed release repeatedly crashed", time.Now().UTC()}
			_ = containerSupervisorWriteJSON(filepath.Join(dir, "failed.json"), failed)
			var previous selfupdate.Manifest
			fallback = imageSource
			if containerSupervisorReadJSON(filepath.Join(dir, "last-good.json"), &previous) == nil && failed[previous.SHA256].Reason == "" && versionLess(imageVersion, previous.Version) {
				if path, err := containerSupervisorValidate(dir, previous, opts.PublicKey, "linux/"+runtime.GOARCH); err == nil {
					fallback = containerSupervisorSource{path, &previous}
				}
			}
			if fallback.Manifest != nil {
				_ = containerSupervisorWriteJSON(filepath.Join(dir, "active.json"), fallback.Manifest)
			} else {
				_ = os.Remove(filepath.Join(dir, "active.json"))
			}
			failures = 0
		}
		delay := containerSupervisorBackoff(failures)
		log.Printf("[container] node exited (%d, %s); restarting in %s", result.ExitCode, result.Reason, delay)
		if !containerSupervisorWait(delay, signals) {
			return
		}
	}
}
