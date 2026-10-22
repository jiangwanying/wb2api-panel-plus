package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchFake 记录检索落点/体，并按脚本回状态与原文。
type searchFake struct {
	paths  []string
	hosts  []string
	bodies []string
	status int
	body   string
}

func (f *searchFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.hosts = append(f.hosts, r.URL.Host)
			f.bodies = append(f.bodies, string(raw))
			return &http.Response{
				StatusCode: f.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(f.body)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

const searchOKBody = `{"query":"玉林 电梯 维保","type":"text2text","provider":"0",` +
	`"results":[{"title":"新规","url":"https://x.example/1","snippet":"..."}],` +
	`"total_results":5,"response_time_ms":631}`

// TestSearchEndpoint POST /v1/search：原文透传、缺 query 400、请求级 400 不轮转。
func TestSearchEndpoint(t *testing.T) {
	f := &searchFake{status: 200, body: searchOKBody}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(body)))
		return rec
	}

	// ① 成功：上游原文一字不动，落点在 CN 域
	rec := post(`{"query":"玉林 电梯 维保","max_results":5}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != searchOKBody {
		t.Fatalf("原文透传不符: %s", rec.Body.String())
	}
	if f.paths[0] != "/agenttool/v1/search" || f.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s", f.hosts[0], f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["type"] != "text2text" || sent["max_results"] != float64(5) {
		t.Fatalf("请求体归一不符: %v", sent)
	}

	// ② 缺 query：400 且不打上游（少一次白烧）
	before := len(f.paths)
	if rec := post(`{"query":"  "}`); rec.Code != 400 {
		t.Fatalf("空 query 应 400，得到 %d", rec.Code)
	}
	if len(f.paths) != before {
		t.Fatal("空 query 不应发出上游请求")
	}

	// ③ 请求级 400（15003）：原文透传、只打 1 次上游（不轮转烧第二个号）
	f2 := &searchFake{status: 400, body: `{"code":15003,"msg":"unsupported search provider: 0"}`}
	h2 := NewHandler(Config{Pool: p, Upstream: f2.client()})
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "15003") {
		t.Fatalf("请求级 400 应原文透传，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(f2.paths) != 1 {
		t.Fatalf("请求级错误应 fail-fast 不轮转，实际打了 %d 次", len(f2.paths))
	}

	// ④ 无可用账号：503 no_account（不是含糊的 502）
	empty := NewHandler(Config{Pool: testPoolWith(), Upstream: f.client()})
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no_account") {
		t.Fatalf("空池应 503 no_account，得到 %d %s", rec.Code, rec.Body.String())
	}
}

// TestSearchDeepSwitch deep:true 走 agentic_search（综述原文），缺省仍走 snippet 快路径。
func TestSearchDeepSwitch(t *testing.T) {
	f := &searchFake{status: 200, body: `{"query":"x","results":[{"title":"t","url":"https://x/1","snippet":"s"}]}`}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 200 || f.paths[0] != "/agenttool/v1/search" {
		t.Fatalf("缺省应走 snippet 快路径: code=%d path=%v", rec.Code, f.paths)
	}

	// deep:true → agentic_search，且 done 帧原文透出（synthesis/content 都在）
	agentic := `event: done` + "\n" + `data: {"partial":true,"partial_reason":"timeout",` +
		`"synthesis":{"content":"综述正文","inline_content":"综述"},"results":[{"id":"r1","content":"正文"}]}` + "\n\n"
	f2 := &searchFake{status: 200, body: agentic}
	h2 := NewHandler(Config{Pool: p, Upstream: f2.client()})
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x","deep":true}`)))
	if rec.Code != 200 {
		t.Fatalf("deep code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f2.paths[0] != "/agenttool/v1/agentic_search" {
		t.Fatalf("deep 应落 agentic_search: %v", f2.paths)
	}
	var done map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("deep 响应不是 done 原文: %v (%s)", err, rec.Body.String())
	}
	if syn, _ := done["synthesis"].(map[string]any); syn["content"] != "综述正文" {
		t.Fatalf("综述未透传: %v", done)
	}
	// partial 是上游自己的降级标记，必须让调用方看见（别把降级当完整）
	if done["partial"] != true || done["partial_reason"] != "timeout" {
		t.Fatalf("partial 未透传: %v", done)
	}
}
