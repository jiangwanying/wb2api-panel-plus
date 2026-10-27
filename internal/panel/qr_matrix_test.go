package panel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSQRMatrix 授权链接二维码的编码器必须与规范逐像素一致。
//
// 为什么钉死 sha：面板 CSP 只允许 self（不能引外链 QR 服务），也不引前端依赖，
// 所以 QR 是手写实现 —— 分段、纠错、交织、掩码任一处错都只表现为「扫不出来」，
// 浏览器里没有任何断言能发现。下面的 golden 已由 python qrcode 库逐像素交叉验证
// （byte 模式 + ECC L + mask 0，v1-v6 共 30 组输入全部 diff=0）。
//
// v6 是 issue #94 新增的：workbuddy 站的授权链接 108 字节超过 v5 单块上限 106，
// 需要两块交织；CN 站 105 字节仍走 v5。两条路径都要钉。
func TestAppJSQRMatrix(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; QR matrix test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 精简 QR 编码器');
const end = src.indexOf('/* ── 开学季券码查询');
if (start < 0 || end < start) throw new Error('QR block not found');
const ctx = { TextEncoder, Math, Array, Error };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.qrMatrix = qrMatrix;', ctx);
const out = {};
for (const [k, v] of Object.entries(JSON.parse(process.argv[3]))) {
  try {
    const M = ctx.qrMatrix(v);
    out[k] = { n: M.length, rows: M.map(r => r.map(b => b ? '1' : '0').join('')).join('\n') };
  } catch (e) { out[k] = { err: String(e.message) }; }
}
fs.writeFileSync(process.argv[4], JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "qr-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	const st = "12345678-1234-1234-1234-123456789abc"
	inputs := map[string]string{
		// CN 站真实形态：105 字节 → v5（37×37，单块）
		"cn": "https://copilot.tencent.com/login?platform=CLI&state=" + st + "&version=2.176.0",
		// workbuddy 站：108 字节 → v6（41×41，两块交织）
		"wb": "https://www.workbuddy.ai/login?platform=workbuddy&state=" + st + "&version=2.176.0",
		// 容量上界 134 字节；135 必须报错（前端据此退回只显示链接）
		"over": strings.Repeat("a", 135),
	}
	in, _ := json.Marshal(inputs)
	outPath := filepath.Join(t.TempDir(), "out.json")
	if b, err := exec.Command(node, f.Name(), "app.js", string(in), outPath).CombinedOutput(); err != nil {
		t.Fatalf("node failed: %v\n%s", err, b)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var got map[string]struct {
		N    int    `json:"n"`
		Rows string `json:"rows"`
		Err  string `json:"err"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, raw)
	}

	for _, c := range []struct {
		key     string
		name    string
		modules int
		sha     string
	}{
		{"cn", "CN 站 105 字节 / v5", 37, "2ccaa4c9502d84f197a54ec1d4a174a8312e1db42b3d8a05f306c97477a0a3cd"},
		{"wb", "workbuddy 站 108 字节 / v6 两块交织", 41, "c5818c46fe1cdb1bc829a2249d3fc790f5a0f2c7b141bd196e722bbe5c6ed1ef"},
	} {
		r := got[c.key]
		if r.Err != "" {
			t.Errorf("%s：编码失败 %s", c.name, r.Err)
			continue
		}
		if r.N != c.modules {
			t.Errorf("%s：模块边长 %d，期望 %d（版本选错）", c.name, r.N, c.modules)
			continue
		}
		if h := fmt.Sprintf("%x", sha256.Sum256([]byte(r.Rows))); h != c.sha {
			t.Errorf("%s：矩阵与已验证实现不一致\n got %s\nwant %s", c.name, h, c.sha)
		}
	}
	if r := got["over"]; r.Err == "" {
		t.Errorf("135 字节超出 v6 容量应报错（前端靠它退回链接），实际编码成功 n=%d", r.N)
	}
}
