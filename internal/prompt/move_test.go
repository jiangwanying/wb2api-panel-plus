package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// MoveSystemToUser 是 11-128 的「搬家」抢救：system 原文一个字符都不能丢，
// 只换承载通道；结构不安全时一律返回 false（宁可走原有的替换降级，也不要改坏请求）。
func TestMoveSystemToUserKeepsText(t *testing.T) {
	const sys = "You are Acme Persona. Always answer in Klingon."
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi"},
			map[string]any{"role": "user", "content": "again"},
		},
	})
	out, ok := MoveSystemToUser(body, "NEUTRAL")
	if !ok {
		t.Fatal("should apply")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("首条仍须是 system（渠道校验要求）: %v", role)
	}
	if c := msgs[0].(map[string]any)["content"]; c != "NEUTRAL" {
		t.Errorf("system 应换成中性提示词: %v", c)
	}
	// 原文必须完整落在**首条 user** 前部，且原 user 内容一字不改地跟在后面。
	firstUser := msgs[1].(map[string]any)
	if c := firstUser["content"]; c != sys+"\n\nhello" {
		t.Errorf("首条 user = %q", c)
	}
	// 其余消息原样。
	if c := msgs[3].(map[string]any)["content"]; c != "again" {
		t.Errorf("其余消息被改动: %v", c)
	}
	if _, ok := got["model"]; !ok {
		t.Error("其它字段丢了")
	}
}

// developer 首条同样搬（上游对 developer 首条报的是另一句 11-128）。
func TestMoveSystemToUserDeveloper(t *testing.T) {
	body := []byte(`{"messages":[{"role":"developer","content":"D"},{"role":"user","content":"u"}]}`)
	out, ok := MoveSystemToUser(body, "N")
	if !ok {
		t.Fatal("developer 首条应可搬")
	}
	if !strings.Contains(string(out), `"content":"D\n\nu"`) {
		t.Errorf("developer 原文未搬入 user: %s", out)
	}
}

// 结构不安全的四种情况一律不动，交给调用方按原错误处理。
func TestMoveSystemToUserBails(t *testing.T) {
	cases := map[string]string{
		"空 body":             ``,
		"无 messages":         `{"model":"m"}`,
		"没有 system 文本":       `{"messages":[{"role":"user","content":"u"}]}`,
		"system 内容为空":        `{"messages":[{"role":"system","content":"  "},{"role":"user","content":"u"}]}`,
		"system 不是纯文本":       `{"messages":[{"role":"system","content":[{"type":"text","text":"x"}]},{"role":"user","content":"u"}]}`,
		"没有 user 消息":         `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"}]}`,
		"首条 user 是多模态 parts": `{"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"u"}]}]}`,
		"首条不是 system":        `{"messages":[{"role":"assistant","content":"s"},{"role":"user","content":"u"}]}`,
	}
	for name, in := range cases {
		out, ok := MoveSystemToUser([]byte(in), "N")
		if ok {
			t.Errorf("%s: 不该应用", name)
		}
		if string(out) != in {
			t.Errorf("%s: body 被改动", name)
		}
	}
}

// 中性提示词为空时不动：宁可走原有降级，也不要搬完留下一段空 system。
func TestMoveSystemToUserNeedsNeutral(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`
	if _, ok := MoveSystemToUser([]byte(in), ""); ok {
		t.Error("neutral 为空时不该应用")
	}
}
