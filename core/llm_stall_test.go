package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNonStreamCallStallsAndFailsFast 回归：上游「已回响应头、随后 body 停摆」
// 时，非流式调用必须在上限内失败，且不重试。
//
// 背景（实测）：协作子 agent 的非流式调用没有 idle 看门狗（无增量信号），
// 而 transport 的 ResponseHeaderTimeout 只覆盖首字节 —— 停在 io.ReadAll 上会
// 永久挂住：日志既无「LLM 调用完成」也无任何错误，只能等 30 分钟子任务墙钟。
func TestNonStreamCallStallsAndFailsFast(t *testing.T) {
	restore := llmNonStreamTimeout
	llmNonStreamTimeout = 200 * time.Millisecond
	t.Cleanup(func() { llmNonStreamTimeout = restore })

	var requests int32
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)

		// 先回响应头（越过 ResponseHeaderTimeout 的防护），随后 body 一直不结束
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	t.Cleanup(server.Close)
	// 后注册 → 先执行：先放行卡住的 handler，再让 server.Close 收尾
	t.Cleanup(func() { close(release) })

	agent, err := NewAgent(context.Background(), AgentConfig{
		ModelName:     "test-model",
		BaseURL:       server.URL,
		ContextWindow: 200000,
		MaxRetries:    3,
	})
	if err != nil {
		t.Fatalf("NewAgent 失败: %v", err)
	}

	start := time.Now()
	_, err = agent.Run(context.Background(), "你好")
	if !errors.Is(err, errLLMNonStreamTimeout) {
		t.Fatalf("应返回停滞哨兵错误，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("应在请求上限内失败，实际耗时 %s", elapsed)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("停滞超时不应重试（否则用户要等 4 倍上限），实际请求 %d 次", got)
	}
}
