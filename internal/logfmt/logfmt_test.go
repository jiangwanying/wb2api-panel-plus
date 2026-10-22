package logfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Truncate 的契约：按字节上限截断，但切点落在多字节字符中间时让出整个字符。
// 这条契约此前只存在于注释里（上游 #166 的教训：错误 body / 奖品载荷多为中文，
// 按字节切会打出半截序列）。
func TestTruncateRuneBoundary(t *testing.T) {
	// 短于上限：原样（顺带 TrimSpace）。
	if got := Truncate("abc", 10); got != "abc" {
		t.Errorf("短串应原样：%q", got)
	}
	if got := Truncate("  abc  ", 10); got != "abc" {
		t.Errorf("应 TrimSpace：%q", got)
	}
	// 上限非正：空串（不为负索引 panic）。
	if got := Truncate("abc", 0); got != "" {
		t.Errorf("n<=0 应返回空串：%q", got)
	}
	// 中文 3 字节：上限 10 落在第 4 个字中间 → 只能让出该字，返回 9 字节。
	got := Truncate(strings.Repeat("奖", 10), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("截断结果必须仍是合法 UTF-8：%q", got)
	}
	if len(got) != 9 || utf8.RuneCountInString(got) != 3 {
		t.Errorf("应回退到 rune 边界（9 字节 3 字），实际 %d 字节 %d 字：%q",
			len(got), utf8.RuneCountInString(got), got)
	}
	// 上限恰好落在字符边界：不多让。
	if got := Truncate(strings.Repeat("奖", 4), 9); got != strings.Repeat("奖", 3) {
		t.Errorf("边界对齐时不应少让：%q", got)
	}
}
