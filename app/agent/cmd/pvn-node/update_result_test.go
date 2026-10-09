package main

import "testing"

func TestUpdateResultErrorIsPreserved(t *testing.T) {
	for _, reason := range []string{"最新版未提供本机平台发行包", "最新版缺少 sha256sums.txt"} {
		u := &updateState{current: "0.5.71"}
		u.finish(updateResult{latest: "0.5.87", errMsg: reason}, "")
		_, has, _, latest, _, _, _, got := u.snapshot()
		if has || latest != "0.5.87" || got != reason {
			t.Fatalf("lost failed update result: has=%v latest=%q error=%q", has, latest, got)
		}
	}
	u := &updateState{}
	u.finish(updateResult{errMsg: "result error"}, "explicit error")
	_, _, _, _, _, _, _, got := u.snapshot()
	if got != "explicit error" {
		t.Fatalf("explicit error should take precedence: %q", got)
	}
}
