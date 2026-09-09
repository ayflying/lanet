package lanet

import "runtime"

// defaultOS 返回默认操作系统标识。
func defaultOS() string { return runtime.GOOS }

// platform 返回运行平台描述（如 windows/amd64），cfg.Platform 优先。
func (c *Client) platform() string {
	if c.cfg.Platform != "" {
		return c.cfg.Platform
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}
