package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/pool"
	"github.com/jiangwanying/wb2api-panel-plus/internal/upstream"
)

// TestBatchEndpointsRejectConcurrentRuns 五个手动全量运维接口共用防重入闸门：
// 一轮在跑时再次触发任何全量接口都返回 409，而不是并发再跑一轮逐号打上游。
func TestBatchEndpointsRejectConcurrentRuns(t *testing.T) {
	p := newTestPanel()
	p.batchRunning.Store(true) // 模拟一轮全量任务执行中
	for _, path := range []string{
		"/panel/api/checkin_all",
		"/panel/api/travel_all",
		"/panel/api/activity_all",
		"/panel/api/keepalive_all",
		"/panel/api/balance_all",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: got %d want 409 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
	// 闸门释放后恢复 501（无 scheduler 的既有语义），不再 409。
	p.batchRunning.Store(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/checkin_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("闸门释放后应回到 501 语义, got %d", rec.Code)
	}
}

// fakeModelsUpstream 计数版假上游：目录探测回一份最小模型清单，繁忙度端点
// （POST /v2/chat/busy）回合法空数组——不特判会让 ModelBusy 解析失败、结果带
// fetchErr 而进不了缓存。
func fakeModelsUpstream(calls *atomic.Int32) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000}],` +
				`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/chat/busy") {
				body = `{"code":0,"data":[]}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

func fakeModelsPool() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)
	return p
}

// TestModelsCacheShortTTL /panel/api/models 的服务端短缓存：TTL 内重复请求不再
// 打上游（cached=true），refresh=1 绕过缓存并刷新它，TTL 过期后重新聚合。
func TestModelsCacheShortTTL(t *testing.T) {
	var calls atomic.Int32
	pn := New(Config{Version: "test", APIKey: "k", Pool: fakeModelsPool(), Upstream: fakeModelsUpstream(&calls)})

	get := func(query string) (int, bool) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/panel/api/models"+query, nil)
		req.Header.Set("Authorization", "Bearer k")
		pn.ServeHTTP(rec, req)
		var resp struct {
			Cached bool             `json:"cached"`
			Models []map[string]any `json:"models"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp.Cached
	}

	if code, cached := get(""); code != 200 || cached {
		t.Fatalf("首次请求: code=%d cached=%v", code, cached)
	}
	first := calls.Load()
	if calls.Load() == 0 {
		t.Fatal("前置失败：首次聚合未打上游")
	}
	if code, cached := get(""); code != 200 || !cached {
		t.Fatalf("TTL 内二次请求应命中缓存: code=%d cached=%v", code, cached)
	}
	if calls.Load() != first {
		t.Fatalf("缓存命中不应再打上游: calls=%d want %d", calls.Load(), first)
	}
	if code, cached := get("?refresh=1"); code != 200 || cached {
		t.Fatalf("refresh=1 应绕过缓存: code=%d cached=%v", code, cached)
	}
	if calls.Load() == first {
		t.Fatal("refresh=1 应重新探上游")
	}
	afterRefresh := calls.Load()
	// refresh 聚合出的干净结果刷新缓存：再查一次应命中且不再打上游。
	if code, cached := get(""); code != 200 || !cached {
		t.Fatalf("refresh 后缓存应更新: code=%d cached=%v", code, cached)
	}
	if calls.Load() != afterRefresh {
		t.Fatalf("refresh 后的缓存命中不应打上游: calls=%d want %d", calls.Load(), afterRefresh)
	}
	// TTL 过期后重新聚合。
	pn.modelsMu.Lock()
	pn.modelsCacheAt = time.Now().Add(-2 * modelsCacheTTL)
	pn.modelsMu.Unlock()
	if code, cached := get(""); code != 200 || cached {
		t.Fatalf("TTL 过期应重新聚合: code=%d cached=%v", code, cached)
	}
	if calls.Load() == afterRefresh {
		t.Fatal("TTL 过期应重新打上游")
	}
}
