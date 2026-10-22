package scheduler

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// recorderUpstream 记录全部被请求的路径，并对所有端点返回成功信封。
// 用「路径是否出现」断言门控是否生效，比逐端点计数器更能覆盖新增调用。
type recorderUpstream struct {
	mu    sync.Mutex
	paths []string
	// checkinActivityBody 签到活动状态应答（空 = 与既有默认同形的空 data 对象）；
	// checkinActivityCode 非 0 时该端点直接回错误（测 fail-open）。
	checkinActivityBody string
	checkinActivityCode int
}

func (r *recorderUpstream) record(p string) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
}

func (r *recorderUpstream) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func (r *recorderUpstream) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

func (r *recorderUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.record(req.URL.Path)
		switch {
		case strings.HasSuffix(req.URL.Path, "/get-enterprise-user-usage"):
			// 企业口径：credit = 本周期已用，limitNum = 分配给本账号的额度。
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":812.45,"limitNum":2000,` +
				`"cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"2099-10-26 23:59:59",` +
				`"cycleResetTime":"2099-10-27 00:00:00"}}`))
		case strings.HasSuffix(req.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}]}}}}`))
		case strings.HasSuffix(req.URL.Path, "/checkin-activity-status"):
			if r.checkinActivityCode != 0 {
				http.Error(w, `{"code":500,"msg":"boom"}`, r.checkinActivityCode)
				return
			}
			body := r.checkinActivityBody
			if body == "" {
				body = `{"code":0,"msg":"ok","data":{}}`
			}
			w.Write([]byte(body))
		default:
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
}

// enterpriseTestScheduler 构造只含一个账号的调度器；enterprise=true 时该号带
// enterpriseId（企业版），否则是个人号（对照组）。
func enterpriseTestScheduler(t *testing.T, enterprise bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	if enterprise {
		a.EnterpriseID = "ent-1"
	}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestEnterpriseGateSkipsGrowthTasks 企业版无个人成长体系：五类任务必须一个上游调用都不发。
// 每个子用例都配一组个人号对照（同构造、只差 enterpriseId），证明"没发请求"是门控
// 生效而不是构造有误——企业版门控与既有 IsGlobal/D4 门控同源。
func TestEnterpriseGateSkipsGrowthTasks(t *testing.T) {
	// 把夜猫子的窗口判定钉在窗口内：守卫先于账号循环，不钉住时钟则该用例的
	// 判别力取决于运行环境的墙钟（本机白天跑 = 什么都没测）。钉住后五个用例
	// 在任何环境、任何时刻都真正执行。
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 7, 23, 30, 0, 0, cst.Loc) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name string
		run  func(*Scheduler)
		// endpoint 是该任务在**个人号**上必然发起的上游路径，用来证明对照组
		// 确实有动作（否则"企业号没发请求"可能只是构造有误）。
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		// 夜猫子第一跳不是 heatmap：RunBlackcatNow → BlackcatNeed → ListTasks，
		// 即 /v2/activity/growth/tasks（问"还差几次"）。mock 返回空任务表 ⇒ need=0
		// ⇒ 个人号到此为止，只发这一条。
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sEnt, rEnt := enterpriseTestScheduler(t, true)
			c.run(sEnt)
			if rEnt.has(c.endpoint) {
				t.Errorf("企业号不应发起 %s，实际请求=%v", c.endpoint, rEnt.snapshot())
			}

			sPer, rPer := enterpriseTestScheduler(t, false)
			c.run(sPer)
			if !rPer.has(c.endpoint) {
				t.Errorf("个人号对照应发起 %s，实际请求=%v", c.endpoint, rPer.snapshot())
			}
		})
	}
}

// TestEnterpriseCheckinStillRefreshesCredits 企业号虽跳过签到，但**必须继续刷新额度**
// （否则额度永远不更新，面板与 credit_floor 都失去数据源）——这正是只 skip 签到、
// 不 continue 整轮的原因。
func TestEnterpriseCheckinStillRefreshesCredits(t *testing.T) {
	s, r := enterpriseTestScheduler(t, true)
	s.RunCheckinNow()

	if !r.has("/get-enterprise-user-usage") {
		t.Fatalf("企业号应走企业口径额度查询，实际请求=%v", r.snapshot())
	}
	if r.has("/billing/meter/get-user-resource") {
		t.Errorf("企业号不应再走个人资源包端点，实际请求=%v", r.snapshot())
	}
	st, ok := s.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	// limitNum - round(credit) = 2000 - 812 = 1188。
	if st.Credits != 1188 || st.CreditsTotal != 2000 {
		t.Errorf("企业额度映射错误：credits=%d total=%d want 1188/2000", st.Credits, st.CreditsTotal)
	}
}

// sessionTestScheduler 与 enterpriseTestScheduler 同构，只差 token 形态：
// switched=true 时 AccessToken 是带 token_source=enterprise_switch 的 JWT（该章只记
// 本次授权走了哪条流程，账号 enterpriseId 仍为空）；false 为个人号对照。
func sessionTestScheduler(t *testing.T, switched bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	token := "at"
	if switched {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch"}`))
		token = "hdr." + payload + ".sig"
	}
	a := &auth.Auth{UID: "u1", AccessToken: token, RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestStampedTokenNotGated 带 token_source=enterprise_switch 的**个人版**号不再被门控：
// 实测两个只带该章的号成长任务全部 claimed、行为事件照常计分（该章只记授权流程），
// 企业形态一律以 enterpriseId 为准（auth.IsEnterprise）。故五类任务必须照常发起上游
// 调用，与个人号对照完全一致——这是「别再把该章当企业上下文」的回归钉子。
func TestStampedTokenNotGated(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, cst.Loc) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name string
		run  func(*Scheduler)
		// endpoint 是该任务必然发起的上游路径。
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sStamped, rStamped := sessionTestScheduler(t, true)
			c.run(sStamped)
			if !rStamped.has(c.endpoint) {
				t.Errorf("带 enterprise_switch 章的个人号应照常发起 %s，实际请求=%v", c.endpoint, rStamped.snapshot())
			}

			sPlain, rPlain := sessionTestScheduler(t, false)
			c.run(sPlain)
			if !rPlain.has(c.endpoint) {
				t.Errorf("个人号对照应发起 %s，实际请求=%v", c.endpoint, rPlain.snapshot())
			}
		})
	}
}

// TestEnterpriseSessionKeepsKeepalive 带该章的号照常保活（token 会过期，授权流程
// 不影响选号/刷新能力）。
func TestEnterpriseSessionKeepsKeepalive(t *testing.T) {
	s, r := sessionTestScheduler(t, true)
	s.RunKeepaliveNow()
	if !r.has("/token/refresh") {
		t.Errorf("企业切换会话必须照常保活，实际请求=%v", r.snapshot())
	}
}

// TestEnterpriseGateKeepsKeepaliveAndBalance 企业版照常参与保活与选号前置能力
// （token 会过期、且它照常接流量），不受成长体系门控影响。
func TestEnterpriseGateKeepsKeepaliveAndBalance(t *testing.T) {
	s, r := enterpriseTestScheduler(t, true)
	s.RunKeepaliveNow()
	if !r.has("/token/refresh") {
		t.Errorf("企业号必须照常保活，实际请求=%v", r.snapshot())
	}
	if s.cfg.Pool == nil {
		t.Fatal("pool missing")
	}
}

// TestCheckinSkipsWriteWhenUpstreamSaysCheckedIn 上游口径说今天已签 → 不再发签到写调用。
// 本机 checkin_done 只记录我们自己发起的签到，用户从手机 / 官方客户端签过就看不到，
// 上游 today_checked_in 才权威；余额查询必须照常（跳过签到 ≠ 跳过额度刷新）。
func TestCheckinSkipsWriteWhenUpstreamSaysCheckedIn(t *testing.T) {
	s, r := enterpriseTestScheduler(t, false)
	r.checkinActivityBody = `{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,` +
		`"streak_days":17,"daily_credit":100,"end_time":"2026-10-31 23:59:59"}}`
	s.RunCheckinNow()

	for _, p := range []string{"/checkin-activity-status", "/get-user-resource"} {
		if !r.has(p) {
			t.Errorf("应请求 %s，实际=%v", p, r.snapshot())
		}
	}
	if r.has("/daily-checkin") {
		t.Errorf("上游已签到却仍发了签到写调用：%v", r.snapshot())
	}
	if st, ok := s.cfg.Pool.Status("u1"); !ok || !st.CheckinDone {
		t.Errorf("上游已签到应记账 checkin_done：%+v ok=%v", st, ok)
	}
}

// TestCheckinWritesWhenUpstreamSaysNotCheckedIn 上游说没签 → 照常签一次。
func TestCheckinWritesWhenUpstreamSaysNotCheckedIn(t *testing.T) {
	s, r := enterpriseTestScheduler(t, false)
	r.checkinActivityBody = `{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":false,"daily_credit":100}}`
	s.RunCheckinNow()
	if !r.has("/daily-checkin") {
		t.Errorf("上游未签到却没发签到：%v", r.snapshot())
	}
}

// TestCheckinFailsOpenWhenStatusQueryFails 活动状态查不到时 fail-open：照常签到——
// 新增的只读查询不得阻断签到这条有状态链路。
func TestCheckinFailsOpenWhenStatusQueryFails(t *testing.T) {
	s, r := enterpriseTestScheduler(t, false)
	r.checkinActivityCode = http.StatusInternalServerError
	s.RunCheckinNow()
	if !r.has("/daily-checkin") {
		t.Errorf("状态查询失败必须照常签到（fail-open）：%v", r.snapshot())
	}
}

// apiKeyTestScheduler 与 enterpriseTestScheduler 同构，只是账号换成静态 API Key：
// 用来验证「静态 Key 也进同一个门控」。两者共用 recorderUpstream，故门控行为
// ——「一个上游调用都不发」——用同一套判据断言。
func apiKeyTestScheduler(t *testing.T) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	// 静态 Key：无 refreshToken、无 expiresAt、无 enterpriseId——三者缺一都说明
	// 它是个「不能刷新、不会过期、没有企业身份」的凭证形态。
	a := &auth.Auth{UID: "k1", AccessToken: "ck_abc123.secret"}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), r
}

// TestAPIKeyGateSkipsGrowthTasks 静态 API Key 没有任何个人账号身份，五类成长任务
// 在上游对它都没有意义：必须一个请求都不发。配对断言「额度查询照常」——这是
// 只 skip 成长、不 continue 整轮的关键（否则面板额度永远不刷新）。
func TestAPIKeyGateSkipsGrowthTasks(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 7, 23, 30, 0, 0, cst.Loc) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name     string
		run      func(*Scheduler)
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, r := apiKeyTestScheduler(t)
			c.run(s)
			if r.has(c.endpoint) {
				t.Fatalf("API Key 账号不应发起 %s，实际请求=%v", c.endpoint, r.snapshot())
			}
		})
	}

	// 额度查询必须照常，且走企业口径端点。
	s, r := apiKeyTestScheduler(t)
	s.RunCheckinNow()
	if !r.has("/get-enterprise-user-usage") {
		t.Fatalf("API Key 账号应继续刷新额度（企业口径），实际请求=%v", r.snapshot())
	}
	if r.has("/billing/meter/get-user-resource") {
		t.Errorf("API Key 不该走个人资源包端点，实际请求=%v", r.snapshot())
	}
	st, ok := s.cfg.Pool.Status("k1")
	if !ok {
		t.Fatal("status not found")
	}
	if st.Credits != 1188 || st.CreditsTotal != 2000 {
		t.Errorf("额度映射错误：credits=%d total=%d want 1188/2000", st.Credits, st.CreditsTotal)
	}
	if !st.APIKey {
		t.Errorf("状态应带 api_key 标记：%+v", st)
	}
}

// TestAPIKeyKeepaliveSkipped 保活对静态 Key 无意义：它没有 refreshToken，刷新端点
// 只认 OAuth 设备流。既有代码按「refreshToken 为空即跳过」已经躲开，这里钉住这条
// 不变式——将来若有人把守卫改成「有 accessToken 就刷」，这个用例会红。
func TestAPIKeyKeepaliveSkipped(t *testing.T) {
	s, r := apiKeyTestScheduler(t)
	s.RunKeepaliveNow()
	for _, p := range r.snapshot() {
		if strings.Contains(p, "refresh") || strings.Contains(p, "oauth") {
			t.Fatalf("静态 API Key 不该触发刷新类请求：%v", r.snapshot())
		}
	}
}
