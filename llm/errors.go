package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Reason 一次失败的原因分类。
//
// 对齐 hermes-agent 的做法：分类结果不只是"能不能重试"，还决定上层要不要
// 换一个凭证、要不要压缩上下文后重试、以及是否属于必须让用户看到的配置问题
// （见 conversation_loop 的 should_rotate_credential / should_compress）。
type Reason string

const (
	ReasonNetwork         Reason = "network"          // 连接失败 / 上游提前断开
	ReasonTimeout         Reason = "timeout"          // 超时（含 408 / 504）
	ReasonCanceled        Reason = "canceled"         // 用户中断 / ctx 取消
	ReasonRateLimit       Reason = "rate_limit"       // 429
	ReasonServerError     Reason = "server_error"     // 5xx
	ReasonOverloaded      Reason = "overloaded"       // 529 上游过载
	ReasonAuth            Reason = "auth"             // 401 / 403
	ReasonBilling         Reason = "billing"          // 402 欠费 / 额度耗尽
	ReasonContextOverflow Reason = "context_overflow" // 上下文超限
	ReasonPayloadTooLarge Reason = "payload_too_large"
	ReasonModelNotFound   Reason = "model_not_found"
	ReasonContentPolicy   Reason = "content_policy" // 内容风控
	ReasonFormatError     Reason = "format_error"   // 参数 / 请求构造问题
	ReasonEmptyResponse   Reason = "empty_response" // 200 但没有 choices
	ReasonUnknown         Reason = "unknown"
)

// Retryable 该原因是否值得原样重试。
func (r Reason) Retryable() bool {
	switch r {
	case ReasonNetwork, ReasonTimeout, ReasonRateLimit, ReasonServerError,
		ReasonOverloaded, ReasonEmptyResponse:
		return true
	default:
		return false
	}
}

// ShouldCompress 该原因是否应该压缩上下文后重试。
func (r Reason) ShouldCompress() bool { return r == ReasonContextOverflow }

// ShouldRotateCredential 该原因是否可能是凭证问题（换 key 再试一次）。
func (r Reason) ShouldRotateCredential() bool {
	return r == ReasonAuth || r == ReasonBilling || r == ReasonRateLimit
}

// Error 一次 LLM 调用失败的分类结果。
//
// Retryable 由 Reason 判定，供上层决定是否重试；渲染成下游可见的文本时
// 统一走 Error()。
type Error struct {
	StatusCode int    // HTTP 状态码；网络层错误为 0
	Code       string // 上游错误码（若响应体里带）
	Message    string // 面向用户的说明（已从上游错误体提取）
	RawBody    string // 原始响应体，便于排查（可能很长，仅日志/调试用）
	Reason     Reason
	// RetryAfter 上游通过 Retry-After 头给出的最小等待时长（0 = 未提供）。
	RetryAfter time.Duration
	Retryable  bool
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("API 请求失败: %s", e.Message)
	}

	return fmt.Sprintf("API 错误 (%d): %s", e.StatusCode, e.Message)
}

// IsRetryable 判断错误是否值得重试。
//
// 429（限流）、5xx（上游故障）、网络层错误与空响应可重试；4xx 中的鉴权/
// 参数类错误重试无意义，属于配置问题，必须让用户看到。
func IsRetryable(err error) bool {
	var llmErr *Error
	if errors.As(err, &llmErr) {
		return llmErr.Retryable
	}

	return false
}

// ReasonOf 返回错误的原因分类（非 llm.Error 时为 ReasonUnknown）。
func ReasonOf(err error) Reason {
	var llmErr *Error
	if errors.As(err, &llmErr) {
		return llmErr.Reason
	}

	return ReasonUnknown
}

// RetryAfterOf 返回上游要求的等待时长（未提供时为 0）。
func RetryAfterOf(err error) time.Duration {
	var llmErr *Error
	if errors.As(err, &llmErr) {
		return llmErr.RetryAfter
	}

	return 0
}

// StatusCodeOf 返回错误对应的 HTTP 状态码（非 llm.Error 时返回 0）。
func StatusCodeOf(err error) int {
	var llmErr *Error
	if errors.As(err, &llmErr) {
		return llmErr.StatusCode
	}

	return 0
}

// newHTTPError 由状态码、响应头与响应体构造分类错误。
func newHTTPError(status int, header http.Header, body []byte) *Error {
	e := &Error{
		StatusCode: status,
		RawBody:    string(body),
	}

	// 优先解析 OpenAI 形状的错误体：{"error":{"message":...,"code":...}}
	e.Message, e.Code = parseErrorBody(body, e.RawBody)
	// 解析出的 message 同样要截断：网关常把上游/后端的整个 HTML 错误页
	// 塞进 JSON 的 error.message 里（实测 llmgate 透传 hr-admin 的 404 页），
	// 不截断就会把一大坨 HTML 渲染进对话气泡
	e.Message = truncate(e.Message, 500)

	e.Reason = classifyReason(status, e.Code, e.Message)
	e.Retryable = e.Reason.Retryable()
	if header != nil {
		e.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}

	return e
}

// classifyReason 按状态码优先、错误文本兜底判定失败原因。
//
// 顺序很重要：400 既可能是参数写错，也可能是上下文超限/内容风控，只有看
// 错误文本才能区分，所以状态码命中后仍要继续看关键词。
func classifyReason(status int, code, message string) Reason {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ReasonAuth
	case http.StatusPaymentRequired:
		return ReasonBilling
	case http.StatusNotFound:
		return ReasonModelNotFound
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ReasonTimeout
	case http.StatusRequestEntityTooLarge:
		return ReasonPayloadTooLarge
	case http.StatusTooManyRequests:
		return ReasonRateLimit
	case 529: // 非标准：部分上游用 529 表示过载
		return ReasonOverloaded
	}

	if status >= 500 {
		return ReasonServerError
	}

	lower := strings.ToLower(code + " " + message)
	switch {
	case hasAnyKeyword(lower,
		"context length", "context_length_exceeded", "maximum context",
		"too many tokens", "prompt is too long", "exceeds the limit",
		"reduce the length", "token limit"):
		return ReasonContextOverflow
	case hasAnyKeyword(lower, "payload too large", "request entity too large"):
		return ReasonPayloadTooLarge
	case hasAnyKeyword(lower, "content policy", "content_filter", "content filter",
		"flagged", "safety system"):
		return ReasonContentPolicy
	case hasAnyKeyword(lower, "model not found", "model_not_found", "no such model",
		"does not exist"):
		return ReasonModelNotFound
	case hasAnyKeyword(lower, "invalid api key", "invalid_api_key", "unauthorized",
		"authentication", "token expired"):
		return ReasonAuth
	case hasAnyKeyword(lower, "rate limit", "rate_limit", "too many requests",
		"throttled", "resource_exhausted"):
		return ReasonRateLimit
	case hasAnyKeyword(lower, "invalid request", "invalid_request", "malformed",
		"format_error", "bad request"):
		return ReasonFormatError
	}

	return ReasonUnknown
}

// hasAnyKeyword 判断文本里是否出现任一关键词。
func hasAnyKeyword(text string, keywords ...string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}

	return false
}

// parseRetryAfter 解析 Retry-After 头：秒数或 HTTP-date，两种都支持。
//
// 上限 10 分钟：上游偶尔给出离谱的值（例如 3600），不能让一个回合卡死。
func parseRetryAfter(value string) time.Duration {
	const maxWait = 10 * time.Minute

	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0
		}

		d := time.Duration(secs) * time.Second
		if d > maxWait {
			return maxWait
		}

		return d
	}

	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d <= 0 {
			return 0
		}
		if d > maxWait {
			return maxWait
		}

		return d
	}

	return 0
}

// parseErrorBody 从响应体里提取可读的错误说明与错误码。
//
// 上游错误体形状并不统一：多数是 {"error":{"message":..}}，也有直接
// {"message":..}（部分国产厂商）或纯文本（异常中转）。逐层降级提取。
func parseErrorBody(body []byte, fallback string) (message, code string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "上游返回空响应体", ""
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message, stringifyCode(envelope.Error.Code)
		}
		if envelope.Message != "" {
			return envelope.Message, stringifyCode(envelope.Code)
		}
	}

	// 非 JSON 或形状不认识：截断后原样透出，避免把整个 HTML 错误页塞进 UI。
	return truncate(trimmed, 500), ""
}

// stringifyCode 把 error.code 统一成字符串（上游有时给数字、有时给字符串）。
func stringifyCode(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case float64:
		// JSON 数字统一解成 float64；整数错误码去掉小数尾巴
		if c == float64(int64(c)) {
			return fmt.Sprintf("%d", int64(c))
		}

		return fmt.Sprintf("%v", c)
	default:
		return fmt.Sprintf("%v", c)
	}
}

// truncate 按 rune 截断字符串，避免把超长响应体塞进错误信息。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}

	return string(runes[:limit]) + "…"
}
