package main

import "testing"

// TestIsAlreadyNarrowMatching 已签到判定必须窄匹配：上游业务错误统一是
// HTTP 400 + code=%d，宽匹配 code=400 / 裸 "checkin" 会把参数缺失、风控、
// 「活动未开启」等真实失败全部误判成 ALREADY，静默掩盖失败。
func TestIsAlreadyNarrowMatching(t *testing.T) {
	already := []string{
		"upstream error: code=10001 今天已签到",
		"code=10001 msg=already checked in today",
		"今日已签到，明天再来吧",
		"already claimed",
	}
	for _, m := range already {
		if !isAlready(m) {
			t.Errorf("isAlready(%q) = false, want true", m)
		}
	}
	failures := []string{
		"upstream error: code=400 msg=活动未开启",           // 400 但非幂等码
		"upstream error: code=400 msg=request illegal", // 风控/参数错误
		"checkin activity not started",                 // 裸 checkin 字样不再误判
		"http 500",
	}
	for _, m := range failures {
		if isAlready(m) {
			t.Errorf("isAlready(%q) = true, want false（真实失败被误判成已签到）", m)
		}
	}
}
