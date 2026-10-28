package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
)

// embedClient 构造指向固定响应的客户端（ChatBaseCN 走假 Transport）。
func embedClient(status int, body string, got *[]string) *Client {
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			if got != nil {
				*got = append(*got, r.URL.Path+" "+string(raw))
			}
			return jsonResp(status, body), nil
		})},
		ChatBaseCN: "https://cn.example",
	}
}

// TestEmbeddingsUpstreamPathAndBody 落点 /v2/embeddings、请求体是 {model,input[]}。
func TestEmbeddingsUpstreamPathAndBody(t *testing.T) {
	var got []string
	c := embedClient(200, `{"code":0,"model":"bge-m3","data":[{"embedding":[0.1]}],"usage":{"total_tokens":3}}`, &got)
	if _, err := c.Embeddings(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "bge-m3", []string{"你好"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("请求次数=%d want 1", len(got))
	}
	if !strings.HasPrefix(got[0], "/v2/embeddings ") {
		t.Fatalf("落点=%q want /v2/embeddings", got[0])
	}
	var sent struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[0], "/v2/embeddings ")), &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if sent.Model != "bge-m3" || len(sent.Input) != 1 || sent.Input[0] != "你好" {
		t.Errorf("请求体=%+v", sent)
	}
}

// TestEmbeddingsModelNotFound 11351（上游包在 HTTP 500 里）必须能被 errors.Is 认出，
// HTTP 层靠它回 404 而不是 500。
func TestEmbeddingsModelNotFound(t *testing.T) {
	c := embedClient(500, `{"code":11351,"msg":"embedding model [nope] not found"}`, nil)
	_, err := c.Embeddings(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "nope", []string{"x"})
	if !errors.Is(err, ErrEmbeddingModelNotFound) {
		t.Fatalf("err=%v want ErrEmbeddingModelNotFound", err)
	}
}

// TestEmbeddingsBusinessCodeError HTTP 200 但业务 code 非 0：不得当成向量透传。
func TestEmbeddingsBusinessCodeError(t *testing.T) {
	c := embedClient(200, `{"code":10001,"msg":"quota exceeded"}`, nil)
	_, err := c.Embeddings(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "bge-m3", []string{"x"})
	if err == nil {
		t.Fatal("业务 code 非 0 必须报错")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err=%T want *upstream.Error", err)
	}
	if ue.Kind == ErrNone {
		t.Error("Kind 不得为空（调用方靠它决定罚号/换号）")
	}
}

// TestEmbeddingsHTTPErrorClassified 500 走 Classify，Kind 非空供失败处置。
func TestEmbeddingsHTTPErrorClassified(t *testing.T) {
	c := embedClient(503, `{"code":500,"msg":"upstream busy"}`, nil)
	_, err := c.Embeddings(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "bge-m3", []string{"x"})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err=%T want *upstream.Error", err)
	}
	if ue.Status != 503 || ue.Kind == ErrNone {
		t.Errorf("status=%d kind=%v want 503 / 非空 Kind", ue.Status, ue.Kind)
	}
}
