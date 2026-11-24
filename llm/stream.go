// OpenAI 兼容 SSE 流解析。
//
// 流式把一次调用的结果拆成多条 `data:` 帧逐块送达，帧结构与一次性响应
// 是同一套 JSON 模型，只是内容字段变成增量：
//
//	data: {"choices":[{"delta":{"content":"你"},...}],...}
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c_1",...}]}}]}
//	data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
//	data: {"usage":{"prompt_tokens":10,...},"choices":[]}
//	data: [DONE]
//
// 解析必须是有状态的：工具调用会被拆到多帧、usage 可能出现在任意一帧、
// reasoning_content 与 content 交替出现。逐帧独立解析必然丢数据。
package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// sseFrame 单条 SSE 数据帧（chat.completion.chunk）。
type sseFrame struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// ReasoningContent 思考链增量。DeepSeek / Qwen / MiniMax 等用这个
			// 扩展字段承载思考内容，与正文 content 分开处理。
			ReasoningContent string `json:"reasoning_content"`
			// Reasoning 部分厂商（含 OpenRouter 归一化后）使用该字段名。
			Reasoning string `json:"reasoning"`
			// ReasoningDetails MiniMax（reasoning_split=true）等把思考内容同时
			// 放在该数组里；reasoning_content 为空时用它兜底。
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usagePayload `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// usagePayload 各家 usage 字段的并集。
//
// cache 统计的位置并不统一：OpenAI 放在 prompt_tokens_details.cached_tokens，
// DeepSeek 用顶层 prompt_cache_hit_tokens。两个都读，谁有取谁。
type usagePayload struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	TotalTokens          int `json:"total_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissToken int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage 归一化为内部 Usage。
func (u *usagePayload) toUsage() Usage {
	if u == nil {
		return Usage{}
	}

	cached := u.PromptCacheHitTokens
	if cached == 0 && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}

	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     cached,
	}
}

// parseSSEStream 从响应体逐帧解析 OpenAI 兼容 SSE。
//
// onChunk 每收到一段正文增量即回调（可为 nil，表示只累加不转发）；
// onReasoning 每收到一段思考增量即回调（可为 nil，忽略思考内容）。
//
// rawFrames 非 nil 时收集每一帧的原始 data 文本（用于 LLM 转储分析，
// 见 infra.LLMCallDump）。收集会额外持有整段响应文本的引用，只在
// 转储开启时传入。
//
// 容错处理：
//   - 只认 `data:` 行，`: ping` 心跳与空行直接跳过；
//   - 上游不发 `[DONE]` 时，读到 EOF 视为正常结束；
//   - choices 为空的帧（例如只带 usage 的尾帧）跳过，不算错误。
func parseSSEStream(r io.Reader, onChunk func(string), onReasoning func(string), rawFrames *[]string) (string, string, []ToolCall, Usage, error) {
	content := &strings.Builder{}
	reasoning := &strings.Builder{}
	toolAcc := newToolAccumulator()
	var usage Usage

	scanner := bufio.NewScanner(r)
	// 单帧 JSON 可能很大（长 arguments 流、长文本块），默认 64KB 行缓冲会截断。
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if rawFrames != nil && data != "" && data != "[DONE]" {
			*rawFrames = append(*rawFrames, data)
		}
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}

		var frame sseFrame
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return "", "", nil, Usage{}, fmt.Errorf("解析流帧失败: %w", err)
		}

		if frame.Error != nil {
			return "", "", nil, Usage{}, &Error{Code: frame.Error.Code, Message: frame.Error.Message}
		}

		if len(frame.Choices) > 0 {
			delta := frame.Choices[0].Delta

			if text := reasoningText(delta.ReasoningContent, delta.Reasoning, delta.ReasoningDetails); text != "" {
				reasoning.WriteString(text)
				if onReasoning != nil {
					onReasoning(text)
				}
			}

			if delta.Content != "" {
				content.WriteString(delta.Content)
				if onChunk != nil {
					onChunk(delta.Content)
				}
			}

			for _, tc := range delta.ToolCalls {
				toolAcc.Add(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
		}

		if frame.Usage != nil {
			usage = frame.Usage.toUsage()
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", nil, Usage{}, fmt.Errorf("读取流失败: %w", err)
	}

	return content.String(), reasoning.String(), toolAcc.Result(), usage, nil
}

// toolAccumulator 按 index 累积工具调用增量，流结束时按 index 升序还原。
//
// OpenAI 兼容流把一次工具调用拆成多帧：首帧给 id + function.name，
// 后续帧给 function.arguments 的增量片段，全部按 index 索引。
type toolAccumulator struct {
	byIndex map[int]*toolDraft
}

// toolDraft 单个工具调用的累积状态。
type toolDraft struct {
	id   string
	name string
	args *strings.Builder
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byIndex: map[int]*toolDraft{}}
}

// Add 累积一帧工具调用增量。
func (acc *toolAccumulator) Add(index int, id, name, arguments string) {
	draft, ok := acc.byIndex[index]
	if !ok {
		draft = &toolDraft{args: &strings.Builder{}}
		acc.byIndex[index] = draft
	}

	if id != "" {
		draft.id = id
	}

	if name != "" {
		draft.name = name
	}

	if arguments != "" {
		draft.args.WriteString(arguments)
	}
}

// Result 返回按 index 升序排列的完整工具调用列表。
func (acc *toolAccumulator) Result() []ToolCall {
	if len(acc.byIndex) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(acc.byIndex))
	for idx := range acc.byIndex {
		indexes = append(indexes, idx)
	}

	sort.Ints(indexes)

	result := make([]ToolCall, 0, len(indexes))
	for _, idx := range indexes {
		draft := acc.byIndex[idx]
		if draft.name == "" {
			continue
		}

		result = append(result, ToolCall{
			ID:        draft.id,
			Name:      draft.name,
			Arguments: draft.args.String(),
		})
	}

	return result
}

// firstNonEmpty 返回第一个非空字符串（思考字段名各家用得不一致）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// reasoningDetail 思考内容的结构化表达（MiniMax reasoning_details 数组元素，
// 形如 {"type":"reasoning.text","format":"MiniMax-response-v1","text":"..."}）。
type reasoningDetail struct {
	Text string `json:"text"`
}

// reasoningText 归一化思考内容：先取文本字段，都为空时再用结构化数组兜底。
//
// 顺序不能反：MiniMax 同一帧里 reasoning_content 与 reasoning_details[].text
// 内容相同，先取数组会导致同一段思考被累计两次。
func reasoningText(content, plain string, details []reasoningDetail) string {
	if text := firstNonEmpty(content, plain); text != "" {
		return text
	}

	for _, d := range details {
		if d.Text != "" {
			return d.Text
		}
	}

	return ""
}
