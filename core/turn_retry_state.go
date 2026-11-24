package core

import "fmt"

// TurnRetryState 回合重试状态管理器
type TurnRetryState struct {
	maxRetries   int   // 最大重试次数
	currentRetry int   // 当前重试计数
	lastError    error // 最后一次错误
}

// NewTurnRetryState 创建重试状态管理器
func NewTurnRetryState(maxRetries int) *TurnRetryState {
	if maxRetries <= 0 {
		maxRetries = 3 // 默认最大重试 3 次
	}
	return &TurnRetryState{
		maxRetries: maxRetries,
	}
}

// CanRetry 检查是否还能重试
func (s *TurnRetryState) CanRetry() bool {
	return s.currentRetry < s.maxRetries
}

// RecordError 记录一次错误并递增重试计数
func (s *TurnRetryState) RecordError(err error) {
	s.currentRetry++
	s.lastError = err
}

// RetryCount 返回当前已重试次数
func (s *TurnRetryState) RetryCount() int {
	return s.currentRetry
}

// LastError 返回最后一次记录的错误
func (s *TurnRetryState) LastError() error {
	return s.lastError
}

// Reset 重置重试状态
func (s *TurnRetryState) Reset() {
	s.currentRetry = 0
	s.lastError = nil
}

// String 返回状态摘要
func (s *TurnRetryState) String() string {
	if s.lastError == nil {
		return fmt.Sprintf("retry %d/%d, no error", s.currentRetry, s.maxRetries)
	}
	return fmt.Sprintf("retry %d/%d, last error: %v", s.currentRetry, s.maxRetries, s.lastError)
}
