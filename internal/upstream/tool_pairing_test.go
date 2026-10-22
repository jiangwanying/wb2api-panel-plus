package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCleanupOrphanToolCallsNamelessCalls 钉桩「无可用功能名的 tool_call 一律剔除」
// （线上 11133 model_param_invalid，见 tool_pairing.go 的 keepCalls 否决条件）。
//
// 触发链：上游流在 tool_call 的 name 分片到达前异常结束（200 空流也算）→ 聚合器/流回程
// 落下无名条目 → 客户端写进会话历史 → 下一轮原样重发 → 带结果的脏条目命中旧的 keepCalls
// 原样出站 → 上游 400，整条会话每轮报废。
func TestCleanupOrphanToolCallsNamelessCalls(t *testing.T) {
	assistant := func(calls ...any) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": calls}
	}
	call := func(id, name string) map[string]any {
		return map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": "{}"}}
	}
	// callNoNameKey 有 function 但无 name 键；callNoFunc 连 function 键都没有——
	// 两种形状都是本网关聚合器在 name 分片缺失时真实产出的占位条目。
	callNoNameKey := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "function", "function": map[string]any{}}
	}
	callNoFunc := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "function"}
	}
	tool := func(id, out string) map[string]any {
		return map[string]any{"role": "tool", "tool_call_id": id, "content": out}
	}

	cases := []struct {
		name   string
		in     []any
		want   []any // 期望出站 messages
		change bool
	}{
		{
			name:   "空 name + 配对结果：调用与结果成对剔除，assistant 消息保留",
			in:     []any{assistant(call("c1", "")), tool("c1", "ok")},
			want:   []any{map[string]any{"role": "assistant", "content": ""}},
			change: true,
		},
		{
			name:   "纯空白 name 同样判脏",
			in:     []any{assistant(call("c1", "  ")), tool("c1", "ok")},
			want:   []any{map[string]any{"role": "assistant", "content": ""}},
			change: true,
		},
		{
			name:   "有 function 无 name 键 + 配对结果：剔除",
			in:     []any{assistant(callNoNameKey("c1")), tool("c1", "ok")},
			want:   []any{map[string]any{"role": "assistant", "content": ""}},
			change: true,
		},
		{
			name:   "无 function 键 + 配对结果：剔除",
			in:     []any{assistant(callNoFunc("c1")), tool("c1", "ok")},
			want:   []any{map[string]any{"role": "assistant", "content": ""}},
			change: true,
		},
		{
			name:   "空 name 且无结果：仍走既有孤儿路径（回归位，不得早退漏出站）",
			in:     []any{assistant(call("c1", ""))},
			want:   []any{map[string]any{"role": "assistant", "content": ""}},
			change: true,
		},
		{
			name: "批内混合：只剔脏项，干净调用与其结果原样保留",
			in: []any{
				assistant(call("c1", ""), call("c2", "Bash")),
				tool("c1", "ok1"), tool("c2", "ok2"),
			},
			want: []any{
				assistant(call("c2", "Bash")),
				tool("c2", "ok2"),
			},
			change: true,
		},
		{
			name:   "健康流量（有 name + 配对）：零改动",
			in:     []any{assistant(call("c1", "Bash")), tool("c1", "ok")},
			want:   []any{assistant(call("c1", "Bash")), tool("c1", "ok")},
			change: false,
		},
	}

	for _, tc := range cases {
		got, changed := cleanupOrphanToolCalls(tc.in)
		if changed != tc.change {
			t.Errorf("%s: changed=%v, want %v", tc.name, changed, tc.change)
		}
		if !reflect.DeepEqual(normalize(got), normalize(tc.want)) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tc.want)
			t.Errorf("%s:\n got %s\nwant %s", tc.name, gb, wb)
		}
	}
}

// normalize 把 messages 转成 map 便于 DeepEqual（入参可能是 []map[string]any 或已改写的
// []any，比较只关心内容）。
func normalize(m []any) []map[string]any {
	b, _ := json.Marshal(m)
	var out []map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
