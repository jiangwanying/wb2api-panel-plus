// imagecharge_test.go 出图实扣分档观测（ImageChargeAgg）的回归。
package upstream

import "testing"

// TestRecordImageChargeBuckets 分档聚合：同一档累计，不同档分桶，样本多的排前。
//
// 为什么必须分档：global 按输出图像 token 计费（实测 1024² low = 0.55 / 3997 tokens，
// high = 4.84 / 35333 tokens），CN 每张固定 5.71——把两档混成一个均值，面板上就再也
// 对不上任何一档真实价格（issue：面板显示 0.55/张，用户选 high 后均值漂移）。
func TestRecordImageChargeBuckets(t *testing.T) {
	c := New()
	c.RecordImageCharge("global", "gpt-image-2.5-sunburst", "1024x1024", "low", 0.55, 3997, 0)
	c.RecordImageCharge("global", "gpt-image-2.5-sunburst", "1024x1024", "low", 0.55, 3997, 0)
	c.RecordImageCharge("global", "gpt-image-2.5-sunburst", "1024x1024", "high", 4.84, 35333, 0)
	// 另一个模型 / 另一个域不串档。
	c.RecordImageCharge("cn", "hunyuan-image-alpha", "512x512", "", 5.71, 0, 0)
	// credit<=0（上游省略 usage）不入账，也不改已有样本。
	c.RecordImageCharge("global", "gpt-image-2.5-sunburst", "1024x1024", "low", 0, 0, 0)

	got := c.ImageCharges("global", "gpt-image-2.5-sunburst")
	if len(got) != 2 {
		t.Fatalf("分档数=%d want 2: %+v", len(got), got)
	}
	// 样本多的在前：low 2 次、high 1 次。
	if got[0].Quality != "low" || got[0].Samples != 2 {
		t.Fatalf("第一档应是 low×2，得到 %+v", got[0])
	}
	if got[0].Avg != 0.55 || got[0].LastTokens != 3997 {
		t.Errorf("low 档 avg=%v tokens=%d want 0.55 / 3997", got[0].Avg, got[0].LastTokens)
	}
	if got[1].Quality != "high" || got[1].Avg != 4.84 || got[1].LastTokens != 35333 {
		t.Errorf("high 档 %+v want avg 4.84 tokens 35333", got[1])
	}
	// 跨域不串：CN 只有自己那一档，且不带画质维（CN 回执没有 quality）。
	if cn := c.ImageCharges("cn", "hunyuan-image-alpha"); len(cn) != 1 || cn[0].Avg != 5.71 || cn[0].Quality != "" {
		t.Errorf("CN 档 %+v want 单档 5.71 且无画质", cn)
	}
	// 没出过图的模型返回空。
	if none := c.ImageCharges("global", "no-such-model"); len(none) != 0 {
		t.Errorf("未出图模型应返回空，得到 %+v", none)
	}
}

// TestImageChargeKeyDistinguishesMissingFields 缺省尺寸/画质要落到同一个 "-" 桶，
// 不能因为空串互相覆盖或分裂成多个桶。
func TestImageChargeKeyDistinguishesMissingFields(t *testing.T) {
	c := New()
	c.RecordImageCharge("cn", "hunyuan-image-alpha", "", "", 5.71, 0, 0)
	c.RecordImageCharge("cn", "hunyuan-image-alpha", "", "", 5.71, 0, 0)
	got := c.ImageCharges("cn", "hunyuan-image-alpha")
	if len(got) != 1 || got[0].Samples != 2 {
		t.Fatalf("空尺寸/画质应落同一桶，得到 %+v", got)
	}
	if got[0].Last != 5.71 || got[0].Avg != 5.71 {
		t.Errorf("avg/last=%v/%v want 5.71", got[0].Avg, got[0].Last)
	}
}
