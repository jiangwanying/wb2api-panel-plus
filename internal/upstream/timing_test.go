package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ChatTiming 只做观测：nil 出参不得影响行为；非 nil 时 prep/do 都要有测量值。
func TestChatTimingObservation(t *testing.T) {
	var gotTiming ChatTiming
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	body := []byte(`{"model":"glm-5.2","messages":[]}`)

	rc, _, _, err := c.ChatStreamContext(nil, a, body, "", ChatMeta{CacheKeyID: "s"}, &gotTiming)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	rc.Close()
	if gotTiming.Prep <= 0 {
		t.Errorf("Prep 未测量：%+v", gotTiming)
	}
	if gotTiming.Do <= 0 {
		t.Errorf("Do 未测量：%+v", gotTiming)
	}
	if gotTiming.Total() != gotTiming.Prep+gotTiming.Do {
		t.Errorf("Total 应等于两段之和：%+v", gotTiming)
	}

	// nil 出参：零开销路径，不得 panic 也不得改变返回。
	rc2, status, _, err := c.ChatStreamContext(nil, a, body, "", ChatMeta{}, nil)
	if err != nil || status != 200 {
		t.Fatalf("nil timing 路径：status=%d err=%v", status, err)
	}
	rc2.Close()

	// 零值语义：未测量时 Total 为 0（不是负值/NaN）。
	var zero ChatTiming
	if zero.Total() != 0 || zero.Prep != 0 {
		t.Errorf("零值语义被破坏：%+v", zero)
	}
}
