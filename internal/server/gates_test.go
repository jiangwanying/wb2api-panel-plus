package server

import (
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 闸门只允许产生「假阳性」（多解析一次）；结果必须与无条件解析逐项相等。
func TestToolNamesGateEquivalence(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","tools":[{"type":"function","function":{"name":"read_file"}}]}`,
		`{"model":"m","tools":[{"name":"flat_name"}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"name":"bash"}}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","function_call":{"name":"legacy"}}]}`,
		`{"model":"m","functions":[{"name":"legacy_fn"}]}`,
		`{"model":"m","tool_choice":"auto","messages":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"mention tool in text"}]}`,
		`not json`,
		``,
	}
	for _, body := range bodies {
		want := upstream.ToolNameAllowlist([]byte(body))
		got := toolNamesIfPresent([]byte(body))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("gate mismatch for %s: got=%v want=%v", body, got, want)
		}
	}
}

func TestImagePartGateEquivalence(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/1.png"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://x/1.png"}]}]}`,
		// 非 content part 里出现 image_url 键：闸门假阳性，结果仍须一致（false）。
		`{"model":"m","metadata":{"image_url":"https://x/1.png"},"messages":[]}`,
		`not json`,
		``,
	}
	for _, body := range bodies {
		want := hasImagePart([]byte(body))
		got := imagePartIfPresent([]byte(body))
		if got != want {
			t.Errorf("gate mismatch for %s: got=%v want=%v", body, got, want)
		}
	}
}
