package session

import (
	"testing"
	"time"
)

// TestResolveForModelReassignsWhenModelLimited 模型维度粘性的核心语义（生产已
// 接线、此前零测试）：绑定号被该模型限流（6004）但对其他模型仍可用时，
// ResolveForModel 必须走慢路径重分配，而不是把会话钉在失败号上（issue #31/#35）。
func TestResolveForModelReassignsWhenModelLimited(t *testing.T) {
	r := New(Config{
		TTL:       time.Minute,
		Store:     newCountingStore(),
		Available: func() []string { return []string{"a1", "a2"} },
		AvailableForModel: func(model string) []string {
			if model == "m-limited" {
				return []string{"a2"} // a1 在该模型上被限
			}
			return []string{"a1", "a2"}
		},
	})
	r.Bind("k", "a1")

	if uid, ok := r.ResolveForModel("k", "m-ok"); !ok || uid != "a1" {
		t.Fatalf("模型可用时应命中绑定号, got %q ok=%v", uid, ok)
	}
	if uid, ok := r.ResolveForModel("k", "m-limited"); !ok || uid != "a2" {
		t.Fatalf("绑定号在该模型不可用时应重分配到 a2, got %q ok=%v", uid, ok)
	}
}

// TestResolveForModelFallsBackToAvailable 未注入 AvailableForModel 时回落
// Available（与引入该字段之前的行为逐字一致，裸用/测试路径不受影响）。
func TestResolveForModelFallsBackToAvailable(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("k", "a1")
	uid, ok := r.ResolveForModel("k", "any-model")
	if !ok || uid != "a1" {
		t.Fatalf("回落路径应与账号级粘性一致, got %q ok=%v", uid, ok)
	}
}
