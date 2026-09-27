package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

// TestNodeConfigPutKeepsUnspecifiedFields 整份保存不能把请求体里没带的字段抹掉。
//
// 背景（实测踩过）：PUT /api/node-config 是「整份配置保存」，未提供的字段按零值
// 落盘。当时只想清掉历史 bootstrap 字面量，发的是 {name, console, bootstrap}，
// 结果 network_key 被清空，节点立刻切到按身份派生的专属网络——虚拟 IP 变化、
// 成员表清空，看起来像「网络里其他节点全掉线」。私有仓库令牌同理（控制台根本
// 没有该输入框，每次保存都会丢）。
func TestNodeConfigPutKeepsUnspecifiedFields(t *testing.T) {
	const seed = "/ip4/1.2.3.4/tcp/4001/p2p/12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf"
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.json")

	base := defaultNodeConfig()
	base.Name = "keep-fields"
	base.NetworkKey = strPtr("yunloli")
	base.GitHubToken = "ghp_example"
	base.Bootstrap = strPtr(seed)
	if err := base.save(path); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	h := nodeConfigRoutes(path, nodeRuntime{}, func() *lanet.Client { return nil })["PUT /api/node-config"]
	if h == nil {
		t.Fatal("未注册 PUT /api/node-config")
	}

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPut, "/api/node-config", strings.NewReader(body)))
		return rec
	}

	// ① 完整保存：只显式清空 bootstrap，其余字段（未提供）必须原样保留。
	rec := put(`{"name":"keep-fields","console":"127.0.0.1:8900","bootstrap":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("① 保存失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("① 读回配置失败: %v", err)
	}
	if got.NetworkKey == nil || *got.NetworkKey != "yunloli" {
		t.Fatalf("① network_key 应保留 yunloli，实际 %v", derefStr(got.NetworkKey))
	}
	if got.GitHubToken != "ghp_example" {
		t.Fatalf("① github_token 应保留，实际 %q", got.GitHubToken)
	}
	if got.Bootstrap != nil && *got.Bootstrap != "" {
		t.Fatalf("① bootstrap 应被清空，实际 %q", derefStr(got.Bootstrap))
	}

	// ② 显式空串 = 用户意图清空网络密钥（回到按身份派生的专属网络）。
	rec = put(`{"name":"keep-fields","console":"127.0.0.1:8900","network_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("② 保存失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err = readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("② 读回配置失败: %v", err)
	}
	if got.NetworkKey == nil || *got.NetworkKey != "" {
		t.Fatalf("② network_key 应被显式清空，实际 %v", derefStr(got.NetworkKey))
	}
}

// TestNodeConfigPutDisableDefaultSeed 内置入口开关要能落盘，且未提供时保留原值。
//
// 语义要点：清空连接种子（bootstrap=""）= 使用内置入口；再勾上
// disable_default_seed 才是「真的不连任何入口，只走私有 DHT + mDNS」。
// 两者是两件事，不能互相覆盖。
func TestNodeConfigPutDisableDefaultSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.json")
	base := defaultNodeConfig()
	base.Name = "seed-switch"
	base.NetworkKey = strPtr("yunloli")
	if err := base.save(path); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	h := nodeConfigRoutes(path, nodeRuntime{}, func() *lanet.Client { return nil })["PUT /api/node-config"]
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPut, "/api/node-config", strings.NewReader(body)))
		return rec
	}
	full := func(extra string) string {
		return `{"name":"seed-switch","console":"127.0.0.1:8900"` + extra + `}`
	}

	// ① 勾选开关 + 清空种子：两者都要落盘（清空不等于关闭内置入口）。
	if rec := put(full(`,"bootstrap":"","disable_default_seed":true`)); rec.Code != http.StatusOK {
		t.Fatalf("① 保存失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("① 读回失败: %v", err)
	}
	if got.DisableDefaultSeed == nil || !*got.DisableDefaultSeed {
		t.Fatalf("① disable_default_seed 应为 true，实际 %v", got.DisableDefaultSeed)
	}
	if got.Bootstrap != nil && *got.Bootstrap != "" {
		t.Fatalf("① bootstrap 应被清空，实际 %q", derefStr(got.Bootstrap))
	}

	// ② 请求体没带该字段（旧版控制台）：必须保留 true，不能被零值覆盖。
	if rec := put(full(``)); rec.Code != http.StatusOK {
		t.Fatalf("② 保存失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err = readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("② 读回失败: %v", err)
	}
	if got.DisableDefaultSeed == nil || !*got.DisableDefaultSeed {
		t.Fatalf("② 未提供该字段时应保留 true，实际 %v", got.DisableDefaultSeed)
	}

	// ③ 显式取消勾选：回到「留空即用内置入口」。
	if rec := put(full(`,"disable_default_seed":false`)); rec.Code != http.StatusOK {
		t.Fatalf("③ 保存失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err = readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("③ 读回失败: %v", err)
	}
	if got.DisableDefaultSeed != nil && *got.DisableDefaultSeed {
		t.Fatalf("③ disable_default_seed 应为 false/nil，实际 %v", got.DisableDefaultSeed)
	}
}
