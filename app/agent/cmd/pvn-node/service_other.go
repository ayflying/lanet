//go:build !windows

package main

import (
	"context"
	"errors"
)

func handleWindowsServiceCommand() (bool, error) { return false, nil }

func runAsSystemService(func(context.Context)) (bool, error) { return false, nil }

func isServiceProcess() bool { return false }

func restartWindowsService() error { return errors.New("当前平台不是 Windows 服务") }

func isWindowsServiceInstalled() bool { return false }

func installWindowsService() error {
	return errors.New("当前平台不支持 Windows 服务（Linux 请使用 systemd，容器请使用 restart 策略）")
}

func removeWindowsService() error {
	return errors.New("当前平台不支持 Windows 服务")
}
