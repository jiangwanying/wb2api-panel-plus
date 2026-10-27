package server

// issue #44 观测面：声明了 tools 的回合要把「声明数 / 实际调用数」写进归档事件。
// 发生率只能靠这两个字段算出来——日志里那一行 INFO 翻不出来，也不该靠人肉统计。
// 反例（没声明 tools 的纯聊天）不得被计入，否则分母是错的、比率没有意义。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/reqlog"
)

// sseToolCall 上游正常返回一次工具调用（delta.tool_calls + finish_reason=tool_calls）。
const sseToolCall = `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestRequestMetricsToolsSurface(t *testing.T) {
	cases := []struct {
		name                     string
		reqBody                  string
		upstream                 func(string) (int, string, bool)
		wantDeclared, wantCalled int
	}{
		{
			name:    "声明了 tools 但零工具调用收尾",
			reqBody: `{"model":"glm-5.2","stream":true,"messages":[],` + declaredToolsBody + `}`,
			upstream: func(string) (int, string, bool) {
				return 200, sseOK, true
			},
			wantDeclared: 1, wantCalled: 0,
		},
		{
			name:    "声明了 tools 且确实调用了",
			reqBody: `{"model":"glm-5.2","stream":true,"messages":[],` + declaredToolsBody + `}`,
			upstream: func(string) (int, string, bool) {
				return 200, sseToolCall, true
			},
			wantDeclared: 1, wantCalled: 1,
		},
		{
			// 非流式走 upstream.Aggregate 组包，tool_calls 是 []map[string]any 而不是 []any：
			// 只看流式用例会让这条路径的计数永远是 0（线上冒烟抓到过一次）。
			name:    "非流式：声明了 tools 且确实调用了",
			reqBody: `{"model":"glm-5.2","stream":false,"messages":[],` + declaredToolsBody + `}`,
			upstream: func(string) (int, string, bool) {
				return 200, sseToolCall, true
			},
			wantDeclared: 1, wantCalled: 1,
		},
		{
			name:    "没声明 tools 的纯聊天不进统计",
			reqBody: `{"model":"glm-5.2","stream":true,"messages":[]}`,
			upstream: func(string) (int, string, bool) {
				return 200, sseOK, true
			},
			wantDeclared: 0, wantCalled: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reqLog := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   newFakeUpstream(t, c.upstream),
				RequestLog: reqLog,
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.reqBody)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			s := reqLog.Snapshot()
			if len(s.Recent) != 1 {
				t.Fatalf("recent = %+v", s.Recent)
			}
			e := s.Recent[0]
			if e.ToolsDeclared != c.wantDeclared || e.ToolCalls != c.wantCalled {
				t.Fatalf("tools_declared=%d tool_calls=%d, want %d/%d",
					e.ToolsDeclared, e.ToolCalls, c.wantDeclared, c.wantCalled)
			}
		})
	}
}
