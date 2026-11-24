package llm

import (
	"encoding/json"
	"testing"
)

// TestBuildBodyStreamIncludeUsage 流式请求按渠道开关决定是否下发
// stream_options.include_usage。
//
// 背景：MiniMax 流式默认每帧 "usage": null，必须显式要求才下发 usage 帧
// （该帧 choices 为空数组），否则 token 统计恒为 0、缓存命中率全部失真。
// 标准 OpenAI 渠道默认已带 usage，不需要这个参数。
func TestBuildBodyStreamIncludeUsage(t *testing.T) {
	cases := []struct {
		name    string
		profile ChannelProfile
		want    bool
	}{
		{"minimax 需要", ChannelProfile{StreamIncludeUsage: true}, true},
		{"标准渠道不需要", ChannelProfile{}, false},
		{"其他 profile 组合", ChannelProfile{ReasoningSplit: true, TopPMax: 0.99}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{cfg: Config{Profile: tc.profile}}
			body := c.buildBody(Options{Stream: true})

			so, ok := body["stream_options"]
			if !tc.want {
				if ok {
					t.Fatalf("不应下发 stream_options，实际: %v", so)
				}

				return
			}
			if !ok {
				t.Fatal("应下发 stream_options")
			}
			m, ok := so.(map[string]any)
			if !ok {
				t.Fatalf("stream_options 类型异常: %T", so)
			}
			if m["include_usage"] != true {
				t.Fatalf("include_usage 应为 true，实际: %v", m["include_usage"])
			}
		})
	}
}

// TestBuildBodyNoStreamIncludeUsage 非流式请求不下发 stream_options。
//
// 非流式调用始终返回 usage，多传这个参数没有意义，还可能被严格校验的
// 渠道拒参。
func TestBuildBodyNoStreamIncludeUsage(t *testing.T) {
	c := &Client{cfg: Config{Profile: ChannelProfile{StreamIncludeUsage: true}}}
	body := c.buildBody(Options{Stream: false})

	if _, ok := body["stream_options"]; ok {
		t.Fatal("非流式不应下发 stream_options")
	}
}

// TestBuildBodyExtraBodyOverridesStreamOptions 用户 ExtraBody 仍可覆盖。
func TestBuildBodyExtraBodyOverridesStreamOptions(t *testing.T) {
	c := &Client{cfg: Config{
		Profile:   ChannelProfile{StreamIncludeUsage: true},
		ExtraBody: map[string]any{"stream_options": map[string]any{"include_usage": false}},
	}}
	body := c.buildBody(Options{Stream: true})

	so, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options 类型异常: %T", body["stream_options"])
	}
	if so["include_usage"] != false {
		t.Fatalf("ExtraBody 应覆盖开关，实际: %v", so)
	}
}

// TestUsagePayloadParseCachedTokens usage 帧的 choices 为空数组，
// 仍须能解析出 prompt_tokens 与 cached_tokens。
//
// 这正是 MiniMax 下发 usage 的方式：尾帧 choices: []，只带 usage。
func TestUsagePayloadParseCachedTokens(t *testing.T) {
	raw := `{
		"id": "074dc3841138bed5ac36e8ee773d7342",
		"choices": [],
		"created": 1795068036,
		"model": "MiniMax-M3.1-Flash",
		"object": "chat.completion.chunk",
		"usage": {
			"total_tokens": 229,
			"prompt_tokens": 201,
			"completion_tokens": 28,
			"completion_tokens_details": {"reasoning_tokens": 24},
			"prompt_tokens_details": {"cached_tokens": 200}
		}
	}`

	// 整帧解析：usage 是 sseFrame 的子字段，不能直接 Unmarshal 到 usagePayload
	var f sseFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.Usage == nil {
		t.Fatal("usage 帧未解析出 usage（choices 为空数组不应影响 usage 解析）")
	}

	usage := f.Usage.toUsage()
	if usage.PromptTokens != 201 {
		t.Fatalf("PromptTokens 应为 201，实际: %d", usage.PromptTokens)
	}
	if usage.CachedTokens != 200 {
		t.Fatalf("CachedTokens 应为 200，实际: %d", usage.CachedTokens)
	}
	if usage.CompletionTokens != 28 {
		t.Fatalf("CompletionTokens 应为 28，实际: %d", usage.CompletionTokens)
	}
}

// TestUsagePayloadParseDeepSeekCacheHit DeepSeek 用顶层 prompt_cache_hit_tokens。
func TestUsagePayloadParseDeepSeekCacheHit(t *testing.T) {
	raw := `{"choices": [{"delta": {"content": "hi"}}],
	         "usage": {"prompt_tokens": 100, "completion_tokens": 10,
	                   "prompt_cache_hit_tokens": 64, "prompt_cache_miss_tokens": 36}}`

	var f sseFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.Usage == nil {
		t.Fatal("未解析出 usage")
	}

	usage := f.Usage.toUsage()
	if usage.CachedTokens != 64 {
		t.Fatalf("CachedTokens 应为 64，实际: %d", usage.CachedTokens)
	}
}
