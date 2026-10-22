//go:build ignore

// probe169 是 issue #169 的真机测量工具：一条命令给出「安全吗 / 有效吗」两个结论。
//
// 2026-10-21 实测结果（官方 5.8.3 客户端通道，CN 域，deepseek-v4.1-flash）：
//   - 安全：带 conversation_id 三发全 200，不被渠道校验拒；
//   - 有效：**无效**。固定 id 与不带 id 的第二发命中率完全一致（90.9% vs 90.9%），
//     说明上游前缀缓存按内容共享，与本字段无关；hy4-preview / glm-5.3 两臂都是 0%，
//     是真不返回缓存命中，不是缺这个字段。
// 所以仓库里不再提供「回写 conversation_id」的开关（2026-10-21 已删除），
// 本工具保留为测量/复验手段。
//
// 背景：上游按 body 的 conversation_id 分前缀缓存槽（上报人的单变量 2×2），
// 但官方桌面客户端只发 X-Conversation-* 头、body 里没有该字段（96MB app.asar
// 全包检索零命中）。本工具就是对上面这条假设做判决。
//
//	阶段 A（安全）：带上该字段会不会被渠道校验拒（11-128 / 11140 这类 body 形态拒绝）；
//	阶段 B（有效）：同一段长前缀，固定 conversation_id 重放是否真的整段命中。
//
// 用法（仓库根目录，auth 目录两种格式都认）：
//
//	go run internal/probe/probe169.go -auths ./auths -realm cn
//	go run internal/probe/probe169.go -tokfile /path/token.txt -realm cn -uid <uid>
//
// 只打印状态码与 usage 缓存字段，**不打印任何 token**。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sample 是单次请求观测到的用量与缓存字段。
type sample struct {
	status int
	err    string
	ttfb   time.Duration
	prompt int64
	hit    int64
	miss   int64
	cached int64
	raw    string
}

func (s sample) hitRate() float64 {
	if s.prompt <= 0 {
		return 0
	}
	return float64(s.hit) * 100 / float64(s.prompt)
}

func (s sample) String() string {
	if s.err != "" {
		return fmt.Sprintf("status=%d ERR=%s", s.status, truncate(s.err, 120))
	}
	return fmt.Sprintf("status=%d ttfb=%5dms prompt=%d hit=%d miss=%d cached=%d hit%%=%.1f",
		s.status, s.ttfb.Milliseconds(), s.prompt, s.hit, s.miss, s.cached, s.hitRate())
}

func main() {
	authDir := flag.String("auths", "", "auth 文件目录")
	realm := flag.String("realm", "cn", "cn / global")
	model := flag.String("model", "", "模型 id（裸名；空 = 该域默认便宜模型）")
	tokFile := flag.String("tokfile", "", "token 文件（首行 access token，不回显）")
	uid := flag.String("uid", "probe169uid", "账号 uid（缓存键隔离段）")
	base := flag.String("base", "", "覆盖 chat base")
	phase := flag.String("phase", "all", "all / safe / cache")
	flag.Parse()

	a := pickAccount(*tokFile, *authDir, *realm, *uid)
	if a == nil {
		fmt.Printf("FAIL: 没有可用的 realm=%s 账号（-auths 或 -tokfile）\n", *realm)
		os.Exit(1)
	}
	uidShow := a.UID
	if len(uidShow) > 8 {
		uidShow = uidShow[:8]
	}
	fmt.Printf("account: uid=%s realm=%s\n", uidShow, a.Realm())

	c := upstream.New()
	if *base != "" {
		if a.IsGlobal() {
			c.ChatBaseGlobal = *base
		} else {
			c.ChatBaseCN = *base
		}
	}
	m := *model
	if m == "" {
		if a.IsGlobal() {
			m = "gpt-5.4"
		} else {
			m = "deepseek-v4.1-flash"
		}
	}
	fmt.Printf("model=%s base=%s/%s\n\n", m, c.ChatBaseCN, c.ChatBaseGlobal)

	safeOK := true
	if *phase == "all" || *phase == "safe" {
		safeOK = runPhaseA(c, a, m)
	}
	if *phase == "all" || *phase == "cache" {
		runPhaseB(c, a, m)
	}
	if !safeOK {
		os.Exit(1) // 安全没过：退出码非 0，便于脚本判断
	}
}

// pickAccount 从 token 文件或 auth 目录取一个账号（不回显凭证）。
func pickAccount(tokFile, authDir, realm, uid string) *auth.Auth {
	if tokFile != "" {
		raw, err := os.ReadFile(tokFile)
		if err != nil {
			fmt.Println("read tokfile:", err)
			return nil
		}
		tok := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		a := &auth.Auth{AccessToken: tok, UID: uid, ExpiresAt: time.Now().Add(time.Hour).Unix()}
		_, _ = auth.BackfillRealmFor(a, realm)
		return a
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		fmt.Println("read auths:", err)
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(authDir, e.Name()))
		if err != nil {
			continue
		}
		a := parseLoose(raw)
		if a == nil {
			continue
		}
		if a.IsGlobal() != (realm == "global") {
			continue
		}
		if a.ExpiresAt > 0 && time.Now().Unix() > a.ExpiresAt {
			continue
		}
		return a
	}
	return nil
}

// parseLoose 兼容两种 auth 文件格式（本仓嵌套/扁平形 + 其它面板导出的 token/user_id 形）。
func parseLoose(raw []byte) *auth.Auth {
	if a, err := auth.Parse(raw); err == nil && a != nil && a.AccessToken != "" {
		return a
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	tok := str("accessToken", "token", "access_token")
	if tok == "" {
		return nil
	}
	exp := int64(0)
	for _, k := range []string{"expiresAt", "expires_at"} {
		if v, ok := m[k].(float64); ok && v > 0 {
			exp = int64(v)
			break
		}
	}
	if exp > 1e12 {
		exp /= 1000
	}
	a := &auth.Auth{AccessToken: tok, UID: str("uid", "user_id", "userId"), ExpiresAt: exp, Nickname: str("nickname")}
	switch str("realm") {
	case "intl", "global", "international":
		_, _ = auth.BackfillRealmFor(a, "global")
	default:
		_, _ = auth.BackfillRealmFor(a, "cn")
	}
	return a
}

// runPhaseA 安全组：同一短请求三发（带固定 id / 不带 / 带随机 id），都必须 200。
func runPhaseA(c *upstream.Client, a *auth.Auth, model string) bool {
	msgs := `[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"回复一个字：好"}]`
	fmt.Println("=== 阶段 A：安全（带 conversation_id 会不会被拒）===")
	cases := []struct {
		label string
		conv  string
	}{
		{"A1 带固定 conversation_id", fixedConv()},
		{"A2 不带 conversation_id", ""},
		{"A3 带随机 conversation_id", "probe169-" + nonce()},
	}
	ok := true
	for i, cs := range cases {
		if i > 0 {
			time.Sleep(1200 * time.Millisecond)
		}
		s := send(c, a, model, msgs, cs.conv)
		fmt.Printf("  %-26s %s\n", cs.label, s)
		if s.status != 200 || s.err != "" {
			ok = false
			if s.raw != "" {
				fmt.Printf("  %-26s raw=%s\n", "", truncate(s.raw, 240))
			}
		}
	}
	if ok {
		fmt.Println("  → 结论 A：安全。三发全 200，渠道校验不介意 body 的 conversation_id。")
	} else {
		fmt.Println("  → 结论 A：不安全（或凭证/模型本身有问题）。")
		fmt.Println("     若失败的正是 A1/A3、而 A2 通过 → 上游在拒这个字段，开关必须保持关闭。")
	}
	fmt.Println()
	return ok
}

// runPhaseB 缓存组：3k token 同前缀 × 2 轮对照（固定 id vs 不带）。
func runPhaseB(c *upstream.Client, a *auth.Auth, model string) {
	fmt.Println("=== 阶段 B：有效（固定 conversation_id 能否整段命中）===")
	msgs := longMessages()
	conv := fixedConv()

	b1 := send(c, a, model, msgs, conv)
	fmt.Printf("  %-26s %s\n", "B1-a 固定 id 第 1 次", b1)
	time.Sleep(1500 * time.Millisecond)
	b2 := send(c, a, model, msgs, conv)
	fmt.Printf("  %-26s %s\n", "B1-b 固定 id 第 2 次", b2)
	time.Sleep(1500 * time.Millisecond)
	b3 := send(c, a, model, msgs, "")
	fmt.Printf("  %-26s %s\n", "B2-a 不带 id 第 1 次", b3)
	time.Sleep(1500 * time.Millisecond)
	b4 := send(c, a, model, msgs, "")
	fmt.Printf("  %-26s %s\n", "B2-b 不带 id 第 2 次", b4)

	withID := b2.hitRate()
	withoutID := b4.hitRate()
	fmt.Printf("\n  固定 id 复用命中率 = %.1f%%（B1-b）｜不带 id 命中率 = %.1f%%（B2-b）\n", withID, withoutID)
	switch {
	case b2.status != 200 || b4.status != 200:
		fmt.Println("  → 结论 B：无法判定（有请求失败，先看上面的 raw）。")
	case withoutID >= 50 && withID >= 50:
		fmt.Println("  → 结论 B：两种形态都命中 → 上游按内容前缀共享缓存，与本字段无关（本仓已不提供该开关）。")
	case withID-withoutID >= 20:
		fmt.Println("  → 结论 B：该模型上固定 conversation_id 明显提升命中率 —— 与 2026-10-21 的结论不同，")
		fmt.Println("     值得再查（把本次原始输出贴到 issue #169）。")
	case withID > withoutID:
		fmt.Println("  → 结论 B：有提升但幅度小（<20pp）。建议换多个模型各跑一遍再看。")
	default:
		fmt.Println("  → 结论 B：无效，与 2026-10-21 结论一致（命中率无差别或都为 0）。")
	}
	fmt.Println()
}

// send 发一次真实 chat 请求并解析 usage 缓存字段。
func send(c *upstream.Client, a *auth.Auth, model, messagesJSON, convID string) sample {
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":%s}`, model, messagesJSON)
	if convID != "" {
		var obj map[string]any
		if json.Unmarshal([]byte(body), &obj) == nil {
			obj["conversation_id"] = convID
			if nb, err := json.Marshal(obj); err == nil {
				body = string(nb)
			}
		}
	}
	meta := upstream.ChatMeta{ConversationRequestID: "probe169-" + nonce()}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	start := time.Now()
	rc, status, respBody, err := c.ChatStreamContext(ctx, a, []byte(body), "", meta, nil)
	s := sample{status: status, ttfb: time.Since(start)}
	if err != nil {
		s.err = err.Error()
		s.raw = string(respBody)
		return s
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	parseUsage(string(raw), &s)
	if status != 200 {
		s.raw = string(raw)
	}
	return s
}

// parseUsage 从 SSE 里取末帧 usage 的缓存字段。
func parseUsage(text string, s *sample) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		us, ok := obj["usage"].(map[string]any)
		if !ok {
			continue
		}
		s.prompt = num(us["prompt_tokens"])
		if v, ok := us["prompt_cache_hit_tokens"]; ok {
			s.hit = num(v)
		}
		if v, ok := us["prompt_cache_miss_tokens"]; ok {
			s.miss = num(v)
		}
		if v, ok := us["cached_tokens"]; ok {
			s.cached = num(v)
		}
		if d, ok := us["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := d["cached_tokens"]; ok {
				s.cached = num(v)
			}
		}
	}
}

// longMessages 造 ≈3k token 的稳定长前缀（同会话两次请求逐字节相同）。
func longMessages() string {
	var b strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "%03d cache-slot probe material: replay an identical long prefix so upstream can hit.\n", i)
	}
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
		map[string]any{"role": "user", "content": b.String() + "\n\n只回复一个字：好"},
	}
	out, _ := json.Marshal(msgs)
	return string(out)
}

func fixedConv() string { return "probe169-fixed-conversation" }
func nonce() string     { return fmt.Sprintf("%016x", time.Now().UnixNano()) }

func num(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
