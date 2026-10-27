// login.go 面板内嵌的 WorkBuddy CN OAuth 设备授权流程（cmd/login 的进程内移植）。
//
//	POST /panel/api/login/start → 拿 state+authUrl，state 存进程内（不再落 /tmp，
//	  原方案在 Windows 上不可用），返回授权 URL；
//	GET  /panel/api/login/poll   → 面板前端每 3s 轮询本接口；未完成返回 done=false，
//	  完成后取 uid/nickname、凭证落盘 auths/workbuddy-<uid>.json、热加载进池
//	  （pool.Add + Revive），并顺带签到 + 余额刷新 —— 免重启加载新账号。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state），请求头与上游端点与 cmd/login 保持一致。
package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/loginsite"
)

const (
	// clientUA 设备授权链路 UA（与授权链接 version 参数同源，官方口径集中在
	// internal/loginsite，避免面板与 cmd/login 各写一份）。
	clientUA = loginsite.ClientUA
)

// loginSite 别名：站点定义来自 internal/loginsite（官方域表的唯一来源）。
type loginSite = loginsite.Site

// 站点标识别名（面板内部沿用短名）。
const (
	siteCN        = loginsite.SiteCN
	siteIntl      = loginsite.SiteIntl
	siteWorkBuddy = loginsite.SiteWorkBuddy
)

// siteResolver 站点表查询入口（测试可替换为指向 httptest 服务器的站点，
// 否则 loginStart/loginPoll 的单测必须打真实上游）。
var siteResolver = loginsite.For

// siteOrDefault 归一化站点标识：空/非法 → cn（老调用零回归）。
func siteOrDefault(id string) loginSite { return siteResolver(id) }

// siteForRealm 按 realm 映射站点：global → 旧桌面端 workbuddy.ai（历史行为，
// 保持 login.sh / 老前端 realm=global 的语义不变），cn → 国内站。
// 新代码应直接用 siteOrDefault（官方国际站要显式 site=intl）。
// 走 siteResolver 而非直接查表，使单测能把两站点都指到 httptest 服务器。
func siteForRealm(realm string) loginSite { return siteResolver(loginsite.ForRealm(realm).ID) }

// loginEndpointsFor 按站点返回设备授权三端点（auth/state、token、account）+ Origin。
func loginEndpointsFor(s loginSite) (state, token, account, origin string) { return s.Endpoints() }

// loginEndpoints 兼容旧签名：按 realm 选站点（global → workbuddy 旧桌面端）。
func loginEndpoints(realm string) (state, token, account, origin string) {
	return loginEndpointsFor(siteForRealm(realm))
}

// 官方登录码语义（码表与判定在 internal/loginsite，两处调用方共用）。
const (
	loginCodeRetryFetchToken   = loginsite.CodeRetryFetchToken
	loginCodeRetryFetchAccount = loginsite.CodeRetryFetchAccount
	loginCodeLicenseSeatLimit  = loginsite.CodeLicenseSeatLimit
	loginCodeLicenseExpired    = loginsite.CodeLicenseExpired
	loginCodeTrialExpired      = loginsite.CodeTrialExpired
)

// loginCodePending 报告该业务码是否表示「登录进行中，应继续轮询」。
func loginCodePending(code int) bool { return loginsite.CodePending(code) }

// loginCodeFatal 报告该业务码是否属于官方定义的终态许可错误（SignLicenseError）。
func loginCodeFatal(code int) bool { return loginsite.CodeFatal(code) }

// loginEnvelopeError 上游业务码非 0 的结构化错误：调用方按官方码表区分「继续
// 轮询」与「终态失败」，不必解析错误字符串。
type loginEnvelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *loginEnvelopeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// validUID 校验上游返回的 uid 是否可安全用于拼文件名。
// 只放行字母、数字、下划线、连字符（腾讯侧 uid 实测为 UUID 形态），
// 长度上限 64 兜底异常超长串；拒绝 . / \ 等路径字符与空串。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// apiEnvelope 与 upstream 同形：{code,msg,data}，code!=0 视为业务错误。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封。origin 为 Origin/Referer 基础域（随 realm 切）。
func doJSON(method, fullURL, bearer string, body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, origin)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		// 结构化错误：调用方按官方码表区分 pending / 终态（见 loginCodePending/Fatal）。
		return nil, resp.StatusCode, &loginEnvelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

// loginDeviceNonce 官方 ww_device_nonce 的来源（桌面端 Turing 设备指纹 SDK）。
// 本仓无 SDK，取部署侧环境变量；空 = 不加该参数（不伪造设备指纹）。
func loginDeviceNonce() string {
	return strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_NONCE"))
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"site":"cn"|"intl"|"workbuddy"}（官方域表，缺省 cn）；
// 兼容老前端 {"realm":"global"} → 旧桌面端 workbuddy.ai（零回归）。
// state 会话记站点，poll 用同一站点取 token/落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	site := siteOrDefault("")
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
			Site  string `json:"site"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Site != "" {
				// site 显式优先（官方国际站走 site=intl）。
				site = siteOrDefault(strings.ToLower(strings.TrimSpace(reqBody.Site)))
			} else {
				site = siteForRealm(reqBody.Realm)
			}
		}
	}
	epState, _, _, origin := loginEndpointsFor(site)
	data, status, err := doJSON(http.MethodPost, epState, "", bytes.NewReader([]byte("{}")), origin)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	// 对齐官方：授权链接补 version（workbuddy 域再加设备 nonce）。
	st.AuthURL = loginsite.DecorateAuthURL(st.AuthURL, site, loginDeviceNonce())
	p.loginMu.Lock()
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = loginSession{created: time.Now(), realm: site.Realm, site: site.ID}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s site=%s（state=%s...）", site.Realm, site.ID, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": site.Realm, "site": site.ID})
}

// logPollOnce 上游轮询结果变了才落一行日志（state 前缀 + 上游原文）。
//
// 为什么需要：浏览器侧报「登录失败」时，面板此前只有「发起 OAuth」一行记录——
// code/msg 全丢在轮询里，运维无法判断是「浏览器根本没授权成功」还是「授权了但
// 取 token 被拒」。逐轮记不行：前端 3s 一轮、最长 loginTTL，一条 state 能刷出
// 300 行，真正的一次性错误反而被淹掉。state 已不在表里（超时 / 已落盘）则不记。
func (p *Panel) logPollOnce(state, msg string) {
	p.loginMu.Lock()
	s, ok := p.logins[state]
	if !ok || s.lastMsg == msg {
		p.loginMu.Unlock()
		return
	}
	s.lastMsg = msg
	p.logins[state] = s
	p.loginMu.Unlock()
	log.Printf("panel: 登录轮询 state=%s... 上游=%s", state[:min(8, len(state))], msg)
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	if known && sess.processing {
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "正在写入凭据…"})
		return
	}
	if known {
		sess.processing = true
		p.logins[state] = sess
	}
	p.loginMu.Unlock()
	if !known {
		// 410：state 已作废（进程重启或超过 loginTTL）。前端据此停止轮询；网络抖动
		// （5xx/超时）则继续重试——混成一个状态码时，一次抖动就让用户放弃等待，
		// 而 OAuth 其实已在浏览器侧完成、账号却不会入池。
		writeErr(w, http.StatusGone, "login session 已过期，请关闭后重新添加账号")
		return
	}
	// 失败路径复位闸门，供用户重试（成功路径 delete(state) 已经清掉）。
	defer func() {
		p.loginMu.Lock()
		if s, ok := p.logins[state]; ok {
			s.processing = false
			p.logins[state] = s
		}
		p.loginMu.Unlock()
	}()
	_, epToken, epAcct, origin := loginEndpointsFor(siteOrDefault(sess.site))

	// auth/token 是权威登录状态端点。官方码表语义：
	//   11217（RetryFetchToken）→ 还没登完，继续轮询；
	//   12005/11212/11216（席位满/许可证过期/试用到期）→ 终态，立刻把上游 msg
	//   抛给前端并停轮询（官方 SignLicenseError 行为），不再空转到 TTL。
	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, "", nil, origin)
	if err != nil {
		var envErr *loginEnvelopeError
		if errors.As(err, &envErr) && loginCodeFatal(envErr.Code) {
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			log.Printf("panel: 登录被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
			writeJSON(w, http.StatusOK, map[string]any{
				"done":    false,
				"fatal":   true,
				"code":    envErr.Code,
				"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
			})
			return
		}
		// pending（11217/12151）或其它可重试错误：面板前端继续轮询。
		p.logPollOnce(state, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	// 完成：取 uid/nickname。官方 12151（RetryFetchAccount）是「token 已发但账号信息
	// 还在落库」，属于 pending，必须继续轮询而不是报错——否则刚登录成功的号会被
	// 误判成失败（用户重登又撞同一个窗口）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(http.MethodGet, epAcct+state, tok.AccessToken, nil, origin)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	} else {
		var envErr *loginEnvelopeError
		if errors.As(errAcct, &envErr) {
			if loginCodePending(envErr.Code) {
				p.logPollOnce(state, errAcct.Error())
				writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": errAcct.Error()})
				return
			}
			if loginCodeFatal(envErr.Code) {
				p.loginMu.Lock()
				delete(p.logins, state)
				p.loginMu.Unlock()
				log.Printf("panel: 登录账号信息被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
				writeJSON(w, http.StatusOK, map[string]any{
					"done":    false,
					"fatal":   true,
					"code":    envErr.Code,
					"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
				})
				return
			}
		}
	}
	if acct.UID == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）")
		return
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	isNewAccount := p.cfg.Pool.AuthByUID(acct.UID) == nil
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	// realm 分支：CN 走 DailyCheckin；global 无 CN 签到体系，改为注册激活 + trial 领取
	// （D4 门控同 scheduler：CN 任务端点对 global 不发起任何调用）。
	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		// 注册激活（幂等）：region required 时自动补地区（白名单首个，HK）后重新激活。
		// 失败不阻断登录结果（auth 已落盘），只在返回字段里体现。
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "注册激活失败: " + err.Error()
			log.Printf("panel: global 注册激活 uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global 注册激活 uid=%s 完成", acct.UID)
		}
		// trial 加油包（幂等 14051 = 已领过，非错误）。
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s 已领", acct.UID)
		}
	} else if !a.IsEnterprise() {
		// 企业版跳过签到：上游 POST /v2/billing/meter/daily-checkin 对企业号
		// 400 code 10001「企业账号不支持该操作」，发了只会把该错误写进登录返回。
		// 下方 UserResource 余额查询照常（企业额度走 get-enterprise-user-usage 口径）。
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	balanceErr := ""
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	} else {
		// 凭据已落盘但上游仍拒绝 billing（典型：旧会话没清干净 / 渠道降级 token）。
		// 不判登录失败（chat 可能可用），但必须如实回传，否则运维只会看到「已添加」
		// 却继续 401，误以为重新登录没用。
		balanceErr = err.Error()
		log.Printf("panel: 登录后余额校验失败 uid=%s: %v", acct.UID, err)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	if isNewAccount {
		p.StartNewAccountTasks(acct.UID)
	}
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
		"balance_error":   balanceErr,
	})
}

// joinMsg 拼接 login 完成后的提示消息（多段用「；」连接，空段跳过）。
func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

// loginRegions 返回 global 注册可选地区（panel 前端选地区弹窗用；CN 不调用）。
// 未持账号时返回白名单静态兜底（前端只读展示，不依赖上游）。
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// 静态白名单（对齐国际版 web 展示集）：面板前端只读展示，无需账号态。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}
