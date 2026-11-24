package llm

import (
	"strings"
	"testing"
)

// TestApplyProfileTemperature 覆盖两种真实的 temperature 策略：
// 强制固定（kimi-k2.6 只接受 1.0）与完全省略（Kimi K3 由服务端管理）。
func TestApplyProfileTemperature(t *testing.T) {
	kwargs := map[string]any{"temperature": 0.7}
	applyProfile(kwargs, Config{ModelProfile: ModelProfile{OmitTemperature: true}})

	if _, ok := kwargs["temperature"]; ok {
		t.Fatalf("OmitTemperature 应删除 temperature，实际仍有 %v", kwargs["temperature"])
	}

	kwargs = map[string]any{"temperature": 0.7}
	applyProfile(kwargs, Config{ModelProfile: ModelProfile{TemperatureFixed: 1.0}})

	if kwargs["temperature"] != 1.0 {
		t.Fatalf("TemperatureFixed 应覆盖为 1.0，实际 %v", kwargs["temperature"])
	}
}

// TestApplyProfileTopPClamp 验证 GLM 的 top_p 上限钳制（>= 1 会被拒绝）。
func TestApplyProfileTopPClamp(t *testing.T) {
	kwargs := map[string]any{"top_p": 1.0}
	applyProfile(kwargs, Config{Profile: ChannelProfile{TopPMax: 0.99}})

	if kwargs["top_p"] != 0.99 {
		t.Fatalf("top_p 应钳制到 0.99，实际 %v", kwargs["top_p"])
	}
}

// TestApplyProfileMaxTokensField 验证输出上限键名改写（OpenAI o1/gpt-5 系）。
func TestApplyProfileMaxTokensField(t *testing.T) {
	kwargs := map[string]any{maxTokensKeyStandard: 100}
	applyProfile(kwargs, Config{Profile: ChannelProfile{MaxTokensField: maxTokensKeyCompletion}})

	if _, ok := kwargs[maxTokensKeyStandard]; ok {
		t.Fatal("改写后不应保留 max_tokens")
	}

	if kwargs[maxTokensKeyCompletion] != 100 {
		t.Fatalf("max_completion_tokens 应为 100，实际 %v", kwargs[maxTokensKeyCompletion])
	}
}

// TestApplyProfileReasoningModelNameSuffix 验证 DeepSeek 风格：模型名后缀
// 触发思考，并把后缀从实际发送的 model 上剥掉。
func TestApplyProfileReasoningModelNameSuffix(t *testing.T) {
	kwargs := map[string]any{"model": "deepseek-reasoner-thinking"}
	applyProfile(kwargs, Config{Profile: ChannelProfile{ReasoningShape: ReasoningModelNameSuffix}})

	if kwargs["model"] != "deepseek-reasoner" {
		t.Fatalf("应剥掉 -thinking 后缀，实际 %v", kwargs["model"])
	}

	if _, ok := kwargs["thinking"]; !ok {
		t.Fatal("应注入 thinking 字段")
	}
}

// TestApplyProfileReasoningNotInjectedWithoutSwitch 未显式设置 Thinking 时
// 不注入思考字段 —— 这些属于上游的"未知字段"，宁可不发也不要触发严格渠道 400。
func TestApplyProfileReasoningNotInjectedWithoutSwitch(t *testing.T) {
	kwargs := map[string]any{"model": "kimi-k2.6"}
	applyProfile(kwargs, Config{Profile: ChannelProfile{ReasoningShape: ReasoningExtraBodyThinking}})

	if _, ok := kwargs["extra_body"]; ok {
		t.Fatal("Thinking 未设置时不应注入 extra_body")
	}
}

// TestApplyProfileReasoningSplit 验证 MiniMax：要求上游把思考内容拆到独立
// 字段（顶层 reasoning_split，不是 extra_body），否则思考混在 content 里带
// <think> 标签下发。
func TestApplyProfileReasoningSplit(t *testing.T) {
	kwargs := map[string]any{"model": "MiniMax-M3"}
	applyProfile(kwargs, Config{Profile: ChannelProfile{ReasoningSplit: true}})

	if kwargs["reasoning_split"] != true {
		t.Fatalf("应注入顶层 reasoning_split=true，实际 %v", kwargs["reasoning_split"])
	}

	if _, ok := kwargs["extra_body"]; ok {
		t.Fatal("reasoning_split 不应放进 extra_body")
	}

	// 未声明该能力的渠道不得注入（严格渠道会因未知字段 400）
	kwargs = map[string]any{"model": "glm-4.5"}
	applyProfile(kwargs, Config{})
	if _, ok := kwargs["reasoning_split"]; ok {
		t.Fatal("未声明 ReasoningSplit 时不应注入")
	}
}

// TestThinkingOnValue 验证「开启思考」的取值可按系列覆盖：MiniMax 只认
// adaptive（发 enabled 直接 400: invalid thinking.type）。
func TestThinkingOnValue(t *testing.T) {
	on := true

	// 默认：开启值仍是通用的 enabled
	kwargs := map[string]any{"model": "glm-4.6"}
	applyProfile(kwargs, Config{Profile: ChannelProfile{ReasoningShape: ReasoningTopLevelThinking}, Thinking: &on})

	if got := kwargs["thinking"].(map[string]any)["type"]; got != "enabled" {
		t.Fatalf("默认开启值应为 enabled，实际 %v", got)
	}

	// MiniMax：顶层 thinking + adaptive
	kwargs = map[string]any{"model": "MiniMax-M3"}
	applyProfile(kwargs, Config{
		Profile:  ChannelProfile{ReasoningShape: ReasoningTopLevelThinking, ThinkingOnValue: "adaptive"},
		Thinking: &on,
	})

	thinking, ok := kwargs["thinking"].(map[string]any)
	if !ok {
		t.Fatal("MiniMax 应注入顶层 thinking")
	}
	if thinking["type"] != "adaptive" {
		t.Fatalf("MiniMax 开启值应为 adaptive，实际 %v", thinking["type"])
	}

	// 关闭值不受影响
	off := false
	kwargs = map[string]any{"model": "MiniMax-M3"}
	applyProfile(kwargs, Config{
		Profile:  ChannelProfile{ReasoningShape: ReasoningTopLevelThinking, ThinkingOnValue: "adaptive"},
		Thinking: &off,
	})
	if got := kwargs["thinking"].(map[string]any)["type"]; got != "disabled" {
		t.Fatalf("关闭值应为 disabled，实际 %v", got)
	}
}

// TestReasoningTextPrefersTextField 验证 MiniMax 同帧同时返回
// reasoning_content 与 reasoning_details 时不会重复计数。
func TestReasoningTextPrefersTextField(t *testing.T) {
	details := []reasoningDetail{{Text: "同一段思考"}}

	if got := reasoningText("同一段思考", "", details); got != "同一段思考" {
		t.Fatalf("优先取 reasoning_content，实际 %q", got)
	}

	// 只给结构化数组时用它兜底
	if got := reasoningText("", "", details); got != "同一段思考" {
		t.Fatalf("reasoning_content 缺失时应回退 reasoning_details，实际 %q", got)
	}
}

// TestStripImageDataURLPrefix 验证 GLM 要求的裸 base64，同时覆盖字符串与
// 对象两种 image_url 写法。
func TestStripImageDataURLPrefix(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": "data:image/png;base64,AAAA"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,BBBB"}},
		}},
	}

	kwargs := map[string]any{"messages": msgs}
	applyProfile(kwargs, Config{Profile: ChannelProfile{StripImageDataURLPrefix: true}})

	parts := msgs[0]["content"].([]any)
	if got := parts[0].(map[string]any)["image_url"]; got != "AAAA" {
		t.Fatalf("字符串写法应剥掉前缀，实际 %v", got)
	}

	inner := parts[1].(map[string]any)["image_url"].(map[string]any)
	if inner["url"] != "BBBB" {
		t.Fatalf("对象写法应剥掉前缀，实际 %v", inner["url"])
	}
}

// TestParseSSEStreamAccumulatesToolCalls 工具调用被拆到多帧，必须按 index
// 拼装；同时验证"只带 usage 的尾帧"与"不发 [DONE]"两种真实上游行为。
func TestParseSSEStreamAccumulatesToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`data: {"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_cache_hit_tokens":4},"choices":[]}`,
	}, "\n\n")

	content, _, calls, usage, err := parseSSEStream(strings.NewReader(stream), nil, nil, nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if content != "好" {
		t.Fatalf("content 期望 好，实际 %q", content)
	}

	if len(calls) != 1 || calls[0].Name != "lookup" || calls[0].Arguments != `{"q":"x"}` {
		t.Fatalf("工具调用累积错误: %+v", calls)
	}

	if usage.PromptTokens != 10 || usage.CachedTokens != 4 {
		t.Fatalf("usage 期望 10/4，实际 %+v", usage)
	}
}

// TestEndpointIsAsIs 端点只追加固定路径，不做 /v1 猜测 —— 智谱与火山的
// 路径层级不同（/api/paas/v4、/api/v3），任何启发式都会拼错其中一部分。
func TestEndpointIsAsIs(t *testing.T) {
	cases := map[string]string{
		"https://open.bigmodel.cn/api/paas/v4":     "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"https://ark.cn-beijing.volces.com/api/v3": "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		"https://api.deepseek.com/v1/":             "https://api.deepseek.com/v1/chat/completions",
	}

	for in, want := range cases {
		if got := Endpoint(in); got != want {
			t.Errorf("endpoint(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestAuthHeaderValue 避免用户已手写 "Bearer " 前缀时拼成 "Bearer Bearer x"。
func TestAuthHeaderValue(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"sk-abc":         "Bearer sk-abc",
		"Bearer sk-abc":  "Bearer sk-abc",
		"bearer sk-abc":  "bearer sk-abc",
		"  sk-trimmed  ": "Bearer sk-trimmed",
	}

	for in, want := range cases {
		if got := authHeaderValue(in); got != want {
			t.Errorf("authHeaderValue(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
