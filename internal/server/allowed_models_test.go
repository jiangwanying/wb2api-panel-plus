// allowed_models_test.go config models.allowed（模型白名单，issue #68）的生效面回归。
//
// 契约与 hidden_models_test.go 对称，缺一都算没实现：
//  1. 空配置 = 全放行（向后兼容基线）；
//  2. GET /v1/models 列表过滤（cn / global / 出图单列只列白名单项）；
//  3. 点名调用白名单外模型直接拒绝（chat 与出图两条入口）——只滤列表不够，
//     知道名字仍可硬调，而且硬调会真的打上游、真的花积分；
//  4. 别名解析后的真实目标同样受检（别名不能绕过白名单）；
//  5. 与 models.hidden 共存取交集：白名单圈定「可能可见」集合，黑名单再剔除；
//  6. 面板保存后热生效（livecfg 快照优先于启动配置），不需要重启进程。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/livecfg"
	"github.com/jiangwanying/wb2api-panel-plus/internal/upstream"
)

// newCatalogChatUpstream 双面 fake：GET（目录探测 /v3/config 与企业端点）回
// hiddenCatalog 目录；POST（chat）捕获出站 body 后回 sseOK 流。热生效等需要
// 「同一 handler 先看列表再发对话」的用例用它；len(bodies) 即上游请求次数。
func newCatalogChatUpstream(t *testing.T, chatBodies *[]string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				raw, _ := io.ReadAll(r.Body)
				*chatBodies = append(*chatBodies, string(raw))
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sseOK)),
				}, nil
			}
			_, catalog, _ := hiddenCatalog()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(catalog)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// newCatalogImageUpstream 双面 fake：GET 目录探测回 hiddenCatalog；出图端点
// POST 捕获请求 imageOK 信封。出图兜底用例用它（兜底候选来自目录解析
// 的旁路留存，必须先探测过一次目录）。
func newCatalogImageUpstream(t *testing.T, imageBodies *[]string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v2/images/generations" {
				raw, _ := io.ReadAll(r.Body)
				*imageBodies = append(*imageBodies, string(raw))
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(imageOK)),
				}, nil
			}
			_, catalog, _ := hiddenCatalog()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(catalog)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// modelListIDs 触发一次 GET /v1/models，返回 id 集合。
func modelListIDs(t *testing.T, h *Handler) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models: code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/v1/models 响应不是 JSON: %v body=%s", err, rec.Body)
	}
	seen := map[string]bool{}
	for _, m := range resp.Data {
		seen[m["id"].(string)] = true
	}
	return seen
}

func postChat(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

func postImages(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
	return rec
}

// 向后兼容基线：未配置白名单时一切照旧——列表全量、任意模型可点名。
func TestModelsAllowedEmptyPassesAll(t *testing.T) {
	resetDynamicModelsCache()
	var chatBodies []string
	up := newCatalogChatUpstream(t, &chatBodies)
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: nil, // 不配置 = 白名单不启用
	})
	seen := modelListIDs(t, h)
	for _, id := range []string{"cn:glm-5.2", "cn:kimi-k3-1", "cn:hunyuan-image-alpha"} {
		if !seen[id] {
			t.Errorf("未启用白名单时 %s 必须照常可见（全部：%v）", id, seen)
		}
	}
	if !h.allowedModel("cn", "任何没登记的模型") || !h.allowedModel("global", "anything") {
		t.Error("白名单未启用时 allowedModel 必须恒为 true")
	}
	if rec := postChat(h, "kimi-k3-1"); rec.Code != 200 {
		t.Errorf("未启用白名单时点名应照常放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if len(chatBodies) != 1 {
		t.Errorf("白名单未启用时请求应打到上游：%v", chatBodies)
	}
}

// 列表过滤：白名单启用后 cn 对话列表与出图单列只含白名单项。
// global 段过滤共用同一个 allowedModel 谓词（按域语义见 TestModelsAllowedScopeIsPerRealm）。
func TestModelsAllowedFiltersList(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return hiddenCatalog() })
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	seen := modelListIDs(t, h)
	if !seen["cn:glm-5.2"] {
		t.Errorf("白名单内模型必须出现在 /v1/models：%v", seen)
	}
	for _, id := range []string{"cn:kimi-k3-1", "cn:hunyuan-image-alpha"} {
		if seen[id] {
			t.Errorf("白名单外模型不该出现在 /v1/models：%s（全部：%v）", id, seen)
		}
	}
}

// 按域语义（镜像 hidden 的按域测试）：cn: 前缀只放行该域，global 列表同判据
// （modelList 的 global 分支走同一个 allowedModel("global", id)，这里在谓词层钉住）。
func TestModelsAllowedScopeIsPerRealm(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), ModelsAllowed: []string{"cn:glm-5.2"}})
	if !h.allowedModel("cn", "glm-5.2") {
		t.Error("cn:glm-5.2 应在 cn 域放行")
	}
	if h.allowedModel("global", "glm-5.2") {
		t.Error("cn: 前缀不该泄漏到 global 域")
	}
	// 裸名条目两域都放行。
	h2 := NewHandler(Config{Pool: testPoolWith(), ModelsAllowed: []string{"glm-5.2"}})
	if !h2.allowedModel("cn", "glm-5.2") || !h2.allowedModel("global", "glm-5.2") {
		t.Error("裸名应两域都放行")
	}
	// 空模型名不放行（避免「没写 model」被兜底成白名单内某个模型）。
	if h2.allowedModel("cn", "") {
		t.Error("空模型名不该判为放行")
	}
	// 大小写敏感（与 hidden 同口径：不做归一化）。
	if h2.allowedModel("cn", "GLM-5.2") {
		t.Error("白名单应大小写敏感：GLM-5.2 不在白名单")
	}
}

// 点名拒绝：白名单外模型在打上游前就 400，文案给出可操作的换模型指引。
// 裸名 / cn: / global: 三种写法同一判据（镜像 TestChatHiddenModelRejected）。
func TestModelsAllowedRejectsChatBeforeUpstream(t *testing.T) {
	resetDynamicModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		t.Error("白名单外模型的请求不该打到上游（会真花积分）")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	for _, model := range []string{"kimi-k3-1", "cn:kimi-k3-1", "global:kimi-k3-1"} {
		rec := postChat(h, model)
		if rec.Code != 400 {
			t.Errorf("model %q: code=%d, want 400（body=%s）", model, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Errorf("model %q: 应报 code=model_unavailable：%s", model, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "allowlist") {
			t.Errorf("model %q: 文案应说明白名单拒绝：%s", model, rec.Body)
		}
	}
	if calls != 0 {
		t.Errorf("白名单外模型必须零上游请求：calls=%d", calls)
	}
}

// 白名单内模型照常放行（拒绝分支不能把放行路径也堵死）。
func TestModelsAllowedPermitsListedChat(t *testing.T) {
	resetDynamicModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	if rec := postChat(h, "glm-5.2"); rec.Code != 200 {
		t.Fatalf("白名单内模型应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("放行的请求应恰好打一次上游：calls=%d", calls)
	}
}

// 出图入口点名拒绝：白名单外的出图模型不能直接生成，且不打上游（镜像 hidden）。
func TestModelsAllowedRejectsImages(t *testing.T) {
	fake := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{
		Pool:          imagePool(),
		Upstream:      fake.client(),
		ModelsAllowed: []string{"glm-5.2"},
	})
	rec := postImages(h, `{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("应报 model_unavailable：%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "allowlist") {
		t.Errorf("文案应说明白名单拒绝：%s", rec.Body)
	}
	if len(fake.paths) != 0 {
		t.Errorf("白名单外模型不该打到上游：%v", fake.paths)
	}
}

// 出图兜底（设计决策 4）：未点名模型时，默认名不在白名单 → 改选白名单内
// 第一个出图模型，真实打到上游的就是它。
func TestModelsAllowedImagesFallbackToFirstAllowed(t *testing.T) {
	resetDynamicModelsCache()
	var bodies []string
	up := newCatalogImageUpstream(t, &bodies)
	h := NewHandler(Config{
		Pool:          imagePool(),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2", "hunyuan-image-alpha"},
	})
	// 先触发一次目录解析：出图兜底候选来自旁路留存。
	if seen := modelListIDs(t, h); !seen["cn:hunyuan-image-alpha"] {
		t.Fatalf("白名单内出图模型应先出现在列表：%v", seen)
	}
	rec := postImages(h, `{"prompt":"一只猫"}`)
	if rec.Code != 200 {
		t.Fatalf("兜底命中白名单内出图模型应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 1 {
		t.Fatalf("应有且只有一次出图上游请求：%v", bodies)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &obj); err != nil {
		t.Fatalf("出图出站 body 不是 JSON: %v (%s)", err, bodies[0])
	}
	if obj["model"] != "hunyuan-image-alpha" {
		t.Errorf("兜底后出站 model = %v, want hunyuan-image-alpha（body=%s）", obj["model"], bodies[0])
	}
}

// 白名单内无出图模型：未点名时维持默认名落入拒绝（不做隐式放行），零上游。
func TestModelsAllowedImagesDefaultRejectedWhenNoImageAllowed(t *testing.T) {
	resetDynamicModelsCache()
	var bodies []string
	up := newCatalogImageUpstream(t, &bodies)
	h := NewHandler(Config{
		Pool:          imagePool(),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"}, // 只有对话模型，无出图模型
	})
	_ = modelListIDs(t, h) // 触发目录解析（旁路留存里有出图模型，但都不在白名单）
	rec := postImages(h, `{"prompt":"一只猫"}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400 allowlist 拒绝）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "allowlist") {
		t.Errorf("文案应说明白名单拒绝：%s", rec.Body)
	}
	if len(bodies) != 0 {
		t.Errorf("白名单内无出图模型时不该打到上游：%v", bodies)
	}
}

// 别名不能绕过白名单（与 hidden 的「别名不能绕过黑名单」同一条不变量）：
// 白名单检查在别名映射之后，解析出的真实目标同样受检。
func TestModelsAllowedAliasCannotBypass(t *testing.T) {
	resetDynamicModelsCache()

	// 捕获型 fake：sent 记录最后一次打上游的出站 body（nil = 从未打到上游）。
	var sent []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sent = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	// 别名目标不在白名单 → 400，且不打上游。
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
		ModelsAliases: []string{"my-x=deepseek-v4-pro"},
	})
	rec := postChat(h, "my-x")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "allowlist") {
		t.Fatalf("别名目标不在白名单应被拒：code=%d body=%s", rec.Code, rec.Body)
	}
	if sent != nil {
		t.Errorf("被拒请求不该打到上游：%s", sent)
	}

	// 别名目标在白名单 → 放行，且出站 body 用真实名（映射照常生效）。
	h2 := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
		ModelsAliases: []string{"my-gpt=glm-5.2"},
	})
	if rec := postChat(h2, "my-gpt"); rec.Code != 200 {
		t.Fatalf("别名目标在白名单应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v (%s)", err, sent)
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（别名真实名生效）", obj["model"])
	}
}

// 白∩黑交集（列表面）：allowed=["glm-5.2","kimi-k3-1"] + hidden=["kimi-k3-1"]
// → 列表只有 glm-5.2。
func TestModelsAllowedIntersectHiddenList(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return hiddenCatalog() })
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2", "kimi-k3-1"},
		ModelsHidden:  []string{"kimi-k3-1"},
	})
	seen := modelListIDs(t, h)
	if !seen["cn:glm-5.2"] {
		t.Errorf("白名单内且未隐藏的模型必须在列表：%v", seen)
	}
	for id := range seen {
		if strings.Contains(id, "kimi") || strings.Contains(id, "hunyuan") {
			t.Errorf("被隐藏/不在白名单的模型不该在列表：%s（全部：%v）", id, seen)
		}
	}
}

// 白∩黑交集（点名面）：kimi 在白名单内 → 白名单分支放行，落到 hidden 分支
// 被拒（handler 顺序：白名单检查在前，黑名单在后）。glm 两关都过 → 放行。
func TestModelsAllowedIntersectHiddenNamed(t *testing.T) {
	resetDynamicModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2", "kimi-k3-1"},
		ModelsHidden:  []string{"kimi-k3-1"},
	})
	rec := postChat(h, "kimi-k3-1")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("交集内被隐藏的模型点名应被拒：code=%d body=%s", rec.Code, rec.Body)
	}
	// kimi 在白名单内 → 放行发生在白名单检查；拒绝来自后置的 hidden 分支，
	// 文案按黑名单口径（黑名单优先的归因，实现与断言一致）。
	if !strings.Contains(rec.Body.String(), "hidden") {
		t.Errorf("白名单内+黑名单内应走 hidden 分支文案：%s", rec.Body)
	}
	if calls != 0 {
		t.Errorf("被拒请求不该打到上游：calls=%d", calls)
	}
	if rec := postChat(h, "glm-5.2"); rec.Code != 200 {
		t.Fatalf("白名单内且未隐藏应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("放行的请求应打到上游：calls=%d", calls)
	}
}

// 热生效：面板保存后替换 livecfg 快照，收紧/清空都即时生效，无需重启
// （镜像 TestModelsHiddenHotReloadViaLive；同时验证快照优先于启动配置）。
func TestModelsAllowedHotReloadViaLive(t *testing.T) {
	resetDynamicModelsCache()
	var chatBodies []string
	up := newCatalogChatUpstream(t, &chatBodies)
	live := livecfg.New(livecfg.Snapshot{}) // 空快照 = 白名单不启用
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: nil, // 启动配置为空，收紧完全靠运行期快照
		Live:          live,
	})
	// 收紧前：全放行。
	if rec := postChat(h, "kimi-k3-1"); rec.Code != 200 {
		t.Fatalf("未收紧时点名应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if seen := modelListIDs(t, h); !seen["cn:kimi-k3-1"] {
		t.Fatalf("未收紧时列表应全量：%v", seen)
	}
	// 收紧：面板把白名单从空保存为 ["glm-5.2"]。
	allowed, enabled := livecfg.NormalizeAllowedModels([]string{"glm-5.2"})
	live.Store(livecfg.Snapshot{AllowedModels: allowed, AllowlistEnabled: enabled})
	if !h.allowedModel("cn", "glm-5.2") || h.allowedModel("cn", "kimi-k3-1") {
		t.Fatal("替换快照后白名单应立刻生效（面板保存即热生效）")
	}
	seen := modelListIDs(t, h) // 目录已缓存，过滤纯本地
	if !seen["cn:glm-5.2"] || seen["cn:kimi-k3-1"] {
		t.Fatalf("收紧后列表只应含白名单项：%v", seen)
	}
	if rec := postChat(h, "kimi-k3-1"); rec.Code != 400 {
		t.Fatalf("收紧后点名应被拒：code=%d body=%s", rec.Code, rec.Body)
	}
	// 清空：面板把白名单保存回空 = 关闭，下一次响应即时回到全放行。
	live.Store(livecfg.Snapshot{})
	if rec := postChat(h, "kimi-k3-1"); rec.Code != 200 {
		t.Fatalf("清空白名单后应立刻恢复放行：code=%d body=%s", rec.Code, rec.Body)
	}
	if seen := modelListIDs(t, h); !seen["cn:kimi-k3-1"] {
		t.Fatalf("清空后列表应恢复全量：%v", seen)
	}
}

// capacity 信息面回归：白名单启用且模型不在白名单 → model_available_accounts
// 归零；白名单内照常报容量。镜像 TestCapacityHonorsRealmAliasesAndHiddenModels。
func TestCapacityHonorsAllowlist(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "cn-account"},
		&auth.Auth{UID: "global-account", Domain: "www.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, APIKey: "secret", ModelsAllowed: []string{"glm-5.2"}})
	for _, tc := range []struct {
		model string
		want  int
	}{
		{"cn:glm-5.2", 1},       // 白名单内（cn 域容量为正控：证明归零来自白名单而非无号）
		{"global:glm-5.2", 1},   // 裸名条目两域放行，global 域有 global-account
		{"glm-5.2", 1},          // 裸名解析到 cn 域，同判据
		{"cn:kimi-k3-1", 0},     // 不在白名单 → 容量归零（信息面不成为绕过口）
		{"global:kimi-k3-1", 0}, // 同上，global 域
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/capacity?model="+tc.model, nil)
		req.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(rec, req)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || got["model_available_accounts"] != float64(tc.want) {
			t.Fatalf("model=%s status=%d body=%s want model_available_accounts=%d",
				tc.model, rec.Code, rec.Body, tc.want)
		}
	}
}
