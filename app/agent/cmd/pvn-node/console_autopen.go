package main

// shouldAutoOpenConsole 只允许创建默认配置文件的首次启动自动打开控制台。
// 升级重启、控制台重启以及退出后再次启动时，配置文件都已存在。
func shouldAutoOpenConsole(configCreated bool) bool { return configCreated }
