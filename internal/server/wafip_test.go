package server

import (
	"testing"
	"time"
)

// TestWafIPGateStateModel WAF IP 级拦截状态机的四条口径（注释宣称「var 仅供
// 测试注入短窗」，此前一直没有这个测试）：
//  1. 单号反复 403 永不触发（账号级偶发归软冷却管）；
//  2. 窗内两个不同号接连命中 → 激活；
//  3. 激活期内新命中不续期，窗口到期自然解除；
//  4. 解除后需全新命中重新判定（旧账已清）。
func TestWafIPGateStateModel(t *testing.T) {
	old := wafIPWindow
	wafIPWindow = 60 * time.Millisecond
	defer func() { wafIPWindow = old }()

	g := &wafIPGate{}
	for i := 0; i < 5; i++ {
		if g.noteWaf("u1") {
			t.Fatal("单号反复 403 永不应触发 IP 级拦截")
		}
	}
	if g.active() {
		t.Fatal("单号命中不应激活")
	}
	// 窗内第二个不同号 → 激活。
	if g.noteWaf("u1") {
		// u1 已在窗内，本次仍只有 1 个不同号
	}
	if !g.noteWaf("u2") {
		t.Fatal("窗内第二个不同号命中应激活")
	}
	if !g.active() {
		t.Fatal("激活期内 active() 应为 true")
	}
	// 激活期内新命中：仍返回 true（fail-fast 语义）。
	if !g.noteWaf("u3") {
		t.Fatal("激活期内新命中应继续 fail-fast")
	}
	// 窗口到期自然解除。
	time.Sleep(80 * time.Millisecond)
	if g.active() {
		t.Fatal("窗口到期应自然解除")
	}
	// 解除后旧账已清：单号命中不激活，需再集满两个不同号。
	if g.noteWaf("u4") {
		t.Fatal("解除后单号命中不应立即激活（旧账应已清）")
	}
	if !g.noteWaf("u5") {
		t.Fatal("解除后重新集满两个不同号应再次激活")
	}
}
