package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 错误分类与 Retry-After 解析的单元测试。

func TestClassifyReason(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		msg    string
		want   Reason
	}{
		{"401 鉴权", http.StatusUnauthorized, "", "", ReasonAuth},
		{"402 欠费", http.StatusPaymentRequired, "", "", ReasonBilling},
		{"404 模型不存在", http.StatusNotFound, "", "", ReasonModelNotFound},
		{"413 请求体过大", http.StatusRequestEntityTooLarge, "", "", ReasonPayloadTooLarge},
		{"429 限流", http.StatusTooManyRequests, "", "", ReasonRateLimit},
		{"529 过载", 529, "", "", ReasonOverloaded},
		{"500 上游故障", http.StatusInternalServerError, "", "", ReasonServerError},
		{"400 上下文超限", http.StatusBadRequest, "context_length_exceeded", "maximum context length", ReasonContextOverflow},
		{"400 内容风控", http.StatusBadRequest, "", "content policy violation", ReasonContentPolicy},
		{"400 参数错误", http.StatusBadRequest, "", "invalid request format", ReasonFormatError},
		{"400 其它", http.StatusBadRequest, "", "something else", ReasonUnknown},
	}

	for _, c := range cases {
		if got := classifyReason(c.status, c.code, c.msg); got != c.want {
			t.Errorf("%s：期望 %s 实际 %s", c.name, c.want, got)
		}
	}
}

func TestReasonActions(t *testing.T) {
	if !ReasonRateLimit.Retryable() || !ReasonNetwork.Retryable() {
		t.Fatal("限流与网络故障应可重试")
	}
	if ReasonAuth.Retryable() || ReasonCanceled.Retryable() {
		t.Fatal("鉴权失败与用户中断不得重试")
	}
	if !ReasonContextOverflow.ShouldCompress() {
		t.Fatal("上下文超限应触发压缩")
	}
	if !ReasonAuth.ShouldRotateCredential() || !ReasonRateLimit.ShouldRotateCredential() {
		t.Fatal("鉴权失败与限流应尝试轮换凭证")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Fatalf("秒数解析失败：%v", got)
	}

	// 上游给离谱的值时被限幅到 10 分钟
	if got := parseRetryAfter("3600"); got != 10*time.Minute {
		t.Fatalf("超长等待应被限幅到 10 分钟：%v", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Fatalf("非法值应为 0：%v", got)
	}
}

func TestNewHTTPErrorSetsReasonAndRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "7")

	err := newHTTPError(http.StatusTooManyRequests, header, []byte(`{"error":{"message":"slow down"}}`))
	if err.Reason != ReasonRateLimit || !err.Retryable {
		t.Fatalf("429 应分类为可重试限流：%+v", err)
	}
	if err.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After 未解析：%v", err.RetryAfter)
	}

	authErr := newHTTPError(http.StatusUnauthorized, nil, []byte(`{"error":{"message":"bad key"}}`))
	if authErr.Reason != ReasonAuth || authErr.Retryable {
		t.Fatalf("401 应分类为不可重试的鉴权失败：%+v", authErr)
	}
}

// TestNewHTTPErrorTruncatesMessage 错误 message 要截断：网关常把上游/后端的
// 整个 HTML 错误页透传进 JSON 的 error.message（实测 llmgate 转发 hr-admin 的
// 404 页），不截断会把一大坨 HTML 渲染进对话气泡。
func TestNewHTTPErrorTruncatesMessage(t *testing.T) {
	long := strings.Repeat("A", 5000)
	body := []byte(`{"error":{"message":"hr-admin 返回 404: <!DOCTYPE html>` + long + `"}}`)

	err := newHTTPError(http.StatusServiceUnavailable, nil, body)
	// truncate 在截断处补一个省略号，故上限是 limit+1
	if got := len([]rune(err.Message)); got > 501 {
		t.Fatalf("错误 message 应被截断到 500 字符（+省略号），实际 %d", got)
	}
	if !strings.HasSuffix(err.Message, "…") {
		t.Fatalf("截断后应以省略号结尾，实际 %q", err.Message[len(err.Message)-10:])
	}
	if err.Reason != ReasonServerError {
		t.Fatalf("503 应分类为上游故障：%+v", err)
	}
}

func TestNetworkErrorClassifies(t *testing.T) {
	if err := networkError(context.Canceled); err.Retryable || err.Reason != ReasonCanceled {
		t.Fatalf("用户中断不得重试：%+v", err)
	}
	if err := networkError(context.DeadlineExceeded); !err.Retryable || err.Reason != ReasonTimeout {
		t.Fatalf("超时应可重试：%+v", err)
	}
}
