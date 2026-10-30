package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 未知 restart policy 时必须拒绝，而不是返回成功后退出造成停服。
func TestContainerRestartRejectedWithoutStoppingNode(t *testing.T) {
	stubContainer(t, true)
	w := httptest.NewRecorder()
	updateRoutes(func() {})["POST /api/restart"](w, httptest.NewRequest(http.MethodPost, "/api/restart", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// 即使内部误调用，也只能安全返回，不能 release 锁、派生进程或退出。
	restartSelf(0)
	time.Sleep(50 * time.Millisecond)
}
