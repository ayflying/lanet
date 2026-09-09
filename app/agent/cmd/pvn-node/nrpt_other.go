//go:build !windows

package main

// 非 Windows 平台：NRPT 不存在。macOS 可用 /etc/resolver/lanet，
// Linux 需要改 resolv.conf（侵入性强），官方程序暂不自动配置，
// 仅启动 DNS 服务（SDK 层），用户可手动接入。

func ensureNRPTRule(dnsServer string) error { return nil }

func removeNRPTRule() error { return nil }
