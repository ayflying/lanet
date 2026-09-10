package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestUpdateCacheStale 覆盖 /api/update 的缓存新鲜度判定：
// 未检查过 → 过期；缓存窗口内 → 复用；超窗口 → 重查。
func TestUpdateCacheStale(t *testing.T) {
	// 模拟 /api/update 中的判定：force || !checked || time.Since(checkedAt) > TTL
	stale := func(force, checked bool, checkedAt time.Time) bool {
		return force || !checked || time.Since(checkedAt) > updateCacheTTL
	}
	cases := []struct {
		name           string
		force, checked bool
		checkedAt      time.Time
		want           bool
	}{
		{"未检查过需查", false, false, time.Time{}, true},
		{"刚刚检查过用缓存", false, true, time.Now(), false},
		{"缓存窗口内用缓存", false, true, time.Now().Add(-1 * time.Minute), false},
		{"超过窗口需重查", false, true, time.Now().Add(-10 * time.Minute), true},
		{"force 绕过新鲜缓存", true, true, time.Now(), true},
		{"force 绕过旧缓存", true, true, time.Now().Add(-10 * time.Minute), true},
	}
	for _, c := range cases {
		if got := stale(c.force, c.checked, c.checkedAt); got != c.want {
			t.Errorf("%s: got stale=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestUpdateCacheTTLNotTooLong 防止 TTL 被调回"1 小时"级别：
// 发布新版本后用户点开弹框（不带 force 也应基本新鲜）不该等太久。
func TestUpdateCacheTTLNotTooLong(t *testing.T) {
	if updateCacheTTL > 10*time.Minute {
		t.Fatalf("updateCacheTTL=%v 过长（发布新版本后用户会长时间看不到更新）", updateCacheTTL)
	}
	if updateCacheTTL <= 0 {
		t.Fatalf("updateCacheTTL=%v 非法", updateCacheTTL)
	}
}

// TestFailRefreshesCheckedAt 验证失败路径也刷新时间戳：
// 否则网络抖动时每次请求都会重查，打爆 GitHub API。
func TestFailRefreshesCheckedAt(t *testing.T) {
	old := upd
	defer func() { upd = old }()

	upd = &updateState{current: "0.5.20"}
	before := time.Now()
	_ = upd.fail(errors.New("网络抖动"))

	upd.mu.Lock()
	checked, at, msg := upd.checked, upd.checkedAt, upd.errMsg
	upd.mu.Unlock()

	if !checked {
		t.Error("失败后 checked 应为 true（否则会被判定为从未检查而无限重查）")
	}
	if at.Before(before) {
		t.Error("失败后应刷新 checkedAt")
	}
	if msg != "网络抖动" {
		t.Errorf("errMsg=%q, want %q", msg, "网络抖动")
	}
}

// TestUpdateSyncTimeoutReasonable 页面触发的同步检查不能等太久：
// 用户点开弹框应该几秒内有结果，而不是干等十几秒。
func TestUpdateSyncTimeoutReasonable(t *testing.T) {
	if updateSyncTimeout > 10*time.Second {
		t.Fatalf("updateSyncTimeout=%v 过长（用户点开弹框会干等）", updateSyncTimeout)
	}
	if updateSyncTimeout <= 0 {
		t.Fatalf("updateSyncTimeout=%v 非法", updateSyncTimeout)
	}
	if updateStartTimeout < updateSyncTimeout {
		t.Fatalf("启动检查超时(%v)不应短于同步检查超时(%v)", updateStartTimeout, updateSyncTimeout)
	}
}

// TestTimeoutKeepsCachedResult 模拟同步检查超时：应保留上次成功结果，
// 只在响应里标记 timed_out=true，让前端提示"本次超时，展示上次结果"。
func TestTimeoutKeepsCachedResult(t *testing.T) {
	old := upd
	defer func() { upd = old }()

	upd = &updateState{current: "0.5.20"}
	// 上次成功：发现 0.5.21。
	upd.finish(updateResult{latest: "0.5.21", hasUpdate: true, notes: "上次结果",
		assetAPIURL: "https://api.github.com/.../assets/1", assetName: "lanet-0.5.21-windows-amd64.zip"}, "")
	lastSuccess := upd.checkedAt
	time.Sleep(2 * time.Millisecond) // 避开 Windows 时钟粒度，确保时间戳可比较

	// 本次超时：fail 记录错误并刷新时间戳（退避），但不该清掉版本信息。
	upd.fail(context.DeadlineExceeded)

	_, hasUpdate, _, latest, notes, _, _, errMsg := upd.snapshot()
	if !hasUpdate || latest != "0.5.21" {
		t.Errorf("超时后应保留上次结果：hasUpdate=%v latest=%q", hasUpdate, latest)
	}
	if notes != "上次结果" {
		t.Errorf("超时后应保留上次发行说明，got %q", notes)
	}
	if errMsg == "" {
		t.Error("超时后应记录错误信息供页面提示")
	}
	upd.mu.Lock()
	at := upd.checkedAt
	upd.mu.Unlock()
	if !at.After(lastSuccess) {
		t.Errorf("超时（失败）也应刷新 checkedAt 以触发退避：lastSuccess=%v now=%v", lastSuccess, at)
	}
}

// TestTimeoutDoesNotClobberAsset 超时不得让已缓存的下载资产失效——
// 否则用户看到"有新版本"但点更新会报"没有可下载的更新资产"。
func TestTimeoutDoesNotClobberAsset(t *testing.T) {
	old := upd
	defer func() { upd = old }()

	upd = &updateState{current: "0.5.20"}
	upd.finish(updateResult{latest: "0.5.21", hasUpdate: true,
		assetAPIURL: "https://api.github.com/assets/1",
		sumsAPIURL:  "https://api.github.com/assets/2",
		assetName:   "lanet-0.5.21-windows-amd64.zip"}, "")
	upd.fail(context.DeadlineExceeded)

	assetAPI, _, assetName := upd.downloadTargets()
	if assetAPI == "" || assetName == "" {
		t.Errorf("超时后下载目标被清空：assetAPI=%q assetName=%q", assetAPI, assetName)
	}
}

// TestFailKeepsPreviousVersionInfo 失败不应清掉上次成功拿到的版本信息，
// 这样页面在"检查出错"时仍能展示已知最新版。
func TestFailKeepsPreviousVersionInfo(t *testing.T) {
	old := upd
	defer func() { upd = old }()

	upd = &updateState{current: "0.5.20"}
	upd.finish(updateResult{latest: "0.5.21", hasUpdate: true, notes: "说明"}, "")
	_ = upd.fail(errors.New("临时超时"))

	_, hasUpdate, _, latest, notes, _, _, errMsg := upd.snapshot()
	if latest != "0.5.21" || !hasUpdate || notes != "说明" {
		t.Errorf("失败后应保留上次成功结果：latest=%q hasUpdate=%v notes=%q", latest, hasUpdate, notes)
	}
	if errMsg != "临时超时" {
		t.Errorf("errMsg=%q, want %q", errMsg, "临时超时")
	}
}
