// modelblocked_test.go 11102/11158/14401 确定性模型拒绝的判据回归。
//
// 这两条 code 都会让 handler 写 (账号, 模型) 负缓存（BlockModelBackoff）：
//   - 11102「该后端无此模型」（含图像域 14401 route config not found）→ 该模型在本域
//     根本不存在，换号也没用；
//   - 11158「该账号类型无权使用该模型」→ 同一模型在个人 / 企业 / 静态 API Key 上权限
//     不同，换有权限的号仍可能成功。
//
// 判据错误的两个方向代价不对称：漏判（11158 落 ErrClient）→ 该号留在池内被反复选中、
// 每轮都撞同一堵墙且不避让；误判（把限流/普通 400 当模型拒绝）→ 一个健康模型被冷却
// 6h 起跳。故这里既钉命中，也钉**不该命中**的形态。
package upstream

import (
	"net/http"
	"testing"
)

func TestIsModelBlockedCodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "11102_数字code",
			body: `{"code":11102,"msg":"model [glm-5.3] service info not found","requestId":"r1"}`,
			want: true,
		},
		{
			name: "11102_字符串code",
			body: `{"code":"11102","msg":"model service info not found"}`,
			want: true,
		},
		{
			name: "11102_error子对象",
			body: `{"error":{"code":11102,"message":"service info not found"}}`,
			want: true,
		},
		{
			name: "11158_账号类型无权限",
			body: `{"code":11158,"msg":"model [claude-opus-4-1] is not allowed for user type","requestId":"r1"}`,
			want: true,
		},
		{
			name: "11158_仅文案命中",
			body: `{"code":0,"msg":"This model is not allowed for user type: free"}`,
			want: true,
		},
		{
			name: "14401_图像域同族答复",
			body: `{"code":14401,"msg":"route config not found"}`,
			want: true,
		},
		{
			// 11102 出现在 requestId 等无关字段里，不得据此避让一个能用的模型。
			name: "code撞在requestId上不命中",
			body: `{"code":11101,"msg":"bad params","requestId":"req-11102-abc"}`,
			want: false,
		},
		{
			// 11140 request illegal 是账号级风控（ErrAccountFault），不是模型级拒绝：
			// 换模型也不解决，误判成模型拒绝会把该号所有模型逐个冷却一遍。
			name: "11140_request_illegal不命中",
			body: `{"code":11140,"msg":"request illegal"}`,
			want: false,
		},
		{
			name: "6004模型级限流不命中",
			body: `{"code":6004,"msg":"The model provider is rate-limiting requests"}`,
			want: false,
		},
		{
			name: "普通400不命中",
			body: `{"code":11101,"msg":"Unmarshal chat params failed"}`,
			want: false,
		},
		{
			name: "空body",
			body: "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsModelBlocked(http.StatusBadRequest, c.body); got != c.want {
				t.Fatalf("IsModelBlocked(%q)=%v want %v", c.body, got, c.want)
			}
		})
	}
}

// 429 带 11158 仍属限流语义（上游在限流时也可能回带模型的 code）：只看 400/404，
// 否则一个本来能用的模型会因为一次限流被负缓存 6 小时。
func TestIsModelBlockedIgnoresNon400Statuses(t *testing.T) {
	body := `{"code":11158,"msg":"model is not allowed for user type"}`
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusOK, http.StatusInternalServerError} {
		if IsModelBlocked(status, body) {
			t.Fatalf("status=%d 不应判为模型拒绝", status)
		}
	}
	// 400/404 才判。
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		if !IsModelBlocked(status, body) {
			t.Fatalf("status=%d 应判为模型拒绝", status)
		}
	}
}

// ModelBlockReasonFor 决定负缓存条目的 reason 前缀，pool 侧按此前缀分域
// （BlockModelClear 清 11102/11158、台账 kind 区分限流与不可用）。11158 必须与
// 11102 分列，否则运维在台账上看不出「模型不存在」与「这个号没权限」。
func TestModelBlockReasonFor(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"11158数字", `{"code":11158,"msg":"model is not allowed for user type"}`, ModelForbiddenReason},
		{"11158字符串", `{"code":"11158","msg":"x"}`, ModelForbiddenReason},
		{"11158仅文案", `{"code":0,"msg":"This model is not allowed for user type"}`, ModelForbiddenReason},
		{"11102数字", `{"code":11102,"msg":"service info not found"}`, ModelBlockReason},
		{"14401图像域", `{"code":14401,"msg":"route config not found"}`, ModelBlockReason},
		{"非JSON", `not json at all`, ModelBlockReason},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelBlockReasonFor(c.body); got != c.want {
				t.Fatalf("ModelBlockReasonFor(%q)=%q want %q", c.body, got, c.want)
			}
		})
	}
}

// reason 前缀必须能被 pool 的正则表识别（"11102"/"11158"）——两处字面量分属不同包，
// 漂移会让 BlockModelClear 静默失效（该模型重新跑通也永远不清缓存）。
func TestModelBlockReasonPrefixesAreRecognizable(t *testing.T) {
	for _, reason := range []string{ModelBlockReason, ModelForbiddenReason} {
		if len(reason) < 5 || (reason[:5] != "11102" && reason[:5] != "11158") {
			t.Fatalf("reason %q 不以 11102/11158 开头，pool.BlockModelClear 将无法识别", reason)
		}
	}
}
