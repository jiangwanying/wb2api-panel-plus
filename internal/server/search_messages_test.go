package server

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 工具声明判定：type 前缀与 name 两种形态都要认，无用声明不能放行。
func TestDeclaresWebSearch(t *testing.T) {
	cases := []struct {
		name  string
		tools []messagesTool
		want  bool
	}{
		{"type 前缀", []messagesTool{{Type: "web_search_20250305"}}, true},
		{"name 形态", []messagesTool{{Name: "web_search"}}, true},
		{"无工具", nil, false},
		{"其他工具", []messagesTool{{Type: "computer_20250124"}}, false},
	}
	for _, c := range cases {
		if got := declaresWebSearch(c.tools); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// 搜索词提取：纯字符串 / block 数组 / 前缀剥离 / 多轮只取最后一条。
func TestExtractMessagesQuery(t *testing.T) {
	mk := func(content string) messagesTurnItem {
		return messagesTurnItem{Role: "user", Content: json.RawMessage(content)}
	}
	cases := []struct {
		name  string
		items []messagesTurnItem
		want  string
	}{
		{"纯字符串", []messagesTurnItem{mk(`"OpenAI 最新模型"`)}, "OpenAI 最新模型"},
		{"带前缀", []messagesTurnItem{mk(`"Perform a web search for the query: 玉林 电梯"`)}, "玉林 电梯"},
		{"前缀大小写", []messagesTurnItem{mk(`"perform a WEB SEARCH FOR THE QUERY: qwen3"`)}, "qwen3"},
		{"block 数组", []messagesTurnItem{mk(`[{"type":"text","text":"Perform a web search for the query: golang"}]`)}, "golang"},
		{"多块拼接", []messagesTurnItem{mk(`[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]`)}, "第一段\n第二段"},
		{"多轮取最后", []messagesTurnItem{
			mk(`"Perform a web search for the query: 旧问题"`),
			mk(`"Perform a web search for the query: 新问题"`),
		}, "新问题"},
		{"空内容", []messagesTurnItem{mk(`""`)}, ""},
	}
	for _, c := range cases {
		if got := extractMessagesQuery(c.items); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// 响应结构契约：tool_result block 必须存在、url 非空、snippet 走 citations、
// 每次调用的 id 必须不同。
func TestBuildMessagesSearchResponse(t *testing.T) {
	sr := upstream.SearchResponse{Results: []upstream.SearchResult{
		{Title: "标题 A", URL: "https://a.example/1", Snippet: "摘要 A"},
		{Title: "", URL: "https://b.example/2"},
	}}
	resp := buildMessagesSearchResponse("q", "", sr)
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Fatalf("响应外壳不符: %+v", resp)
	}
	if resp.Model != "web-search" {
		t.Fatalf("空 model 应回填 web-search，得到 %q", resp.Model)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("应有 tool_result + text 两块，得到 %d", len(resp.Content))
	}
	blk, ok := resp.Content[0].(messagesToolResultBlock)
	if !ok || blk.Type != "web_search_tool_result" {
		t.Fatalf("第一块应为 web_search_tool_result: %#v", resp.Content[0])
	}
	if len(blk.Content) != 2 || blk.Content[0].URL != "https://a.example/1" {
		t.Fatalf("结果条目不符: %+v", blk.Content)
	}
	if blk.Content[1].Title != "https://b.example/2" {
		t.Fatalf("无标题应以 URL 兜底，得到 %q", blk.Content[1].Title)
	}
	txt, ok := resp.Content[1].(messagesTextBlock)
	if !ok || len(txt.Citations) != 1 || txt.Citations[0].CitedText != "摘要 A" {
		t.Fatalf("citations 不符: %#v", resp.Content[1])
	}
	if resp2 := buildMessagesSearchResponse("q", "", sr); resp2.ID == resp.ID {
		t.Fatal("两次调用的 msg id 不应相同")
	}
}
