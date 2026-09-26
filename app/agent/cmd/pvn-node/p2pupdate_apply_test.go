package main

import (
	"errors"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

func TestApplyP2PUpdateLockedInstallFailureReleasesGate(t *testing.T) {
	if updateInFlight() {
		t.Fatal("测试开始时闸门已占用")
	}
	if !acquireUpdate() {
		t.Fatal("无法占用更新闸门")
	}
	t.Cleanup(func() {
		if updateInFlight() {
			releaseUpdate()
		}
	})

	oldInstall, oldRestart := installP2PBinary, restartP2PProcess
	t.Cleanup(func() { installP2PBinary, restartP2PProcess = oldInstall, oldRestart })
	installCalls, restartCalls := 0, 0
	installP2PBinary = func(newPath, exePath string) error {
		installCalls++
		if newPath != "candidate.exe" || exePath != "lanet.exe" {
			t.Fatalf("安装参数错误: %q -> %q", newPath, exePath)
		}
		return errors.New("replace failed")
	}
	restartP2PProcess = func(time.Duration) { restartCalls++ }

	applyP2PUpdateLocked("candidate.exe", selfupdate.Manifest{Version: "1.2.3"}, "lanet.exe")
	if installCalls != 1 {
		t.Fatalf("应委托安装一次，实际 %d 次", installCalls)
	}
	if restartCalls != 0 {
		t.Fatalf("替换失败不应重启，实际 %d 次", restartCalls)
	}
	if updateInFlight() {
		t.Fatal("替换失败后应释放更新闸门")
	}
}

func TestApplyP2PUpdateLockedSuccessKeepsGateAndSchedulesRestart(t *testing.T) {
	if updateInFlight() {
		t.Fatal("测试开始时闸门已占用")
	}
	if !acquireUpdate() {
		t.Fatal("无法占用更新闸门")
	}
	t.Cleanup(func() {
		if updateInFlight() {
			releaseUpdate()
		}
	})

	oldInstall, oldRestart := installP2PBinary, restartP2PProcess
	t.Cleanup(func() { installP2PBinary, restartP2PProcess = oldInstall, oldRestart })
	installCalls, restartCalls := 0, 0
	var restartDelay time.Duration
	installP2PBinary = func(newPath, exePath string) error {
		installCalls++
		if newPath != "candidate.exe" || exePath != "lanet.exe" {
			t.Fatalf("安装参数错误: %q -> %q", newPath, exePath)
		}
		return nil
	}
	restartP2PProcess = func(delay time.Duration) {
		restartCalls++
		restartDelay = delay
	}

	applyP2PUpdateLocked("candidate.exe", selfupdate.Manifest{Version: "1.2.3"}, "lanet.exe")
	if installCalls != 1 {
		t.Fatalf("应委托安装一次，实际 %d 次", installCalls)
	}
	if restartCalls != 1 || restartDelay < time.Minute || restartDelay >= 8*time.Minute {
		t.Fatalf("成功后应安排一次 1~8 分钟内的重启，次数=%d 延时=%v", restartCalls, restartDelay)
	}
	if !updateInFlight() {
		t.Fatal("替换成功、等待重启期间应保持更新闸门")
	}
}
