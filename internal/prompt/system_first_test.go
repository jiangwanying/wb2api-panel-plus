// system_first_test.go 上游渠道校验「首条必须是 system」的回归。
//
// 2026-10-20 真机实测（国际版 5.8.4，同一 token / 同一 body，只改首条角色）：
//
//	首条 user      → 400 code=11-128 "first message is not system prompt"
//	首条 developer → 400 code=11-128 "Illegal API invocation from an unapproved channel"
//	首条 system    → 200，模型正常回包
//
// 这不是内容策略也不是账号问题：**首条角色本身**属于渠道校验。此前 passthrough 模式
// 原样透传，客户端不带 system 时每次请求都必然 11-128，账号被反复记成「渠道校验
// 拦截」而全池冷却——表象即 issue #231「国际版账号全被禁用但网页还能聊」。
package prompt

import (
	"encoding/json"
	"testing"
)

func firstRole(t *testing.T, body []byte) (string, []map[string]any) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, body)
	}
	raw, _ := obj["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		out = append(out, mm)
	}
	if len(out) == 0 {
		return "", nil
	}
	r, _ := out[0]["role"].(string)
	return r, out
}

// 首条 user（最常见的手搓/SDK 形态）→ 补一条 system 在最前。
func TestEnsureSystemFirstPrepends(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := EnsureSystemFirst(in, "GW")
	role, msgs := firstRole(t, out)
	if role != "system" {
		t.Fatalf("首条 role=%q want system", role)
	}
	if msgs[0]["content"] != "GW" {
		t.Errorf("补的 system content=%v want GW", msgs[0]["content"])
	}
	if len(msgs) != 2 || msgs[1]["role"] != "user" || msgs[1]["content"] != "hi" {
		t.Errorf("原有消息必须逐字不动：%v", msgs)
	}
}

// 首条已是 system → 一个字节都不改（幂等，且不重复插入网关提示词）。
func TestEnsureSystemFirstKeepsSystem(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"system","content":"client"},{"role":"user","content":"hi"}]}`)
	out := EnsureSystemFirst(in, "GW")
	role, msgs := firstRole(t, out)
	if role != "system" || len(msgs) != 2 {
		t.Fatalf("不该改动：role=%q len=%d", role, len(msgs))
	}
	if msgs[0]["content"] != "client" {
		t.Errorf("客户端 system 应原样保留，got %v", msgs[0]["content"])
	}
}

// 首条 developer：上游同样拒绝（另一句 unapproved channel）→ 就地归一成 system。
func TestEnsureSystemFirstConvertsDeveloper(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"developer","content":"dev-msg"},{"role":"user","content":"hi"}]}`)
	out := EnsureSystemFirst(in, "GW")
	role, msgs := firstRole(t, out)
	if role != "system" {
		t.Fatalf("developer 应归一成 system，got %q", role)
	}
	if msgs[0]["content"] != "dev-msg" {
		t.Errorf("内容必须逐字保留，got %v", msgs[0]["content"])
	}
	if len(msgs) != 2 {
		t.Errorf("不该新增消息，len=%d", len(msgs))
	}
}

// 未配提示词时用 Degraded 兜底，而不是「因为没有提示词就不补」。
func TestEnsureSystemFirstFallsBackWhenEmptyPrompt(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := EnsureSystemFirst(in, "")
	role, msgs := firstRole(t, out)
	if role != "system" {
		t.Fatalf("空 fallback 也应补 system，got %q", role)
	}
	if msgs[0]["content"] != Degraded {
		t.Errorf("content=%v want Degraded", msgs[0]["content"])
	}
}

// 坏 JSON / 无 messages 原样返回（关键路径绝不失败、绝不凭空造对话）。
func TestEnsureSystemFirstPassthroughOnBadInput(t *testing.T) {
	for _, in := range []string{
		`not json`,
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`{"model":"m","messages":"nope"}`,
	} {
		out := EnsureSystemFirst([]byte(in), "GW")
		if string(out) != in {
			t.Errorf("输入 %q 应原样返回，got %q", in, out)
		}
	}
}

// 其他字段（tools / temperature / max_tokens…）不得因为补 system 而丢失。
func TestEnsureSystemFirstKeepsOtherFields(t *testing.T) {
	in := []byte(`{"model":"m","temperature":0.3,"max_tokens":99,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`)
	out := EnsureSystemFirst(in, "GW")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "m" || obj["temperature"] != 0.3 || obj["max_tokens"] != float64(99) {
		t.Errorf("基础字段丢了：%v", obj)
	}
	if _, ok := obj["tools"].([]any); !ok {
		t.Errorf("tools 丢了：%v", obj["tools"])
	}
}
