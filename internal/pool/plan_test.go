package pool

import "testing"

func TestClassifyPlan(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		want PlanKind
	}{
		{"无包信息不猜", nil, PlanUnknown},
		{"空列表不猜", []string{}, PlanUnknown},
		{"免费版", []string{"Free Plan"}, PlanFree},
		{"免费版中文", []string{"免费版"}, PlanFree},
		{"体验版", []string{"CodeBuddy 体验版"}, PlanTrial},
		{"标准版", []string{"标准版"}, PlanStandard},
		{"订阅优先于活动包", []string{"国内运营裂变包", "标准版"}, PlanStandard},
		{"全活动包", []string{"国内运营裂变包", "拉新权益包"}, PlanActivity},
		{"混合来源不敢判", []string{"国内运营裂变包", "某未知来源包"}, PlanUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ev := ClassifyPlan(c.pkgs)
			if got != c.want {
				t.Errorf("ClassifyPlan(%v)=%s（证据 %q）want %s", c.pkgs, got, ev, c.want)
			}
			if got != PlanUnknown && ev == "" {
				t.Errorf("非 unknown 必须给出命中证据，got %s/%q", got, ev)
			}
		})
	}
}
