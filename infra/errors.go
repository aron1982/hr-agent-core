package infra

import (
	"errors"
	"net"
)

// SSLError SSL/TLS 证书配置错误
type SSLError struct {
	Message string
}

// Error 实现 error 接口
func (e *SSLError) Error() string {
	return e.Message
}

// AgentError 通用 Agent 错误类型，支持错误码和错误链
type AgentError struct {
	Code    string // 错误码
	Message string // 错误消息
	Cause   error  // 底层错误
}

// Error 实现 error 接口
func (e *AgentError) Error() string {
	return e.Message
}

// Unwrap 返回底层错误，支持 errors.Is / errors.As
func (e *AgentError) Unwrap() error {
	return e.Cause
}

// NewAgentError 创建一个无底层错误的 AgentError
func NewAgentError(code, message string) *AgentError {
	return &AgentError{
		Code:    code,
		Message: message,
	}
}

// WrapAgentError 创建一个包装底层错误的 AgentError
func WrapAgentError(code string, cause error, message string) *AgentError {
	return &AgentError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// IsRetryable 判断错误是否可重试（网络超时等临时性错误）
func IsRetryable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// IsSSLError 判断错误链中是否包含 SSLError
func IsSSLError(err error) bool {
	var sslErr *SSLError
	return errors.As(err, &sslErr)
}
