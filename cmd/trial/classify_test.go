package main

import (
	"errors"
	"testing"
)

// TestClassifyTrial 领取结果三态分类（纯函数，注释一直声称「供测试直接断言」，
// 此前没有这个测试）。
func TestClassifyTrial(t *testing.T) {
	if st, msg := classifyTrial(true, nil); st != trialOK || msg != "trial granted" {
		t.Errorf("claimed: %q %q", st, msg)
	}
	if st, _ := classifyTrial(false, nil); st != trialAlready {
		t.Errorf("idempotent: %q", st)
	}
	wrapped := errors.New("code=14051 already claimed")
	if st, msg := classifyTrial(false, wrapped); st != trialFailed || msg != wrapped.Error() {
		t.Errorf("error: %q %q", st, msg)
	}
}
