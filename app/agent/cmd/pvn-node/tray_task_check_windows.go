//go:build windows

package main

// 已注册托盘任务「定义是否过期」的判定。
//
// 背景（2026-10-26 真机故障）：lanet.exe 内嵌 requireAdministrator 清单，托盘要由
// 任务计划程序拉起，任务定义必须带 <RunLevel>HighestAvailable</RunLevel>。若任务由
// **未提升的进程**注册（旧版在用户会话里跑控制台、或用户手工 schtasks），Windows 会
// 静默忽略该声明，落成「最低权限」任务：登录时启动直接失败 0x800702E4
// （ERROR_ELEVATION_REQUIRED），托盘图标永远不出现，且没有任何用户可见的报错。
//
// 原实现的服务兜底只判断「任务存在」就直接拉起，这类过期定义永远得不到修正；本文件
// 只做**纯判定**（输入是任务定义文本），便于用真实任务 XML 做回归测试。

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// trayTaskRunLevelHighest 任务计划程序里「以最高权限运行」的取值。
// 最低权限任务不写 RunLevel 元素，因此「取不到 / 不等于它」都视为过期。
const trayTaskRunLevelHighest = "HighestAvailable"

// trayInstalledTask 只解析判定所需字段（其余元素忽略，查询输出里还有大量设置项）。
type trayInstalledTask struct {
	Principals struct {
		Principal struct {
			UserID   string `xml:"UserId"`
			LogonTyp string `xml:"LogonType"`
			RunLevel string `xml:"RunLevel"`
		} `xml:"Principal"`
	} `xml:"Principals"`
	Triggers struct {
		Logon struct {
			UserID string `xml:"UserId"`
		} `xml:"LogonTrigger"`
	} `xml:"Triggers"`
	Actions struct {
		Exec struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

// decodeTrayTaskXMLBytes 把 schtasks 的原始输出归一化成合法 UTF-8 文本。
//
// 三种真实形态都要吃得下：UTF-16LE（带/不带 BOM，任务定义文件本身是这个格式）、
// UTF-16BE（BOM）、以及 stdout 的本地代码页字节（中文任务描述在 zh-CN 上是 GBK，
// 会含非法 UTF-8 序列）。判定只需要 ASCII 字段，非法字节替换掉即可。
func decodeTrayTaskXMLBytes(raw []byte) string {
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return utf16BytesString(raw[2:], false)
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return utf16BytesString(raw[2:], true)
	case len(raw) >= 2 && raw[0] == 0x3C && raw[1] == 0x00:
		return utf16BytesString(raw, false) // 无 BOM 的 UTF-16LE
	}
	return strings.ToValidUTF8(string(raw), "?")
}

// utf16BytesString 把 UTF-16 字节串解码成 UTF-8 文本（be=true 按大端读取）。
func utf16BytesString(b []byte, be bool) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if be {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(units))
}

// parseTrayInstalledTask 解析任务定义文本；空串或非法 XML 返回错误。
func parseTrayInstalledTask(text string) (trayInstalledTask, error) {
	var t trayInstalledTask
	if strings.TrimSpace(text) == "" {
		return t, fmt.Errorf("任务定义为空")
	}
	// 声明写的是 UTF-16，而字节已由 decodeTrayTaskXMLBytes 归一化为 UTF-8；
	// 这里给一个透传的 CharsetReader，避免解码器在声明处直接报错。
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := dec.Decode(&t); err != nil {
		return t, err
	}
	return t, nil
}

// trayTaskStaleReason 判定已注册定义与期望定义是否不一致，返回中文原因（空串=一致）。
//
// wantUser 为空表示调用方还不知道当前交互登录用户（开机场景服务先于登录启动），
// 此时不比较触发用户——身份未知不能当成「不一致」，否则会误删/误重建他人任务。
func trayTaskStaleReason(t trayInstalledTask, wantExe, wantConfig, wantUser string) string {
	if !strings.EqualFold(strings.TrimSpace(t.Principals.Principal.RunLevel), trayTaskRunLevelHighest) {
		return "任务未声明以最高权限运行，登录启动会因需要提升而失败（0x800702E4）"
	}
	if !sameTrayPath(t.Actions.Exec.Command, wantExe) {
		return fmt.Sprintf("程序路径已变（任务=%q 期望=%q）", t.Actions.Exec.Command, wantExe)
	}
	if !containsTrayArg(t.Actions.Exec.Arguments, trayModeArg) ||
		!containsTrayArg(t.Actions.Exec.Arguments, wantConfig) {
		return fmt.Sprintf("启动参数已变（任务=%q）", t.Actions.Exec.Arguments)
	}
	// Principals 里的 UserId 在查询输出中是 SID（S-1-5-21-...），触发器里是
	// 「域\用户名」；只比较触发器一侧，避免把同一个用户误判成换人。
	if wantUser != "" && !strings.EqualFold(strings.TrimSpace(t.Triggers.Logon.UserID), strings.TrimSpace(wantUser)) {
		return fmt.Sprintf("触发用户已变（任务=%q 期望=%q）", t.Triggers.Logon.UserID, wantUser)
	}
	return ""
}

// sameTrayPath 比较路径（Windows 路径大小写不敏感）。
func sameTrayPath(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// containsTrayArg 参数串里是否含有某个参数（大小写不敏感）。只做包含判断：
// schtasks 对引号的转义细节无法完全复现，硬比整串反而会把正确的任务误判成过期。
func containsTrayArg(args, want string) bool {
	if want == "" {
		return true
	}
	return strings.Contains(strings.ToLower(args), strings.ToLower(want))
}
