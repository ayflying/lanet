//go:build !windows

// 非 Windows 平台暂不支持开机自启（Linux 用 systemd unit、容器用 restart
// 策略，各自有成熟的编排机制，无需节点程序内建）。接口与 Windows 版对齐，
// 调用方无需区分平台：查询恒 false，设置恒报不支持。
package main

import "errors"

const autorunEnv = "LANET_AUTORUN"

func isAutorunLaunch() bool { return false }

func isAutorunEnabled() bool { return false }

func autorunSupported() bool { return false }

func setAutorunEnabled(enable bool) error {
	return errors.New("当前平台不支持程序内开机自启（Linux 请使用 systemd unit）")
}
