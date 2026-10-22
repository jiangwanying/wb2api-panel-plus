package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// compensationStub 桩 /billing/meter/compensation-status，返回给定 body。
func compensationStub(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/compensation-status" {
			t.Errorf("path=%s want /billing/meter/compensation-status", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// TestGetCompensationStatusParsesAllFields 全字段解析（线上实测口径）。
func TestGetCompensationStatusParsesAllFields(t *testing.T) {
	c := compensationStub(t, `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,`+
		`"credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}`)
	st, err := c.GetCompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !st.Active || st.Claimed || st.CreditNum != 500 || st.ValidityPeriod != 1 || st.EndTime != "2026-10-22 23:59:59" {
		t.Errorf("解析不符: %+v", st)
	}
	if !st.Pending() {
		t.Error("active && !claimed && credit_num>0 应判待领")
	}
}

// TestCompensationPendingThreeWayAnd 三与门逐条判别：任一条不成立即「无需领取」。
// 无活动的账号会让该接口天天被调，误判成「待领」就等于每天对上游发一次无意义领取。
func TestCompensationPendingThreeWayAnd(t *testing.T) {
	cases := []struct {
		name string
		st   CompensationStatus
		want bool
	}{
		{"可领未领", CompensationStatus{Active: true, Claimed: false, CreditNum: 500}, true},
		{"无活动", CompensationStatus{Active: false, Claimed: false, CreditNum: 500}, false},
		{"已领过", CompensationStatus{Active: true, Claimed: true, CreditNum: 500}, false},
		{"活动在但无额度", CompensationStatus{Active: true, Claimed: false, CreditNum: 0}, false},
	}
	for _, c := range cases {
		if got := c.st.Pending(); got != c.want {
			t.Errorf("%s: Pending()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestGetCompensationStatusNoActivityIsNotError active=false 是正常 200（无补偿活动），
// 必须返回 nil error —— 否则没活动的账号会把排程日志刷成一片错误。
func TestGetCompensationStatusNoActivityIsNotError(t *testing.T) {
	c := compensationStub(t, `{"code":0,"msg":"OK","data":{"claimed":false,"active":false,"credit_num":0}}`)
	st, err := c.GetCompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("无活动不该报错: %v", err)
	}
	if st.Pending() {
		t.Error("无活动不该判待领")
	}
}

// TestGetCompensationStatusErrorsOnHTTPFailure 真失败要显形：非 2xx 必须返回 error
// （旧盲领写法正是在这里把失败吞掉）。
func TestGetCompensationStatusErrorsOnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":401,"msg":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GetCompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
		t.Error("401 必须返回 error（失败不得静默）")
	}
}

// TestGetGiftStatusPathAndShape 礼包状态：路径**无 /v2 前缀**（带 /v2 上游 404），
// 字段与补偿同构（线上实测 shape）。
func TestGetGiftStatusPathAndShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/check-gift-claimed" {
			t.Errorf("path=%s want /billing/meter/check-gift-claimed（无 /v2 前缀）", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"claimed":true,` +
			`"claimed_at":"2026-09-24 18:15:15","active":true,"credit_num":1500,` +
			`"validity_period":1,"start_time":"2026-04-01 00:00:00","end_time":"2026-10-31 23:59:59"}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	st, err := c.GetGiftStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !st.Active || !st.Claimed || st.CreditNum != 1500 || st.EndTime != "2026-10-31 23:59:59" {
		t.Fatalf("解析不符: %+v", st)
	}
	if st.Pending() {
		t.Fatal("已领过不得判为待领（盲领会据此打出假 +0c 成功行）")
	}
}
