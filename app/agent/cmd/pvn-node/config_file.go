package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
)

// utf8BOM Windows 记事本「另存为 UTF-8」与 PowerShell 5.1 的
// Set-Content -Encoding UTF8 都会在文件开头写入 UTF-8 BOM。encoding/json
// 不接受 BOM，不剥离的后果是「文件在、内容也对」仍被判成损坏配置：节点按
// 默认值启动（换节点名、换控制台端口、换网络密钥），用户只看到「组里没人」，
// 几乎不可能联想到是 BOM。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// decodeConfigJSON 解析节点配置 JSON，容忍文件开头的 UTF-8 BOM。
func decodeConfigJSON(data []byte, v any) error {
	return json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), v)
}

// readNodeConfigFile 读取并解析节点配置文件（BOM 容忍）。文件不存在、
// 读不出或 JSON 非法时返回错误，由调用方决定如何降级。
func readNodeConfigFile(path string) (*nodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var nc nodeConfig
	if err := decodeConfigJSON(data, &nc); err != nil {
		return nil, err
	}
	return &nc, nil
}

// loadNodeConfig 读取配置文件；文件不存在时生成默认模板（双击启动的场景）。
//
// 文件存在但解析失败时**保留原文件**：只按默认值运行并把原因打出来。
// 早期实现会把损坏的配置直接改写成默认模板——一次误编辑（或上面说的 BOM）
// 就会静默丢掉用户填的网络密钥、控制台端口与自定义地址，比启动失败更难查。
func loadNodeConfig(path string) (*nodeConfig, bool) {
	if _, err := os.Stat(path); err == nil {
		nc, derr := readNodeConfigFile(path)
		if derr == nil {
			return nc, false
		}
		log.Printf("[node] 配置文件解析失败，本次按默认值运行（原文件已保留，请修正或删除后重新生成）: %s: %v", path, derr)
		return defaultNodeConfig(), false
	}
	nc := defaultNodeConfig()
	if err := nc.save(path); err != nil {
		log.Printf("[node] 默认配置文件生成失败（不影响启动）: %v", err)
	}
	return nc, true
}
