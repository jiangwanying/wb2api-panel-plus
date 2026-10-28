package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/upstream"
)

// embedFake 记录 /v2/embeddings 的落点与请求体，按脚本回状态与原文。
type embedFake struct {
	paths  []string
	bodies []string
	status int
	body   string
}

func (f *embedFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
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

// embedOKBody 上游成功形态：data[] 里**没有** object/index（由网关补齐）。
const embedOKBody = `{"code":0,"msg":"OK","model":"bge-m3","data":[` +
	`{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":6,"total_tokens":6}}`

func embedHandler(t *testing.T, f *embedFake, enabled bool) *Handler {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	return NewHandler(Config{Pool: p, Upstream: f.client(), EmbeddingsEnabled: enabled})
}

func postEmbeddings(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEmbeddingsDisabledByDefault 默认关闭：回 403 embeddings_disabled 且不打上游。
//
// 为什么不是 404：404 会让调用方以为「路径拼错了」，而实际是功能存在、只是没开。
func TestEmbeddingsDisabledByDefault(t *testing.T) {
	f := &embedFake{status: 200, body: embedOKBody}
	h := embedHandler(t, f, false)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":"你好"}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("默认应 403，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("错误体不是 JSON: %v", err)
	}
	if resp.Error.Code != "embeddings_disabled" {
		t.Errorf("code=%q want embeddings_disabled", resp.Error.Code)
	}
	if len(f.paths) != 0 {
		t.Errorf("关闭时不得打上游，实际 %d 次", len(f.paths))
	}
}

// TestEmbeddingsHappyPath 单条字符串输入：上游只认数组 → 包成一条；
// 回程补 object/index（上游不返回），model/usage 原样保留。
func TestEmbeddingsHappyPath(t *testing.T) {
	f := &embedFake{status: 200, body: embedOKBody}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":["你好","世界"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 1 || f.paths[0] != "/v2/embeddings" {
		t.Fatalf("上游路径=%v want [/v2/embeddings]", f.paths)
	}
	var sent struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if sent.Model != "bge-m3" || len(sent.Input) != 2 {
		t.Errorf("上游请求体=%s want model=bge-m3 input 两条", f.bodies[0])
	}

	var got struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	if got.Object != "list" {
		t.Errorf("顶层 object=%q want list", got.Object)
	}
	if got.Model != "bge-m3" || got.Usage.TotalTokens != 6 {
		t.Errorf("model/usage 未原样保留: %+v", got)
	}
	for i, d := range got.Data {
		if d.Object != "embedding" || d.Index != i {
			t.Errorf("data[%d] object=%q index=%d want embedding/%d", i, d.Object, d.Index, i)
		}
	}
}

// TestEmbeddingsModelNotFoundMaps404 上游 11351（HTTP 500）= 模型不存在，
// 必须映射成 OpenAI 的 404，并且在文案里点出已知可用模型——直接回 500 会让调用方去查服务健康。
func TestEmbeddingsModelNotFoundMaps404(t *testing.T) {
	f := &embedFake{status: 500, body: `{"code":11351,"msg":"embedding model [nope] not found"}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"nope","input":"你好"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("11351 应映射 404，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), upstream.EmbeddingModelBGE) {
		t.Errorf("文案应点出可用模型 %s: %s", upstream.EmbeddingModelBGE, rec.Body.String())
	}
	// 模型不存在与账号无关：不得换号重试。
	if len(f.paths) != 1 {
		t.Errorf("模型不存在不该换号重试，实际打了 %d 次", len(f.paths))
	}
}

// TestEmbeddingsBadInput 输入形态非法一律 400，且不打上游。
func TestEmbeddingsBadInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"缺 model", `{"input":"你好"}`},
		{"缺 input", `{"model":"bge-m3"}`},
		{"空串", `{"model":"bge-m3","input":""}`},
		{"空数组", `{"model":"bge-m3","input":[]}`},
		{"数组含非字符串", `{"model":"bge-m3","input":["a",1]}`},
		{"分词数组", `{"model":"bge-m3","input":[[1,2,3]]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &embedFake{status: 200, body: embedOKBody}
			h := embedHandler(t, f, true)
			rec := postEmbeddings(h, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
			}
			if len(f.paths) != 0 {
				t.Errorf("非法请求不得打上游，实际 %d 次", len(f.paths))
			}
		})
	}
}

// TestEmbeddingsCountMismatchIs502 上游返回条数与输入不符：错位配对比报错更难查，
// 一律 502 拒绝（不透传错位向量）。
func TestEmbeddingsCountMismatchIs502(t *testing.T) {
	f := &embedFake{status: 200, body: `{"code":0,"model":"bge-m3","data":[{"embedding":[0.1]}],` +
		`"usage":{"total_tokens":3}}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":["a","b"]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("条数不符应 502，实得 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEmbeddingsRealmPrefixStripped cn: / global: 前缀只用于选号域，出站必须剥掉
// （上游目录里的模型 id 不带域前缀）。
func TestEmbeddingsRealmPrefixStripped(t *testing.T) {
	f := &embedFake{status: 200, body: `{"code":0,"model":"bge-m3","data":[{"embedding":[0.1]}],` +
		`"usage":{"total_tokens":3}}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"cn:bge-m3","input":"你好"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if sent.Model != "bge-m3" {
		t.Errorf("出站模型名=%q want bge-m3（前缀必须剥掉）", sent.Model)
	}
}
