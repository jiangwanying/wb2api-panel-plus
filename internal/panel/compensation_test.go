package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compPanel 起一个只服务补偿两端点的桩，并装配 Panel（含 Scheduler）。
// failClaimFor 非空时，只有 Authorization 含该子串的账号领取失败（按号区分成败）。
type compPanel struct {
	panel        *Panel
	mu           sync.Mutex
	statusBody   string
	claimBody    string
	failClaimFor string
	claimCalls   int
}

func newCompPanel(t *testing.T, uid ...string) *compPanel {
	t.Helper()
	c := &compPanel{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":500}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/compensation-status"):
			c.mu.Lock()
			body := c.statusBody
			c.mu.Unlock()
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/claim-compensation"):
			c.mu.Lock()
			c.claimCalls++
			body, failFor := c.claimBody, c.failClaimFor
			c.mu.Unlock()
			if failFor != "" && strings.Contains(r.Header.Get("Authorization"), failFor) {
				body = `{"code":500,"msg":"boom"}`
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, u := range uid {
		p.Add(&auth.Auth{UID: u, AccessToken: "at-" + u, ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	c.panel = New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
	return c
}

func (c *compPanel) do(t *testing.T, method, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	c.panel.ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// TestPanelCompensationListIsReadOnly 只读列表：查到待领但不领取（取数与领取分离）。
func TestPanelCompensationListIsReadOnly(t *testing.T) {
	c := newCompPanel(t, "u1", "u2")
	code, body := c.do(t, "GET", "/panel/api/compensation")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	if got := body["pending"]; got != float64(2) {
		t.Errorf("pending=%v want 2", got)
	}
	if got := body["credit"]; got != float64(1000) {
		t.Errorf("credit=%v want 1000", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimCalls != 0 {
		t.Errorf("只读查询触发了 %d 次领取", c.claimCalls)
	}
}

// TestPanelCompensationErrorsSurfaceInList 查询失败必须进 errored 计数（面板据此显形），
// 不得被当成「没有待领」——把故障说成「没活动」是本 PR 要消灭的那类界面谎言。
func TestPanelCompensationErrorsSurfaceInList(t *testing.T) {
	c := newCompPanel(t, "u1")
	c.mu.Lock()
	c.statusBody = `{"code":500,"msg":"boom"}` // 业务失败信封
	c.mu.Unlock()
	code, body := c.do(t, "GET", "/panel/api/compensation")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if body["pending"] != float64(0) {
		t.Errorf("pending=%v want 0", body["pending"])
	}
	if body["errored"] == float64(0) || body["errored"] == nil {
		t.Errorf("查询失败必须计入 errored: %v", body)
	}
}

// TestPanelCompensationOneAndAll 逐号与全量领取；summary 的 errored 与 claimed 分开计。
func TestPanelCompensationOneAndAll(t *testing.T) {
	c := newCompPanel(t, "u1", "u2")
	if code, body := c.do(t, "POST", "/panel/api/accounts/u1/compensation"); code != 200 {
		t.Fatalf("逐号领取 code=%d body=%v", code, body)
	}
	c.mu.Lock()
	if c.claimCalls != 1 {
		t.Errorf("逐号领取次数=%d want 1", c.claimCalls)
	}
	c.mu.Unlock()

	// 全量：u1 已领（桩状态恒为待领，故再领一次成功）、u2 领取失败 → claimed=1 errored=1
	// （两者分开，面板先看 errored）。
	c.mu.Lock()
	c.failClaimFor = "at-u2"
	c.mu.Unlock()
	code, body := c.do(t, "POST", "/panel/api/compensation_all")
	if code != 200 {
		t.Fatalf("全量 code=%d body=%v", code, body)
	}
	sum, _ := body["summary"].(map[string]any)
	if sum == nil {
		t.Fatalf("缺 summary: %v", body)
	}
	if sum["claimed"] != float64(1) || sum["errored"] != float64(1) {
		t.Errorf("summary=%v want claimed=1 errored=1（失败不得被吞成「没有待领」）", sum)
	}
	if sum["credit"] != float64(500) {
		t.Errorf("credit=%v want 500（只计成功到账）", sum["credit"])
	}
}

// TestPanelCompensationUnknownUID404 未知 uid 返回 404（面板据此区分「没有这个号」）。
func TestPanelCompensationUnknownUID404(t *testing.T) {
	c := newCompPanel(t, "u1")
	if code, _ := c.do(t, "POST", "/panel/api/accounts/nope/compensation"); code != http.StatusNotFound {
		t.Errorf("code=%d want 404", code)
	}
}

// TestPanelCompensationWithoutScheduler501 无调度器（未接入）时三个入口一律 501。
func TestPanelCompensationWithoutScheduler501(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), Upstream: &upstream.Client{}})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/compensation"},
		{"POST", "/panel/api/accounts/u1/compensation"},
		{"POST", "/panel/api/compensation_all"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("%s %s code=%d want 501", c.method, c.path, rr.Code)
		}
	}
}
