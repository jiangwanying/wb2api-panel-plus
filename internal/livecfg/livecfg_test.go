package livecfg

import "testing"

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - 非 cn/global 前缀（如 "xx:yy"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 非法前缀视为裸名整体
	got = NormalizeHiddenModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}
}

// TestNormalizeAllowedModels 白名单归一化（issue #68）：条目语法与
// NormalizeHiddenModels 完全一致，另返回 enabled 标志——
//   - nil/空/全空白 → nil,false（白名单不启用 = 默认放行全部）
//   - 裸名 → 两域都允许（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只允许该域；非合法前缀视为裸名整体
//   - 前后空白裁掉；条目大小写敏感（与 hidden 同口径）
func TestNormalizeAllowedModels(t *testing.T) {
	if got, enabled := NormalizeAllowedModels(nil); got != nil || enabled {
		t.Errorf("nil → %v,%v, want nil,false", got, enabled)
	}
	if got, enabled := NormalizeAllowedModels([]string{}); got != nil || enabled {
		t.Errorf("empty → %v,%v, want nil,false", got, enabled)
	}
	if got, enabled := NormalizeAllowedModels([]string{"  ", ""}); got != nil || enabled {
		t.Errorf("all blank → %v,%v, want nil,false (全空条目必须回落到不启用)", got, enabled)
	}

	got, enabled := NormalizeAllowedModels([]string{"glm-5.2"})
	if !enabled {
		t.Fatal("非空有效条目应启用白名单")
	}
	for _, want := range []string{"cn:glm-5.2", "global:glm-5.2"} {
		if _, ok := got[want]; !ok {
			t.Errorf("裸名：缺 %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("裸名：得 %d 条 %v, want 2", len(got), got)
	}

	got, _ = NormalizeAllowedModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: 前缀：缺 %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: 前缀：%q 不得泄漏到 global 域", "global:pro-model")
	}

	// 前后空白被裁掉
	got, _ = NormalizeAllowedModels([]string{"  deepseek-v4-pro  "})
	if _, ok := got["cn:deepseek-v4-pro"]; !ok {
		t.Errorf("trim：缺 %q in %v", "cn:deepseek-v4-pro", got)
	}

	// 大小写敏感（与 hidden 同口径：不做归一化）
	got, _ = NormalizeAllowedModels([]string{"glm-5.2"})
	if _, ok := got["cn:GLM-5.2"]; ok {
		t.Error("条目应大小写敏感，GLM-5.2 不该被登记")
	}

	// 非法前缀视为裸名整体（与 NormalizeHiddenModels 对称）
	got, _ = NormalizeAllowedModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("非法前缀：缺 %q in %v", want, got)
		}
	}
}

// TestNormalizeModelAliases 模型映射归一化（issue #30）：
//   - nil/空/无 "="/空键/空值/自我映射 → 丢弃
//   - 键小写化（查找端同口径，大小写不敏感）
//   - 裸名键 → 两域都生效；"cn:"/"global:" 前缀键 → 只在该域生效
//   - 值原样保留（可带 realm 前缀跨域指向）
func TestNormalizeModelAliases(t *testing.T) {
	if got := NormalizeModelAliases(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeModelAliases([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeModelAliases([]string{"no-equals", "  ", "", "=x", "y=", "self=self"}); got != nil {
		t.Errorf("all invalid → %v, want nil", got)
	}

	// 裸名键：两域都生效，键小写化
	got := NormalizeModelAliases([]string{"My-GPT=glm-5.2"})
	for _, k := range []string{"cn:my-gpt", "global:my-gpt"} {
		if got[k] != "glm-5.2" {
			t.Errorf("bare key: %q = %q, want glm-5.2 (%v)", k, got[k], got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare key: got %d entries %v, want 2", len(got), got)
	}

	// 域前缀键：只在该域生效；值带前缀原样保留（跨域指向）
	got = NormalizeModelAliases([]string{"cn:opus=global:gpt-5.4"})
	if got["cn:opus"] != "global:gpt-5.4" {
		t.Errorf("scoped key: cn:opus = %q, want global:gpt-5.4", got["cn:opus"])
	}
	if _, ok := got["global:opus"]; ok {
		t.Error("scoped key: must not register global realm")
	}

	// 键值两侧空白裁掉；值只在 "=" 第一次出现处切分
	got = NormalizeModelAliases([]string{"  alias1 =glm-5.2  "})
	if got["cn:alias1"] != "glm-5.2" {
		t.Errorf("trim: cn:alias1 = %q, want glm-5.2 (%v)", got["cn:alias1"], got)
	}
}

// TestModelAliasesHotReload 快照整体替换后立即生效（面板保存语义）。
func TestModelAliasesHotReload(t *testing.T) {
	h := New(Snapshot{ModelAliases: NormalizeModelAliases([]string{"a=glm-5.2"})})
	if _, ok := h.Load().ModelAliases["cn:a"]; !ok {
		t.Fatal("初始快照应含 cn:a")
	}
	h.Store(Snapshot{ModelAliases: NormalizeModelAliases([]string{"b=deepseek-v3"})})
	if _, ok := h.Load().ModelAliases["cn:a"]; ok {
		t.Error("替换快照后旧映射必须消失（整体覆盖语义）")
	}
	if _, ok := h.Load().ModelAliases["cn:b"]; !ok {
		t.Error("替换快照后新映射应生效")
	}
}
