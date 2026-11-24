package core

import "fmt"

// 工具结果修剪（Phase-1 式，零 LLM 成本）。
//
// 动机：长会话的上下文膨胀大头是旧工具输出（read_file/terminal 的几 KB 到
// 几十 KB 正文），它们只在产生它们的回合有用，之后每个回合都要原样重发给
// 模型。这里把保护范围之外的旧 role:"tool" 正文替换成一行 stub —— 模型仍
// 知道"此处曾有一个结果、需要时可重跑工具获取"，语义无损，且完全不触碰
// tool_call/tool 的配对结构（ID 原样保留，不会触发端点的孤儿 tool 400）。
//
// 红线（讨论结论，对应 MiniMax 对 assistant 思维链连续性的要求）：
//   - assistant 消息一律不动：tool_calls 结构与思考内容（<think> 等）是
//     思维链的组成部分，截断会制造残缺历史；
//   - 边界取「最后一条 user 消息」而非最后一条消息：单回合内可能有多级
//     assistant(tool_calls) → tool 嵌套，按消息切会把进行中的调用链腰斩；
//   - 保护尾按 token 预算向前扩展，最近的内容不裁。
//
// 与 Compress 的关系：修剪在更低阈值触发（压缩阈值的一半），把旧工具输出
// 提前收走后，全量压缩（机械截断摘要）可以晚很久才发生；两者叠加而非互斥。
const (
	// pruneTailBudgetTokens 保护尾 token 预算：最近这么多 token 内的消息
	// 保持原样（对齐 hermes 的 protect-tail-by-token-budget 思路，取值保守）。
	pruneTailBudgetTokens = 8000

	// minPruneChars 短于该长度的工具结果不修剪：stub 与正文差不多大，
	// 剪了不省 token 反而增加噪音。
	minPruneChars = 256
)

// PruneToolResults 对消息历史做零成本工具结果修剪。
//
// budget 为触发阈值：未达到时原样返回 —— 修剪结果会被回写会话历史
// （prepareMessages 里 SetMessages），早期会话体量小，没必要为一两条小
// 结果改写历史。返回修剪后的列表与修剪条数。
func PruneToolResults(messages []Message, budget int) ([]Message, int) {
	if len(messages) == 0 {
		return messages, 0
	}
	if budget <= 0 {
		budget = 100000 / 2
	}
	if estimateTotalTokens(messages) <= budget {
		return messages, 0
	}

	return pruneOldToolResults(messages, estimateMessageTokens)
}

// pruneOldToolResults 按保护规则修剪旧工具结果（独立于触发阈值，便于测试）。
func pruneOldToolResults(messages []Message, estimate func(Message) int) ([]Message, int) {
	// 1. 边界：最后一条 user 消息。无 user 消息时从末尾向前按预算保护。
	lastUser := -1
	for i := range messages {
		if messages[i].Role == RoleUser {
			lastUser = i
		}
	}
	start := len(messages) - 1
	if lastUser >= 0 {
		start = lastUser
	}

	// 2. 保护尾：从边界向前按 token 预算扩展，protectedStart 之前才可修剪。
	//    边界之后的消息（活跃回合）不在修剪范围内，天然受保护。
	budget := pruneTailBudgetTokens
	protectedStart := 0
	for i := start; i >= 0; i-- {
		tok := estimate(messages[i])
		if tok > budget {
			protectedStart = i + 1
			break
		}
		budget -= tok
		protectedStart = i
	}
	if lastUser >= 0 && protectedStart > lastUser {
		protectedStart = lastUser
	}

	// 3. 收集 tool_call id → 工具名，stub 里带上名字便于模型理解上下文。
	names := make(map[string]string)
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				names[tc.ID] = tc.Function.Name
			}
		}
	}

	// 4. 修剪：只动保护范围外的 role:"tool"，替换正文、保留结构与 ID。
	out := make([]Message, len(messages))
	copy(out, messages)
	count := 0
	for i := 0; i < protectedStart; i++ {
		m := out[i]
		if m.Role != RoleTool || len(m.Content) < minPruneChars {
			continue
		}
		name := names[m.ToolCallID]
		if name == "" {
			name = "tool"
		}
		out[i] = Message{
			Role:       RoleTool,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			Content: fmt.Sprintf(
				"[PRUNED: %s output (%d chars) — pruned to save context. Original is archived: use session_search to recover it (search for distinctive text you remember), or re-run the tool]",
				name, len(m.Content),
			),
		}
		count++
	}

	return out, count
}
