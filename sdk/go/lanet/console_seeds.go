package lanet

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
)

// =================================================================================
// 控制台：种子表接口
//
// 单独一个文件、只往 console.go 里加一行注册，避免与「单文件内嵌 UI」那批
// 改动互相踩踏。UI 本身在 console/index.html（另一处改动面），这里只保证
// 接口先可用、可验收。
// =================================================================================

// registerSeedRoutes 注册种子表相关路由。
func (c *Client) registerSeedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/seed-settings", c.apiSeedSettings)
	mux.HandleFunc("PUT /api/seed-settings", c.apiSetSeedSettings)
	mux.HandleFunc("GET /api/seeds", c.apiSeeds)
	mux.HandleFunc("POST /api/seeds", c.apiDeleteSeed)
}

// seedOverview 一个范围的概览（统计数字由 DB 现算，不做缓存——
// 种子表上限 1000 条量级，COUNT 成本可以忽略，缓存反而会显示过期数字）。
type seedOverview struct {
	Scope           string `json:"scope"`
	Enabled         bool   `json:"enabled"`
	Limit           int    `json:"limit"`
	Total           int    `json:"total"`
	Verified        int    `json:"verified"`
	PublicReachable int    `json:"public_reachable"`
}

// apiSeedSettings 读取种子交换设置与两张表的概览。
func (c *Client) apiSeedSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	settings, err := c.SeedSettings(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{
		"settings": settings,
		"db_path":  c.PeersDBPath(),
	}
	if c.peers != nil {
		overviews := make([]seedOverview, 0, 2)
		overviews = append(overviews, c.seedOverviewOf(ctx, peersdb.SeedScopeGroup, settings.GroupEnabled, peersdb.DefaultGroupSeedLimit))
		overviews = append(overviews, c.seedOverviewOf(ctx, peersdb.SeedScopeGlobal, settings.GlobalEnabled, settings.GlobalLimit))
		out["scopes"] = overviews
	}
	writeJSON(w, http.StatusOK, out)
}

// seedOverviewOf 汇总单个范围的统计。
func (c *Client) seedOverviewOf(ctx context.Context, scope peersdb.SeedScope, enabled bool, limit int) seedOverview {
	ov := seedOverview{Scope: string(scope), Enabled: enabled, Limit: limit}
	seeds, err := c.peers.ListSeeds(ctx, scope, 0)
	if err != nil {
		return ov
	}
	ov.Total = len(seeds)
	for _, s := range seeds {
		if s.Verified() {
			ov.Verified++
		}
		if s.PublicReachable {
			ov.PublicReachable++
		}
	}
	return ov
}

// apiSetSeedSettings 保存设置；保存后立即生效（SetSeedSettings 内部会重装回调）。
func (c *Client) apiSetSeedSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupEnabled  *bool `json:"group_enabled"`
		GlobalEnabled *bool `json:"global_enabled"`
		GlobalLimit   *int  `json:"global_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// 先读现值，只覆盖请求里显式给出的字段（局部更新语义）。
	next, err := c.SeedSettings(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if req.GroupEnabled != nil {
		next.GroupEnabled = *req.GroupEnabled
	}
	if req.GlobalEnabled != nil {
		next.GlobalEnabled = *req.GlobalEnabled
	}
	if req.GlobalLimit != nil {
		next.GlobalLimit = *req.GlobalLimit
	}
	saved, err := c.SetSeedSettings(ctx, next)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c.logf("种子交换设置已更新：群内=%v 全域=%v 全域上限=%d",
		saved.GroupEnabled, saved.GlobalEnabled, saved.GlobalLimit)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": saved})
}

// apiSeeds 列出某个范围的种子（默认群内；scope=global 切到全域）。
func (c *Client) apiSeeds(w http.ResponseWriter, r *http.Request) {
	if c.peers == nil {
		writeJSON(w, http.StatusOK, map[string]any{"seeds": []any{}, "db_path": ""})
		return
	}
	scope := peersdb.SeedScope(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = peersdb.SeedScopeGroup
	}
	if !peersdb.ValidSeedScope(scope) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "scope 只能是 group 或 global"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	seeds, err := c.peers.ListSeeds(ctx, scope, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": string(scope), "seeds": seeds})
}

// apiDeleteSeed 删掉一条种子（手动清理误入的脏记录）。
func (c *Client) apiDeleteSeed(w http.ResponseWriter, r *http.Request) {
	if c.peers == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未启用地址簿"})
		return
	}
	var req struct {
		Scope  string `json:"scope"`
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	scope := peersdb.SeedScope(req.Scope)
	if scope == "" {
		scope = peersdb.SeedScopeGroup
	}
	if !peersdb.ValidSeedScope(scope) || req.PeerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要合法的 scope 与 peer_id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.peers.DeleteSeed(ctx, scope, req.PeerID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": string(scope), "peer_id": req.PeerID})
}
