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
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sweepTask 假上游里的任务状态。accept/claim 会真实改状态——第二轮清扫因此能验证幂等
// （真实上游同款：重复领返回 already_claimed、已接受的任务 accept_status 变 accepted）。
type sweepTask struct {
	Code      string
	Current   int64
	Target    int64
	MPOnly    bool // 只在 mp 口径下发（默认列表不含）
	NotAcc    bool // accept_status=not_accepted（待接受）
	Locked    bool // 上游标记未解锁（达标也不该领）
	Accepted  bool
	Claimed   bool
	FailClaim bool // 领奖返回 500，验证单条失败不中断本轮
}

// sweepFake 假上游：记录每个端点被怎么调（口径 + 平台头），供断言「分流是否正确」。
type sweepFake struct {
	mu      sync.Mutex
	tasks   []*sweepTask
	accepts []string // "default" / "mp"
	claims  []string // "<code>|<x-client-platform>"
	reqs    int
}

func (f *sweepFake) taskJSON(t *sweepTask) map[string]any {
	status := "available"
	switch {
	case t.Claimed:
		status = "claimed"
	case t.Accepted:
		status = "accepted"
	case t.NotAcc:
		status = "not_accepted"
	}
	accept := ""
	switch {
	case t.Claimed:
		accept = "claimed"
	case t.Accepted:
		accept = "accepted"
	case t.NotAcc:
		accept = "not_accepted"
	}
	return map[string]any{
		"task_code": t.Code, "task_id": 1, "title": t.Code,
		"target": t.Target, "current": t.Current, "locked": t.Locked,
		"accept_status": accept, "status": status,
		"reward_credit": 100,
	}
}

func (f *sweepFake) writeList(w http.ResponseWriter, mp bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := []any{}
	for _, t := range f.tasks {
		if t.MPOnly && !mp {
			continue // mp 专属任务默认口径不下发
		}
		list = append(list, f.taskJSON(t))
	}
	body, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": list}})
	_, _ = w.Write(body)
}

func (f *sweepFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs++
		f.mu.Unlock()
		mp := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			f.writeList(w, mp)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			f.mu.Lock()
			kind := "default"
			if mp {
				kind = "mp"
			}
			f.accepts = append(f.accepts, kind)
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, c := range body.TaskCodes {
				for _, x := range f.tasks {
					if x.Code == c {
						x.Accepted = true
					}
				}
			}
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") &&
			strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			platform := r.Header.Get("x-client-platform")
			f.mu.Lock()
			f.claims = append(f.claims, code+"|"+platform)
			fail := false
			for _, x := range f.tasks {
				if x.Code == code {
					if x.FailClaim {
						fail = true
					} else {
						x.Claimed = true
					}
				}
			}
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":0}}`))
		default:
			t.Errorf("未预期的上游请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func sweepPanel(t *testing.T, f *sweepFake, accounts ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	return New(Config{Pool: p, Upstream: up})
}

// 待接受的任务被接受、达标未领的被领（默认口径与 mp 口径分开提交；mp 专属码走 mp 头）。
// 第二轮必须空转：假上游已改状态，重复领上游返回 already_claimed。
func TestTaskSweepAcceptsAndClaims(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{
		{Code: "chat_5", Current: 5, Target: 5},                           // 达标未领（非 mp）
		{Code: "Sequential_Tasks_1", Current: 1, Target: 1, MPOnly: true}, // mp 专属达标未领
		{Code: "Buddy_App", Current: 0, Target: 1, NotAcc: true},          // 待接受
		{Code: "Sequential_Tasks_2", Current: 1, Target: 1, Locked: true}, // 达标但未解锁：不领
	}}
	p := sweepPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})

	swept, msg := p.RunTaskSweepNow()
	if swept != 1 {
		t.Fatalf("swept=%d want 1；msg=%s", swept, msg)
	}
	if len(f.accepts) != 2 || f.accepts[0] != "default" || f.accepts[1] != "mp" {
		t.Errorf("accept 分流=%v want [default mp]（端标记不同，必须分开提交）", f.accepts)
	}
	if got := strings.Join(f.claims, ","); got != "chat_5|web,Sequential_Tasks_1|miniprogram" {
		t.Errorf("claim 分流=%q want chat_5|web,Sequential_Tasks_1|miniprogram", got)
	}

	// 第二轮：状态已被上游改过 → 零动作。
	f.claims = nil
	f.accepts = nil
	p.RunTaskSweepNow()
	if len(f.accepts) != 0 {
		t.Errorf("第二轮不应再接受，实际=%v", f.accepts)
	}
	if len(f.claims) != 0 {
		t.Errorf("第二轮不应再领奖，实际=%v", f.claims)
	}
}

// 同一任务在两种口径都下发时只能领一次（mp 列表是默认口径的超集，不去重会双领）。
func TestTaskSweepDedupesClaimAcrossReconciliations(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{{Code: "Library_read", Current: 1, Target: 1}}}
	p := sweepPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})
	p.RunTaskSweepNow()
	if len(f.claims) != 1 {
		t.Fatalf("claim 次数=%v want 恰好 1 次（两口径同码去重）", f.claims)
	}
}

// 单条领奖失败不中断本轮：其余达标任务照常领到。
func TestTaskSweepClaimFailureKeepsGoing(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{
		{Code: "chat_5", Current: 5, Target: 5, FailClaim: true},
		{Code: "skill_1", Current: 1, Target: 1},
	}}
	p := sweepPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})
	p.RunTaskSweepNow()
	var ok int
	for _, c := range f.claims {
		if strings.HasPrefix(c, "skill_1|") {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("失败码之后的达标任务仍应领到，claims=%v", f.claims)
	}
}

// 禁用 / 国际区 / 企业版账号一个上游请求都不发（门控与任务中心扫描一致）。
func TestTaskSweepSkipsGatedAccounts(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{{Code: "chat_5", Current: 5, Target: 5}}}
	useRealmForTest(t)
	global := testRealmAuth(t, "global-1", "global")
	ent := &auth.Auth{UID: "ent-1", Nickname: "ent", AccessToken: "at", EnterpriseID: "e1"}
	dis := &auth.Auth{UID: "dis-1", Nickname: "dis", AccessToken: "at"}

	p := pool.New("")
	for _, a := range []*auth.Auth{global, ent, dis} {
		p.Add(a)
	}
	p.Disable("dis-1", "test")
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pn := New(Config{Pool: p, Upstream: up})

	swept, _ := pn.RunTaskSweepNow()
	if swept != 0 {
		t.Errorf("清扫账号数=%d want 0", swept)
	}
	if f.reqs != 0 {
		t.Errorf("门控账号不应发起任何上游请求，实际 %d 次", f.reqs)
	}
}

// 持锁账号（正在跑任务行为链路）整轮跳过：一个上游请求都不能发——清扫是「补漏」，
// 不能去抢正在执行的动作，否则同一任务的 accept/claim 会和动作链交叠。解锁后下一轮
// 自然补上（上游 PR #205 的同款用例）。
func TestTaskSweepSkipsBusyAccount(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{{Code: "chat_5", Current: 5, Target: 5}}}
	p := sweepPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})

	if !p.tryLockAccount("cn-1") {
		t.Fatal("测试前置：应能拿到账号锁")
	}
	swept, msg := p.RunTaskSweepNow()
	if swept != 0 || f.reqs != 0 || len(f.claims) != 0 {
		t.Fatalf("持锁期间应整轮跳过：swept=%d reqs=%d claims=%v msg=%s", swept, f.reqs, f.claims, msg)
	}

	p.unlockAccount("cn-1")
	if swept, msg = p.RunTaskSweepNow(); swept != 1 || len(f.claims) != 1 {
		t.Fatalf("解锁后应补上：swept=%d claims=%v msg=%s", swept, f.claims, msg)
	}
}
