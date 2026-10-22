package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestEnterpriseResourceMapsAllocatedQuota 企业版额度走 get-enterprise-user-usage：
// credit 是**已用**（与个人口径相反），故 remain = limitNum - credit、total = limitNum。
func TestEnterpriseResourceMapsAllocatedQuota(t *testing.T) {
	end := time.Now().Add(10 * 24 * time.Hour).Format(packageEndLayout)
	var gotPath, gotEnt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEnt = r.Header.Get("X-Enterprise-Id")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":812.45,"limitNum":2000,` +
			`"cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"` + end + `",` +
			`"cycleResetTime":"2026-10-27 00:00:00"}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	// soon=0：不分桶，但最早到期批次仍要记录。
	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasSuffix(gotPath, "/v2/billing/meter/get-enterprise-user-usage") {
		t.Errorf("path=%s want .../get-enterprise-user-usage", gotPath)
	}
	if gotEnt != "ent-1" {
		t.Errorf("X-Enterprise-Id=%q want ent-1", gotEnt)
	}
	// 2000 - round(812.45) = 1188。
	if remain != 1188 || total != 2000 {
		t.Errorf("remain=%d total=%d want 1188/2000", remain, total)
	}
	if expiring != 0 {
		t.Errorf("soon=0 不应分桶，expiring=%d", expiring)
	}
	if earliestAt.IsZero() || earliestRemaining != 1188 {
		t.Errorf("最早到期批次未记录：at=%v remaining=%d", earliestAt, earliestRemaining)
	}

	// soon 覆盖周期末 → 整份剩余额度计入 expiring（prefer_expiring 据此优先消耗）。
	_, _, expiring2, _, _, err := c.UserResourceDetailedWithExpiry(a, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if expiring2 != 1188 {
		t.Errorf("周期内 expiring=%d want 1188", expiring2)
	}
}

// TestEnterpriseResourceUnlimited 上游 limitNum=-1 表示不限量：池内用大剩余量表示，
// total 记 -1（面板显示「不限」），且不参与周期分桶（不限量没有"未用完作废"语义）。
func TestEnterpriseResourceUnlimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":0,"limitNum":-1}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(a, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != enterpriseUnlimitedRemain || total != enterpriseUnlimitedTotal {
		t.Errorf("remain=%d total=%d want %d/-1", remain, total, enterpriseUnlimitedRemain)
	}
	if expiring != 0 || !earliestAt.IsZero() || earliestRemaining != 0 {
		t.Errorf("不限量不应有周期批次：expiring=%d at=%v remaining=%d", expiring, earliestAt, earliestRemaining)
	}
}

// TestEnterpriseResourceSnakeCase 兼容上游第二套字段命名（桌面端另一条解析路径读
// limit_num/used_num）。
func TestEnterpriseResourceSnakeCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"used_num":500,"limit_num":1500}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 1000 || total != 1500 {
		t.Errorf("remain=%d total=%d want 1000/1500", remain, total)
	}
}

// TestPersonalResourceStaysOnPackageEndpoint 个人号零回归：仍走资源包端点，
// 企业端点一次都不发。
func TestPersonalResourceStaysOnPackageEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "get-enterprise-user-usage") {
			t.Errorf("个人号不应调用企业额度端点")
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
			`[{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}]}}}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 900 || total != 1000 {
		t.Errorf("remain=%d total=%d want 900/1000", remain, total)
	}
	if len(paths) == 0 {
		t.Fatal("应至少请求一次资源包端点")
	}
}

// TestAPIKeyResourceUsesEnterpriseEndpoint 静态 API Key 的额度必须走企业口径端点。
//
// 判据为什么收在 auth.UsesEnterpriseBilling：个人 get-user-resource 对 ck_/pt_ Key
// 恒返回空 Accounts（实测 code 0、Accounts null），若这里漏掉 Key 分支，静态 Key 账号
// 的余额会恒为 0，积分保底（credit_floor）把它当成耗尽号直接挡出选号——号是好的，
// 只是账单查错了地方。
func TestAPIKeyResourceUsesEnterpriseEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.Contains(r.URL.Path, "get-enterprise-user-usage") {
			t.Errorf("静态 API Key 不该走个人资源包端点：%s", r.URL.Path)
		}
		// 静态 Key 无企业身份，不该注入 X-Enterprise-Id（否则上游按企业上下文鉴权）。
		if ent := r.Header.Get("X-Enterprise-Id"); ent != "" {
			t.Errorf("静态 Key 不该带 X-Enterprise-Id：%q", ent)
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"limitNum":1000}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	for _, token := range []string{"ck_abc123.secret", "pt_zzz.999"} {
		paths = nil
		a := &auth.Auth{UID: "u1", AccessToken: token}
		remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
		if err != nil {
			t.Fatalf("token=%q err=%v", token, err)
		}
		if remain != 900 || total != 1000 {
			t.Errorf("token=%q remain=%d total=%d want 900/1000", token, remain, total)
		}
		if len(paths) == 0 {
			t.Fatalf("token=%q 应至少请求一次额度端点", token)
		}
	}
}
