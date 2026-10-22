// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersionCN / defaultClientVersionGlobal 出站 WorkBuddy 客户端版本段
	// （UA 的 `<brand>/<ver>` 与白名单头组的 X-IDE-Version）。
	//
	// 取值来源：官方两个分发包的 resources/install-manifest.json —— 国内版 WorkBuddy
	// 5.8.3（endpoint www.workbuddy.cn）、国际版 WorkBuddy AI 5.8.4（endpoint
	// www.workbuddy.ai）。两版发版节奏不同，故**按 realm 分开**而不是共用一值：
	// 国际版号用 CN 的 5.8.3 会与上游实际分发包版本对不上。
	// config upstream.client_version 可覆盖（空 = 按 realm 取内置默认）。
	defaultClientVersionCN     = "5.8.3"
	defaultClientVersionGlobal = "5.8.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI（2.161.0，
	// 两版分发包内置同一版本，见 cli/package.json）。config upstream.cli_version 可覆盖。
	defaultCliVersion = "2.161.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则按 realm 取内置默认（CN 5.8.3 / global 5.8.4）。
func (c *Client) clientVersion(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方 `buildUserAgent` 形态）：
// `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`
//
// 首段**恒为驼峰 `WorkBuddy`**，只有第二段随 realm 换品牌：
//
//	CN 5.8.3     → WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0
//	global 5.8.4 → WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0
//
// 2026-10-21 国际版 5.8.4 GUI 真机抓包（/v2/chat/completions，主对话 + 标题生成
// 两条）实测即为上式：官方 chat 走 OpenAI SDK（请求带 x-stainless-* 头），SDK 的
// buildUserAgent 把首段固定成 productName 的驼峰形式 `WorkBuddy/<ver>`，第二段才用
// 打包期改写后的 slug `workbuddy-ai/<ver>`。
//
// 旧实现把首段也写成 realm 品牌（global → `workbuddy-ai/5.8.4 workbuddy-ai/5.8.4`），
// 与真机不符——那是 /v2/report、/v3/config 这类非 SDK 请求的形态，chat 链路从不用它。
// global 账号整体送错品牌段可能触发上游 403 code 11140 "request illegal" 风控。
// 官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	ver := c.clientVersion(a)
	return "WorkBuddy/" + ver + " " + uaBrandFor(a) + "/" + ver + " CLI/" + c.cliVersion()
}

// uaBrandFor 出站 UA 的**第二段**品牌：按 realm 取，两版形态不同（不同分发包）。
//
// 2026-10-20 真机双版本抓包实测（同一台机器、同一套代理）：
//   - 国内版 5.8.3（productName `WorkBuddy`）→ `WorkBuddy/5.8.3`，**驼峰原样**，
//     业务链路 28 个路径全部如此；
//   - 国际版 5.8.4（productName `WorkBuddy AI`）→ `workbuddy-ai/5.8.4`，**小写连字符
//     slug**，业务链路 22 个路径全部如此。
//
// 为什么两版规则不同：brand 段来自 buildUserAgent() 的 applicationName/productName，
// 而 applicationName 在打包期被改写。CN 包直接沿用驼峰 productName；GL 包走了
// Electron 的 userAgentFallback 替换路径（asciiSlug：非字母数字换成 `-` 并转小写），
// 于是 `WorkBuddy AI` → `workbuddy-ai`。
//
// 反例警示：曾把 CN 也写成小写 `workbuddy`（按 GL 规则统一），与实测不符——CN 的
// 每个业务请求都是驼峰，写成 slug 反而成了最明显的非官方指纹。
func uaBrandFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "workbuddy-ai"
	}
	return "WorkBuddy"
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// v3ProbeUA /v3/config 目录探测的官方 UA（真机实测形态）。
//
// 2026-10-20 双版本抓包实测，两版对同一个 /v3/config 端点发的 UA 并不同：
//
//	CN 5.8.3    → WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0   （三段，重复品牌）
//	global 5.8.4 → WorkBuddy/5.8.4 CLI/2.161.0                    （两段，单品牌）
//
// 旧实现在这里发 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 与
// `CLI/2.63.2 CodeBuddy/2.63.2` 两套 CodeBuddy 家族 UA —— 那是从旧版 IDE
// 客户端抄来的，WorkBuddy 分发包里根本不存在这两个串，属于最显眼的非官方指纹。
func (c *Client) v3ProbeUA(a *auth.Auth) string {
	ver := c.clientVersion(a)
	if a != nil && a.IsGlobal() {
		return "WorkBuddy/" + ver + " CLI/" + c.cliVersion()
	}
	return "WorkBuddy/" + ver + " " + uaBrandFor(a) + "/" + ver + " CLI/" + c.cliVersion()
}

// v3ProbeUALite /v3/config 第二路探测 UA：单品牌两段式。
//
// 保留双路探测是因为该端点按 UA 家族下发**不同模型集合**（实测 CN 19 / 32 / 54 条，
// global 13 / 22 / 29 条，三族各有独有模型），只探一族会让 /v1/models 少模型。
// 但两路都必须是官方真实出现过的形态——这里取 global 的 config UA 形态
// （`WorkBuddy/<ver> CLI/<cli>`，与 v3ProbeUA 在 CN 下的三段式互补）。
//
// 注意：global 账号下本函数与 v3ProbeUA 返回同一串（真机 global 只发这一种形态），
// 于是两路探测退化为重复请求——无害（并集去重），但不要据此认为 global 有第三族。
func (c *Client) v3ProbeUALite(a *auth.Auth) string {
	return "WorkBuddy/" + c.clientVersion(a) + " CLI/" + c.cliVersion()
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：**恒为 axios 默认串**。
//
// 2026-10-20 双版本真机抓包实测：两版 billing 层（summary / paid-packages /
// free-packages / checkin-activity-status / ambassador status）发的都是
// `axios/1.16.1`，与客户端自身 UA 完全不同——billing 域的 RestOperations 走
// axios 实例而非 SDK 客户端，所以不带任何品牌段。
//
// 此前 CN 侧发 `WorkBuddy/<ver>`：那是「按客户端 UA 推理」的产物，实测不符。
// 显式 client_name="SaaS" 时仍不设 UA（保留既有逃生开关，还原 Go 默认 UA）。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "axios/1.16.1"
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor 返回 Accept-Language。
//
// 2026-10-20 双版本真机抓包实测：这个头**不按账号域切**，而是跟随操作系统区域。
// 同一台中文系统机器上，CN 与 GL 的表现一致：多数请求根本不带该头（CN 128 条里
// 71 条无、GL 218 条里 80 条无），显式带上时以 `zh`（billing 层固定）与
// `zh-CN`（Chromium 渲染层）为主，`en-US` 只在 GL 出现 1 次。
//
// 旧实现「global → en-US」是按账号域臆测的，实测不成立（曾据此把国际版所有请求
// 都打成 en-US，反而成了与真机不符的指纹）。网关没有操作系统区域可言，统一取
// billing 层实测的 `zh`，两端同值。
func acceptLanguageFor(a *auth.Auth) string {
	return "zh"
}

// injectIdentityHeaders 注入官方客户端的身份头族（X-Auth-Method / X-Id-Source /
// X-Userinfo）。三个头都是**按账号形态条件发送**，不是常量——官方客户端的
// runIdentityHeaders()（cli 5.8.4 模块 30876 反编译）等价于：
//
//   有 uid           → X-User-Id = uid
//   有 enterpriseId  → X-Enterprise-Id = enterpriseId
//   有 auth.method   → X-Auth-Method = method（= JWT 的 token_source 声明）
//   有 idSource      → X-Id-Source = idSource
//   有 uid 且 (enterpriseId 或 idSource 或 method 任一存在) →
//       X-Userinfo = base64(JSON{uin, owner_uin?, id_source?, token_source?})
//
// X-Userinfo 的三段都是**条件拼接**：缺哪个字段就不出现在 JSON 里，而不是写空串。
// 个人号三个维度全缺——实测国际版个人账号的出站头里这三头都不存在，故本函数对纯
// 个人号零注入。这不是省略，是与客户端一致：凭「个人号也该有 X-Userinfo」去补空值
// 反而是伪造指纹，比不发更容易被识别。
//
// uin 段取 uid（不是账号的 uin 数字串）：反编译逻辑写的就是 {uin: n.uid}。
//
// 2026-10-22 补充（issue #51）：**个人号即使 token 带 token_source 也不发**。
// 起因是 `login.sh` 的设备流拿到的 CN token 声明里有 `token_source: "enterprise_switch"`，
// 旧逻辑据此给个人号注入了 `X-Auth-Method: enterprise_switch` + `X-Userinfo`，
// 而国际版 5.8.4 与国内版 5.8.3 真机实测的个人号**两个头都不发**
// （capture/raw3/001-chat.json、capture/raw8/002-chat.json）。
// `token_source` 描述的是 token 的来路（企业切换），不是账号身份；个人号的
// `enterprise_id` 为空，本就不存在「企业」这层语义。判定只看 EnterpriseID。
func (c *Client) injectIdentityHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	enterprise := strings.TrimSpace(a.EnterpriseID)
	if enterprise == "" {
		// 个人号：与官方客户端一致，三头全不发（不看 token_source）。
		return
	}
	method := a.TokenSource()
	if method != "" {
		req.Header.Set("X-Auth-Method", method)
	}
	payload := map[string]any{"uin": a.UID}
	payload["owner_uin"] = enterprise
	if method != "" {
		payload["token_source"] = method
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	// CacheKeyID prompt_cache_key 的会话段来源（网关派生的粘性会话键 sessKey）。
	// 与 ConversationID 分开：头是「客户端声明的会话」，缓存槽是「网关认的同一会话」——
	// 无 conversation_id 的客户端（Claude Code 等）只填这个，头部仍不伪造。
	CacheKeyID            string
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat 出站头剥离：CommonHeaders 是 REST 层通用形态，桌面端 chat 并不发这几个
	// （两版客户端真机实测，见 capture/raw3 + capture/raw8 的逐头对照）：
	//   Origin / Referer     ← CLI 客户端没有浏览器语义，chat 路径从不发
	//   Accept-Language      ← 桌面端 chat 不发（billing 层才偶发 zh）
	//   X-Machine-ID / X-Session-ID ← 这两个是网关自造的 36 hex 派生值，客户端没有
	// 保留在 CommonHeaders 里是为了不动 billing/checkin 等域（那层没有真机对照证据）。
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	req.Header.Del("X-Machine-ID")
	req.Header.Del("X-Session-ID")
	// Accept 与官方 chat 完全一致：客户端只发 application/json，照样收 SSE 流
	// （两版实测；旧值多带 ", text/event-stream" 属自造）。
	// Accept-Encoding 刻意不设：官方发 br/gzip 全集，但显式设置会关闭 net/http 的
	// 透明解压，收到 br 时网关没有 brotli 解码器 —— 收益（一个头）远小于风险。
	req.Header.Set("Accept", "application/json")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
		// 静态 API Key 账号：官方 CLI 两个头都发（Authorization 与 X-API-Key 任一
		// 均可鉴权）。双头对齐官方形态，避免只看 X-API-Key 的边缘网关判定缺头。
		if a.IsAPIKey() {
			req.Header.Set("X-API-Key", at)
		}
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业/域头：**只发有值的**。桌面端个人号实测两个方向都不发 X-No-* 哨兵头
	// （capture/raw3 国际版、capture/raw8 国内版，逐头对照均无 X-No-Enterprise-Id /
	// X-No-Department-Info）。旧实现用 X-No-* 声明「无企业」，那是 CodeBuddy CLI 的
	// 约定，不是 WorkBuddy 桌面端的形态——多发一个客户端从不发的头同样是指纹。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// X-Private-Data: true —— 官方 chat 出站恒带（applyGatewayHeaders 实测）。
	// 语义是「请求含私有数据」，客户端无条件声明，故这里也常发。
	req.Header.Set("X-Private-Data", "true")
	// 身份头族（X-Auth-Method / X-Userinfo）：按账号形态条件注入，见
	// injectIdentityHeaders——个人号三头全不发才是官方形态。
	c.injectIdentityHeaders(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, a.UID, meta)
	// SDK / agent 段指纹头（x-stainless-* / x-agent-type / x-agent-intent）。
	c.injectClientSDKHeaders(req)
}

// injectGlobalChatHeaders global 账号的 chat 专属域声明头。
//
// 只发 X-Domain：桌面端个人号实测并不发 X-No-Enterprise-Id（旧实现按 CodeBuddy CLI
// 约定补的哨兵头），国际版 5.8.4 与国内版 5.8.3 的真机报文里都不存在该头
// （capture/raw3/001-chat.json、capture/raw8/002-chat.json）。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定。客户端声明（body conversationId）→ 原值透传；
//     客户端没声明 → 用网关的 uid 隔离会话键（见 issue #169 实验：该头是前缀缓存槽的
//     实际来源，缺了它大前缀在 hy4/step 这类后端上恒 0% 命中）；两者都没有（零值
//     ChatMeta）→ 现造 32 hex，**不留空**——上游按三件套是否齐全做渠道校验，缺任一头
//     即 400 code=11-128 "conversation id missing"。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传，否则独立新值（**不**回落 conversationRequestID——官方实测
//     两族恒不相等，见下方实现注释）。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族，TraceId 跟随 X-Trace-ID。
//     B3 规范只认 16/32 hex TraceId 与 16 hex SpanId；入站透传值非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, uid string, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	} else if src := cacheKeySource(meta); src != "" {
		// 客户端没声明会话：用与 prompt_cache_key 同源的 uid 隔离键填该头。
		// 不带 uid 段的话，同一客户端会话打在不同账号上会共用同一个上游缓存槽（见文件头隔离约束）。
		req.Header.Set("X-Conversation-ID", buildCacheKey(uid, src))
	} else {
		// 零值 ChatMeta（夜猫子对话、面板模型任务等无 body 会话概念的调用方）：
		// 头族是**渠道校验**的一部分——上游要求三件套齐全，缺任一头即 400
		// code=11-128 "conversation id missing"（官方客户端 chat/speech 都带）。
		// 故不再留空，现造一个 32 hex；该路径无会话状态，不必跨请求稳定。
		req.Header.Set("X-Conversation-ID", session.NewMessageID())
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	// X-Trace-ID 与 request id **互不回落**：国际版 5.8.4 真机实测，X-Trace-ID 恒为
	// 一条独立的 32 hex（样本 0ece3294f88c4ec5a02e78547617f874），而
	// X-Conversation-Request-ID 是另一条时间有序 ID（01a160b2…），二者从无相等。
	// 旧实现空则回落 convReqID，导致两族头恒等——是「网关自造」的可见痕迹（issue #51）。
	// 入站透传值仍优先（客户端自己给了就尊重）。
	traceID := meta.TraceID
	if traceID == "" {
		traceID = session.NewTraceID()
	}
	req.Header.Set("X-Trace-ID", traceID)
	// B3 跟着 trace 走（官方 X-B3-TraceId == X-Trace-ID），而不是 request id。
	b3Trace := traceID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 入站透传值非法（含横线/超长/非 hex）→ 回落恒 32 hex
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	// SpanId 必须独立随机：官方 span（9795d0f16a68bd32）与 trace/request 均无关系。
	// 曾用 messageID[:16]，那会把这个头的 12 hex 时间前缀一起带出来。
	spanID := session.NewSpanID()
	req.Header.Set("X-B3-SpanId", spanID)
	// ParentSpanId：官方每条 chat 都带（两版实测），值是上一级 span 的 16 hex。
	// 网关没有真实 span 树，取独立随机值——上游无法校验父子关系，但**缺这个头**
	// 与「有 span 无 parent」在形态上是可区分的。
	parentSpanID := session.NewSpanID()
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	// B3 单头与 W3C traceparent：官方两版都双发（b3 = trace-span-flags-parent，
	// traceparent = 00-trace-span-flags）。缺这两个头时只有 X-B3-* 三件套，
	// 而客户端从不只发三件套。
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("traceparent", "00-"+b3Trace+"-"+spanID+"-01")
	// acp-connection-id：官方把它列进**标准出站头族**（asar applyGatewayHeaders 与
	// X-Conversation-ID / X-Conversation-Request-ID 同一批设置），值来自会话注册表的
	// acpConnectionId —— 每个 session 生成一次、同会话多轮复用、跨会话不同
	// （`acquire(): metadata={acpConnectionId: uuid(), conversationRequestId: uuid()}`）。
	// 网关没有 ACP 连接层，按「同 (账号, 会话) 稳定、与会话 ID 不同值」派生，
	// 语义与官方一致；无会话源时现造一个（该路径无跨请求稳定语义）。
	if src := cacheKeySource(meta); src != "" {
		req.Header.Set("acp-connection-id", buildStableUUID(uid, src, "acp"))
	} else {
		req.Header.Set("acp-connection-id", session.NewConversationID())
	}
}

// 官方 chat 出站的自报 SDK 指纹（国际版 5.8.4 与国内版 5.8.3 实测**完全一致**，
// 见 capture/raw3/001-chat.json 与 capture/raw8/002-chat.json）。
//
// 这些是 openai-node SDK 自动加的 x-stainless-* 头 + 业务语义头 x-agent-type/intent。
// 网关是 Go 实现，没有对应的 SDK 运行时；这里按官方实测值固定，理由是「客户端形态
// 一致性」优先——不发这几个头，上游看到的是一份缺了 SDK 段的残缺头集合。
// 一旦客户端版本变化（package-version / node 版本），这几个常量需要跟着更新。
const (
	clientArch     = "x64"
	clientOS       = "Windows"
	clientLang     = "js"
	clientRuntime  = "node"
	clientNodeVer  = "v22.21.1"
	clientSDKVer   = "6.25.0"
	agentTypeMain  = "main"
	agentIntentCrf = "craft"
)

// injectClientSDKHeaders 注入 x-stainless-* 与 x-agent-* 两族出站头。
// 仅在 chat 路径调用（ChatHeaders）——billing / checkin 等 REST 域没有真机对照证据，
// 不臆造。
func (c *Client) injectClientSDKHeaders(req *http.Request) {
	req.Header.Set("X-Agent-Type", agentTypeMain)
	req.Header.Set("X-Agent-Intent", agentIntentCrf)
	req.Header.Set("X-Stainless-Lang", clientLang)
	req.Header.Set("X-Stainless-Package-Version", clientSDKVer)
	req.Header.Set("X-Stainless-OS", clientOS)
	req.Header.Set("X-Stainless-Arch", clientArch)
	req.Header.Set("X-Stainless-Runtime", clientRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", clientNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// X-Product 恒为 "SaaS"，与 X-IDE-* 分开取值——这是真机抓包纠正过的形态（issue #51）：
//
//	国际版 5.8.4 出站：X-Product: SaaS  X-IDE-Name: WorkBuddy  X-IDE-Type: WorkBuddy  X-IDE-Version: 5.8.4
//	（capture/raw3/001-chat.json 官方全量头；CN 侧 desktop.go 文件头的逆向结论同为 SaaS）
//
// 旧实现让 X-Product 也取客户端名（=WorkBuddy），于是同一个头官方发 SaaS、网关发 WorkBuddy
// ——「同名不同值」比多一个头更容易被规则命中。X-IDE-* 才是承载产品名的地方。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	// X-Product 是 SKU 段，恒 SaaS：官方两版实测都不是产品名。
	req.Header.Set("X-Product", "SaaS")
	if name == "SaaS" {
		// 显式 ClientName=SaaS：仅声明 SKU，不补 X-IDE-*（旧行为，保留兼容）。
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion(a))
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
