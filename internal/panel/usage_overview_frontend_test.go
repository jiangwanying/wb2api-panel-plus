package panel

// 用量总览 KPI 的前端切片测试（仿 credit_frontend_adversarial_test.go 的 node + vm 做法）。
// 钉住「平均缓存命中率」卡：位置在「请求数」「总 token」之后、口径 = 命中/(命中+未命中)、
// 无样本时显示占位符；字符串脏值不得抛异常。只新增测试，不改业务代码。

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const usOverviewScript = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const cacheStart = src.indexOf('function cacheRateText');
const cacheEnd = src.indexOf('function fmtBytes');
const kpiStart = src.indexOf('function fmtTok(n)');
const kpiEnd = src.indexOf('/* renderUsageChart');
if ([escStart, escEnd, cacheStart, cacheEnd, kpiStart, kpiEnd].some(v => v < 0))
  throw new Error('usage overview helpers not found');
const els = {};
const $ = id => (els[id] || (els[id] =
  { innerHTML: '', textContent: '', title: '', dataset: {}, addEventListener() {}, onchange: null }));
const ctx = { Date, Number, String, Math, RegExp, isNaN, parseFloat, parseInt, JSON, $ };
vm.createContext(ctx);
// 切片外的依赖以桩代之：模块级状态、时区/备注/图表与两个缓存格式化函数。
vm.runInContext(
  'let usDim="account", usCreditDim="account", usSort="total", usageData=null;' +
  'function trangeLabel(){ return "近7天"; }' +
  'function remarkName(u, h){ return h; }' +
  'function renderUsageChart(){ }' +
  // renderUsage 末尾还会填按小时表，那个函数在切片之外（切片止于 renderUsageChart 注释）。
  'function renderUsageHours(){ }' +
  'function fmtHit(){ return "—"; }' +
  'function cacheRateCell(){ return "—"; }' +
  src.slice(escStart, escEnd) + src.slice(cacheStart, cacheEnd) + src.slice(kpiStart, kpiEnd) +
  '\nthis.renderUsage=renderUsage;', ctx);
const out = {};
const run = (key, fn) => { try { fn(); out[key] = true; } catch (e) { out[key] = String((e && e.stack) || e); } };

run('sampleOk', () => ctx.renderUsage({ totals: {
  requests: 10, errors: 1, prompt_tokens: 9000, completion_tokens: 1000, total_tokens: 10000,
  avg_latency_ms: 1200, avg_tokens_per_second: 42.5,
  credits: 1.23, credit_samples: 3,
  cache_hit_tokens: 7200, cache_miss_tokens: 800 }, buckets: 3 }));
out.sampleStats = els.usStats.innerHTML;

run('emptyOk', () => ctx.renderUsage({ totals: {} }));
out.emptyStats = els.usStats.innerHTML;

run('dirtyOk', () => ctx.renderUsage({ totals: { requests: '3', cache_hit_tokens: '7200', cache_miss_tokens: 'x' } }));
out.dirtyStats = els.usStats.innerHTML;
console.log(JSON.stringify(out));
`

// TestUsageOverviewCacheHitKPI 验证总览第三张主指标卡「平均缓存命中率」的渲染。
func TestUsageOverviewCacheHitKPI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage overview frontend test skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), "us-kpi-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(usOverviewScript); err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage overview node test failed: %v\n%s", err, raw)
	}
	var out struct {
		SampleOk    bool   `json:"sampleOk"`
		SampleStats string `json:"sampleStats"`
		EmptyOk     bool   `json:"emptyOk"`
		EmptyStats  string `json:"emptyStats"`
		DirtyOk     bool   `json:"dirtyOk"`
		DirtyStats  string `json:"dirtyStats"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 node 输出失败: %v\n%s", err, raw)
	}
	for _, c := range []struct {
		ok  bool
		msg string
	}{
		{out.SampleOk, "正常样本渲染抛异常"},
		{out.EmptyOk, "空 totals 渲染抛异常"},
		{out.DirtyOk, "字符串脏值渲染抛异常"},
	} {
		if !c.ok {
			t.Fatalf("renderUsage %s", c.msg)
		}
	}

	stats := out.SampleStats
	if !strings.Contains(stats, "平均缓存命中率") {
		t.Fatalf("总览缺少「平均缓存命中率」卡:\n%s", stats)
	}
	if !strings.Contains(stats, "90%") {
		t.Fatalf("命中率 7200/8000 应显示 90%%:\n%s", stats)
	}
	if !strings.Contains(stats, "命中 7.2k · 未命中 800") {
		t.Fatalf("副标题应回显命中/未命中 token:\n%s", stats)
	}
	// 积分卡：按窗口累计 upstream usage.credit，样本数一并回显，缺样本时不伪造 0。
	if !strings.Contains(stats, ">总积分<") || !strings.Contains(stats, "1.23") {
		t.Fatalf("总览缺少「总积分」卡或数值未按 fmtCredit 渲染:\n%s", stats)
	}
	if !strings.Contains(stats, "3 条样本") {
		t.Fatalf("积分卡副标题应回显 usage.credit 样本数:\n%s", stats)
	}
	// 位置：请求数 → 总 token → 积分 → 平均缓存命中率 → prompt（主指标同排）。
	iReq := strings.Index(stats, ">请求数<")
	iTotal := strings.Index(stats, ">总 token<")
	iCredit := strings.Index(stats, ">总积分<")
	iHit := strings.Index(stats, ">平均缓存命中率<")
	iPrompt := strings.Index(stats, ">prompt<")
	if !(iReq >= 0 && iReq < iTotal && iTotal < iCredit && iCredit < iHit && iHit < iPrompt) {
		t.Fatalf("卡片顺序应为 请求数 → 总 token → 积分 → 平均缓存命中率 → prompt:\n%s", stats)
	}

	if !strings.Contains(out.EmptyStats, "无样本") {
		t.Fatalf("无积分样本时应提示「无样本」而不是伪造 0:\n%s", out.EmptyStats)
	}
	if !strings.Contains(out.EmptyStats, "无缓存观测样本") || !strings.Contains(out.EmptyStats, ">—<") {
		t.Fatalf("无样本时应显示占位 — 与「无缓存观测样本」:\n%s", out.EmptyStats)
	}
	// 脏值：'7200'/'x' 应按 Number() 归零/取值，不产生 NaN%。
	if strings.Contains(out.DirtyStats, "NaN") {
		t.Fatalf("脏值渲染出 NaN:\n%s", out.DirtyStats)
	}
}

// TestUsageOverviewK8Grid 钉住 index.html：usStats 用 k8 栅格（八卡一行）。
func TestUsageOverviewK8Grid(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if !strings.Contains(html, `class="kpis k8" id="usStats"`) {
		t.Fatal("usStats 容器应使用 kpis k8 栅格")
	}
	if !strings.Contains(html, ".kpis.k8 { grid-template-columns: repeat(8") {
		t.Fatal("缺少 .kpis.k8 八列栅格规则")
	}
	if !strings.Contains(html, "@media (max-width: 700px) { .kpis, .kpis.k4, .kpis.k8 {") {
		t.Fatal("窄屏断点应把 k8 一并折成 2 列")
	}
}
