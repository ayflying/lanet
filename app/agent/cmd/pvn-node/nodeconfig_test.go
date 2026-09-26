package main

import (
	"os"
	"testing"
)

// TestDefaultNodeConfigPrefersLANETName 默认节点名必须优先取 LANET_NAME。
//
// 背景：容器里 os.Hostname() 是容器短 ID（12 位十六进制），旧实现把它直接写进
// 首次生成的 lanet.json，而运行时名字被 LANET_NAME 覆盖，于是控制台页眉显示
// fnos、节点配置页却显示 52b57e329f43——同一台节点两个名字。
func TestDefaultNodeConfigPrefersLANETName(t *testing.T) {
	t.Setenv("LANET_NAME", "fnos")
	nc := defaultNodeConfig()
	if nc.Name != "fnos" {
		t.Fatalf("默认节点名应取 LANET_NAME=fnos，实际 %q", nc.Name)
	}
}

// TestDefaultNodeConfigTrimsLANETName LANET_NAME 首尾空白不应带进配置。
func TestDefaultNodeConfigTrimsLANETName(t *testing.T) {
	t.Setenv("LANET_NAME", "  fnos  ")
	if nc := defaultNodeConfig(); nc.Name != "fnos" {
		t.Fatalf("默认节点名应去掉首尾空白，实际 %q", nc.Name)
	}
}

// TestDefaultNodeConfigFallsBackToHostname 未设 LANET_NAME 时退回主机名（非容器场景的既有行为）。
func TestDefaultNodeConfigFallsBackToHostname(t *testing.T) {
	t.Setenv("LANET_NAME", "")
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("本机拿不到主机名，跳过")
	}
	if nc := defaultNodeConfig(); nc.Name != host {
		t.Fatalf("未设 LANET_NAME 时应退回主机名 %q，实际 %q", host, nc.Name)
	}
}
