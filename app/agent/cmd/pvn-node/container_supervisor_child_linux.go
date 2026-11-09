//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// signalContainerReady is deliberately file based: stdout is not a protocol and
// a restarted child must not satisfy an earlier child's readiness handshake.
func signalContainerReady() {
	if os.Getenv("LANET_CONTAINER_CHILD") != "1" {
		return
	}
	if path := os.Getenv("LANET_CONTAINER_READY"); path != "" {
		if err := containerSupervisorWriteJSON(path, struct {
			PID int `json:"pid"`
		}{os.Getpid()}); err != nil {
			log.Printf("[container] cannot signal ready: %v", err)
		}
	}
}

type containerSupervisorResult struct {
	ExitCode    int
	Uptime      time.Duration
	Stopped     bool
	FailedTrial bool
	Reason      string
}

func containerSupervisorWait(delay time.Duration, signals <-chan os.Signal) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-signals:
		return false
	case <-timer.C:
		return true
	}
}

func containerSupervisorRunChild(dir string, source containerSupervisorSource, args []string, trial bool, signals <-chan os.Signal, opts containerSupervisorOptions, confirm func() error) containerSupervisorResult {
	result := containerSupervisorResult{ExitCode: -1, FailedTrial: trial}
	readyFile, err := os.CreateTemp(dir, ".ready-*")
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	ready := readyFile.Name()
	readyFile.Close()
	os.Remove(ready)
	defer os.Remove(ready)
	cmd := exec.Command(source.Path, args...)
	cmd.Env = containerSupervisorChildEnv(os.Environ(), dir, ready, containerConfigPath(selfExe(), args, os.Getenv("LANET_CONFIG")))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		result.Reason = err.Error()
		return result
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(opts.PollInterval)
	defer tick.Stop()
	readyDeadline := start.Add(opts.ReadyTimeout)
	var stableAt time.Time
	for {
		select {
		case sig := <-signals:
			containerSupervisorStopChild(cmd, done, sig, opts.StopTimeout)
			result.Stopped = true
			return result
		case err := <-done:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			result.Uptime = time.Since(start)
			result.ExitCode = cmd.ProcessState.ExitCode()
			if err != nil {
				result.Reason = err.Error()
			} else {
				result.Reason = "child exited"
			}
			return result
		case now := <-tick.C:
			if !trial {
				continue
			}
			if stableAt.IsZero() {
				var ack struct {
					PID int `json:"pid"`
				}
				info, statErr := os.Lstat(ready)
				if statErr == nil && info.Mode().IsRegular() && containerSupervisorReadJSON(ready, &ack) == nil && ack.PID == cmd.Process.Pid {
					stableAt = now.Add(opts.StableWindow)
					log.Printf("[container] candidate ready; observing stability for %s", opts.StableWindow)
				} else if !now.Before(readyDeadline) {
					containerSupervisorStopChild(cmd, done, syscall.SIGTERM, opts.StopTimeout)
					result.Reason = "ready timeout"
					return result
				}
			} else if !now.Before(stableAt) {
				// Prefer a concurrently delivered exit over promoting a dead child.
				select {
				case err := <-done:
					result.ExitCode = cmd.ProcessState.ExitCode()
					result.Reason = fmt.Sprintf("exited before confirmation: %v", err)
					return result
				default:
				}
				if err := confirm(); err != nil {
					containerSupervisorStopChild(cmd, done, syscall.SIGTERM, opts.StopTimeout)
					result.Reason = "persist active: " + err.Error()
					return result
				}
				trial = false
				result.FailedTrial = false
				log.Printf("[container] release confirmed active")
			}
		}
	}
}

// Signal the process group, reap the direct child, then force-kill the group if
// shutdown stalls. The supervisor itself is in a different process group.
func containerSupervisorStopChild(cmd *exec.Cmd, done <-chan error, sig os.Signal, timeout time.Duration) {
	unixSig, ok := sig.(syscall.Signal)
	if !ok {
		unixSig = syscall.SIGTERM
	}
	_ = syscall.Kill(-cmd.Process.Pid, unixSig)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		// Do not leave child-spawned helpers behind when their leader exits first.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}
