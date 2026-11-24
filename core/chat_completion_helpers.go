package core

import "encoding/json"

// ParseToolCallsFromResponse 从 LLM 响应中解析工具调用。
//
// 注：当前**未接线** —— 活路径只认模型返回的原生 tool_calls（见 llm 包）。
// 从文本里兜底解析 tool_calls 是 hermes-agent 有的能力（早期兼容模型），
// 待需要时再接。原先与本函数同文件的 BuildMessagesForAPI 与
// agent_init.messagesToOpenAI 重复，已删除。
//
// content: LLM 返回的文本内容
// toolCalls: 模型原生的 tool_calls 列表
// 返回解析后的工具调用列表和是否有工具调用
func ParseToolCallsFromResponse(content string, toolCalls []ToolCallRef) ([]ToolCallRef, bool) {
	// 如果有原生 tool_calls，直接返回
	if len(toolCalls) > 0 {
		return toolCalls, true
	}

	// 尝试从文本内容中解析工具调用（部分模型以文本形式返回工具调用）
	if content != "" {
		parsed := parseTextToolCalls(content)
		if len(parsed) > 0 {
			return parsed, true
		}
	}

	return nil, false
}

// parseTextToolCalls 从文本中尝试解析工具调用 JSON
func parseTextToolCalls(content string) []ToolCallRef {
	// 尝试将整段文本解析为 tool_calls JSON 数组
	var calls []ToolCallRef
	if err := json.Unmarshal([]byte(content), &calls); err == nil && len(calls) > 0 {
		return calls
	}

	// 尝试解析为单个 tool_call 对象
	var single ToolCallRef
	if err := json.Unmarshal([]byte(content), &single); err == nil && single.ID != "" {
		return []ToolCallRef{single}
	}

	return nil
}
