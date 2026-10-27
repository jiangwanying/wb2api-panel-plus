// models_auth_test.go GET /v1/models 的鉴权矩阵回归。
//
// 契约：同一个 API Key，无论走 Anthropic 官方 SDK 的 x-api-key，还是 OpenAI 系
// 客户端的 Authorization: Bearer，都必须能列出模型。
//
// 背景（实案）：Hermes 的 Anthropic 适配器启动时会拉 GET /v1/models 做模型发现，
// 走 x-api-key；而该端点此前只挂 withAuth（只认 Bearer），于是每次启动都报
// "could not reach this custom endpoint's model listing"，模型名只能「未经校验
// 接受」。同一把钥匙能开 /v1/messages 的门却开不了 /v1/models 的门，是路由挂载
// 的对称性缺失——withAnthropicAuth 只应用在了两条 messages 路由上。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
)

// modelsAuthHandler 构造只用于鉴权判定的最小 Handler。
//
// 必须给假上游：modelList 在缓存失效时会起后台 goroutine 打上游拉目录，
// 真去连 codebuddy.cn 既慢又 flaky，而上游为 nil 会直接空指针崩掉整个包。
func modelsAuthHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000}]}}`, false
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1"}),
		Upstream: up,
		APIKey:   apiKey,
	})
}

func TestModelsAcceptsAnthropicXAPIKey(t *testing.T) {
	resetDynamicModelsCache()
	h := modelsAuthHandler(t, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key 携带方式应放行，got %d body=%s", rec.Code, rec.Body)
	}
}

func TestModelsAcceptsBearer(t *testing.T) {
	resetDynamicModelsCache()
	h := modelsAuthHandler(t, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer 携带方式应放行，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestModelsRejectsWrongKey 两种携带方式下的错误密钥都必须 401。
func TestModelsRejectsWrongKey(t *testing.T) {
	h := modelsAuthHandler(t, "secret")

	for _, tc := range []struct {
		name   string
		authz  string
		header map[string]string
	}{
		{name: "x-api-key wrong", header: map[string]string{"x-api-key": "nope", "anthropic-version": "2023-06-01"}},
		{name: "bearer wrong", authz: "Bearer nope"},
		{name: "no credentials"},
		{name: "wrong scheme", authz: "Basic secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("应 401，got %d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

// TestModelsErrorShapePerProtocol 401 的错误体形状必须跟请求方协议走：
// Anthropic 侧要 type=error/authentication_error，OpenAI 侧要 error.code=invalid_api_key。
// 形状错了客户端会解析失败，把「密钥不对」误报成「网关坏了」。
func TestModelsErrorShapePerProtocol(t *testing.T) {
	h := modelsAuthHandler(t, "secret")

	t.Run("anthropic shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("x-api-key", "nope")
		req.Header.Set("anthropic-version", "2023-06-01")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应体不是 JSON: %v body=%s", err, rec.Body)
		}
		if body.Type != "error" || body.Error.Type != "authentication_error" {
			t.Fatalf("Anthropic 侧错误形状不对: %s", rec.Body)
		}
	})

	t.Run("openai shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var body struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应体不是 JSON: %v body=%s", err, rec.Body)
		}
		if body.Error.Code != "invalid_api_key" {
			t.Fatalf("OpenAI 侧错误形状不对: %s", rec.Body)
		}
	})
}
