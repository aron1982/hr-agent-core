package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunTurnSummarizesAfterBudgetExhausted 验证预算耗尽后补一次**不带工具**
// 的收尾总结（对齐 hermes-agent 的 _handle_max_iterations）：用户拿到的应该
// 是模型对已完成进展的说明，而不是空气泡或一句中断提示。
func TestRunTurnSummarizesAfterBudgetExhausted(t *testing.T) {
	var requests, noToolRequests int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		_, hasTools := body["tools"]
		if !hasTools {
			atomic.AddInt32(&noToolRequests, 1)
		}

		w.Header().Set("Content-Type", "application/json")

		if hasTools {
			// 一直要求调工具 → 逼出迭代预算耗尽
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":` +
				`[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}}]},` +
				`"finish_reason":"tool_calls"}]}`))

			return
		}

		// 收尾总结请求（不带工具）返回文本
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"收尾总结：已完成一次工具调用"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	agent, err := NewAgent(context.Background(), AgentConfig{
		ModelName:     "test-model",
		BaseURL:       server.URL,
		ContextWindow: 200000,
		MaxIterations: 1,
		Tools: map[string]ToolExecutor{
			"echo": func(_ context.Context, _ string) (string, error) { return "ok", nil },
		},
		ToolSchemas: []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "echo"},
		}},
	})
	if err != nil {
		t.Fatalf("NewAgent 失败: %v", err)
	}

	reply, err := agent.Run(context.Background(), "帮我查一下")
	if err != nil {
		t.Fatalf("回合失败: %v", err)
	}
	if !strings.Contains(reply, "收尾总结") {
		t.Fatalf("预算耗尽后应返回收尾总结，实际 %q", reply)
	}
	if atomic.LoadInt32(&noToolRequests) == 0 {
		t.Fatal("收尾总结调用不应携带工具定义")
	}
	if got := atomic.LoadInt32(&requests); got < 2 {
		t.Fatalf("期望至少 2 次 LLM 调用（工具轮 + 总结），实际 %d", got)
	}
}

// TestRunStreamPropagatesCachedTokens 上游命中前缀缓存的 token 数必须一路
// 透传到会话层：llm 已按渠道差异解析（OpenAI 的
// prompt_tokens_details.cached_tokens / DeepSeek 的 prompt_cache_hit_tokens），
// core 不能在半路把它丢掉。
func TestRunStreamPropagatesCachedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"stop\"}]," +
			"\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"total_tokens\":105,\"prompt_cache_hit_tokens\":80}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	agent, err := NewAgent(context.Background(), AgentConfig{
		ModelName:     "test-model",
		BaseURL:       server.URL,
		ContextWindow: 100000,
	})
	if err != nil {
		t.Fatalf("NewAgent 失败: %v", err)
	}

	reply, usage, err := agent.RunStream(context.Background(), "hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("回合失败: %v", err)
	}
	if reply != "你好" {
		t.Fatalf("回复异常: %q", reply)
	}
	if usage.PromptTokens != 100 || usage.CachedTokens != 80 {
		t.Fatalf("cached tokens 未透传到会话层，实际 %+v", usage)
	}
}
