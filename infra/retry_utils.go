package infra

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// RetryConfig 重试配置
type RetryConfig struct {
	MaxRetries int           // 最大重试次数（默认 3）
	BaseDelay  time.Duration // 基础延迟（默认 1 秒）
	MaxDelay   time.Duration // 最大延迟上限（默认 30 秒）
	Jitter     float64       // 抖动系数 0.0-1.0（默认 0.1）
}

// DefaultRetryConfig 返回默认重试配置
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  1 * time.Second,
		MaxDelay:   30 * time.Second,
		Jitter:     0.1,
	}
}

// BackoffDelay 计算第 attempt 次重试（0 基）之前应等待的时长。
//
// 退避算法：min(MaxDelay, BaseDelay * 2^attempt) * (1 + rand*jitter)，
// 结果再按 MaxDelay 限幅。调用方负责决定是否重试与何时放弃 —— 需要按错误
// 分类决定动作的场景（例如 LLM 调用）比 RetryWithBackoff 的"一律重试"更
// 适合直接用这个函数。
func BackoffDelay(cfg RetryConfig, attempt int) time.Duration {
	backoff := float64(cfg.BaseDelay) * math.Pow(2, float64(attempt))
	if backoff > float64(cfg.MaxDelay) {
		backoff = float64(cfg.MaxDelay)
	}

	// 添加抖动: delay * (1 + rand*jitter)
	jitterFactor := 1.0 + rand.Float64()*cfg.Jitter
	delay := time.Duration(backoff * jitterFactor)

	// 再次限幅到 MaxDelay
	if delay > cfg.MaxDelay {
		delay = cfg.MaxDelay
	}

	return delay
}

// RetryWithBackoff 带指数退避的重试函数
// 循环执行 fn，如果返回 nil 则成功
// 如果返回 error 且 ctx 未取消，等待退避时间后重试
// 退避算法见 BackoffDelay
// 如果 ctx 取消，立即返回 ctx.Err()
// 达到 MaxRetries 后返回最终错误，包装上重试次数信息
func RetryWithBackoff(ctx context.Context, cfg RetryConfig, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		// 检查 context 是否已取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := fn()
		if err == nil {
			return nil
		}

		lastErr = err

		// 最后一次尝试后不再等待
		if attempt == cfg.MaxRetries {
			break
		}

		// 等待延迟或 context 取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(BackoffDelay(cfg, attempt)):
		}
	}

	return fmt.Errorf("重试 %d 次后仍失败: %w", cfg.MaxRetries, lastErr)
}

// IsContextCanceled 判断错误是否由 context 取消引起（使用 errors.Is）
func IsContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
