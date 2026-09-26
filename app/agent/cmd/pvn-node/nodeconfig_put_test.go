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
