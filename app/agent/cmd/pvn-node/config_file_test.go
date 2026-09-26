package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadNodeConfigToleratesBOM 配置文件带 UTF-8 BOM（Windows 记事本 /
// PowerShell 5.1 的 Set-Content -Encoding UTF8 都会写）时必须照常解析：
// 否则节点会按默认值启动——换名、换控制台端口、换网络密钥，用户只看到
// 「组里没人」，根本查不到 BOM 上。
func TestLoadNodeConfigToleratesBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.json")
	raw, err := json.Marshal(map[string]any{
		"name":             "bom-node",
		"network_key":      "bom-net",
		"console":          "127.0.0.1:18999",
		"require_approval": false,
	})
	if err != nil {
		t.Fatalf("构造配置失败: %v", err)
	}
	withBOM := append(append([]byte(nil), utf8BOM...), raw...)
	if err := os.WriteFile(path, withBOM, 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	nc, created := loadNodeConfig(path)
	if created {
		t.Fatal("已有配置文件不应生成模板")
	}
	if nc.Name != "bom-node" {
		t.Fatalf("节点名应来自配置文件，实际 %q（BOM 未被容忍，回退到了默认值）", nc.Name)
	}
	if nc.NetworkKey == nil || *nc.NetworkKey != "bom-net" {
		t.Fatalf("网络密钥应来自配置文件，实际 %v", nc.NetworkKey)
	}
	if nc.Console != "127.0.0.1:18999" {
		t.Fatalf("控制台地址应来自配置文件，实际 %q", nc.Console)
	}
	if nc.RequireApproval == nil || *nc.RequireApproval {
		t.Fatalf("require_approval=false 应被解析，实际 %v", nc.RequireApproval)
	}
	// 只读：解析成功时不该回写文件，BOM 必须原样保留。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("复读配置失败: %v", err)
	}
	if string(after) != string(withBOM) {
		t.Fatal("解析成功时不应改写配置文件")
	}
}

// TestLoadNodeConfigKeepsCorruptFile 解析失败时保留用户原文件：
// 旧实现会把损坏配置直接改写成默认模板，一次误编辑就静默丢掉网络密钥、
// 控制台端口与自定义地址——这比启动失败更难排查。
func TestLoadNodeConfigKeepsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.json")
	corrupt := []byte("{\n  \"network_key\": \"yunloli\",\n  \"name\": 手工改坏了\n}\n")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	nc, created := loadNodeConfig(path)
	if created {
		t.Fatal("解析失败不应报告「已生成模板」")
	}
	if nc.Name != defaultNodeConfig().Name {
		t.Fatalf("解析失败应按默认值运行，实际名 %q", nc.Name)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("复读配置失败: %v", err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("解析失败必须保留原文件，实际内容:\n%s", after)
	}
}

// TestLoadNodeConfigGeneratesTemplateWhenMissing 文件不存在时仍然生成模板，
// 且生成的模板能原样读回（首次双击启动的路径）。
func TestLoadNodeConfigGeneratesTemplateWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanet.json")
	nc, created := loadNodeConfig(path)
	if !created {
		t.Fatal("文件不存在时应生成默认模板")
	}
	if nc.Name == "" {
		t.Fatal("默认配置必须有节点名")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("默认模板应落盘: %v", err)
	}
	again, created2 := loadNodeConfig(path)
	if created2 {
		t.Fatal("模板已存在时不应再次生成")
	}
	if again.Name != nc.Name {
		t.Fatalf("模板回读不一致: %q != %q", again.Name, nc.Name)
	}
}

// TestReadNodeConfigFileErrors 读不出/非法 JSON 必须报错，交给调用方降级，
// 不能悄悄返回空配置（控制台「节点配置」页会显示成空值）。
func TestReadNodeConfigFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := readNodeConfigFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("文件不存在应报错")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := readNodeConfigFile(bad); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestDecodeConfigJSONTolerantAnyStruct decodeConfigJSON 是通用入口（节点配置与
// 托盘精简结构都走它），这里用测试本地结构断言 BOM 容忍与字段解析：不能引用
// 托盘那边的类型，它是 windows-only 文件里的（曾因此在 Linux CI 上构建失败）。
func TestDecodeConfigJSONTolerantAnyStruct(t *testing.T) {
	var cfg struct {
		Console         string `json:"console"`
		ConsolePassword string `json:"console_password"`
	}
	raw := append(append([]byte(nil), utf8BOM...), []byte(`{"console":"127.0.0.1:8900","console_password":"pw"}`)...)
	if err := decodeConfigJSON(raw, &cfg); err != nil {
		t.Fatalf("带 BOM 的配置应能解析: %v", err)
	}
	if cfg.Console != "127.0.0.1:8900" || cfg.ConsolePassword != "pw" {
		t.Fatalf("解析结果不对: %+v", cfg)
	}
}
