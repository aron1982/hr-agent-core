package core

import "strings"

// DefaultAgentIdentity 默认身份标识（未显式配置系统提示词时的兜底身份）。
//
// 原来放在 prompt_builder.go —— 那个文件里的 BuildPrompt 与这里的
// BuildSystemPrompt 是同一件事的两套实现，已收敛到本文件。
const DefaultAgentIdentity = "You are HrBuddy Agent, an AI coding assistant."

// ToolGuidance 工具使用指导文本（对齐 hermes-agent 的 execution_guidance）。
//
// 含 session_search 的使用时机：上下文中的历史内容（尤其是被标记
// [PRUNED: ...] 的旧工具输出）可随时用会话检索找回原文，模型不必
// 假装"从未见过"，也不要凭空编造被裁剪掉的内容。
const ToolGuidance = "You have access to tools for file operations, terminal commands, and more. " +
	"When you need details from earlier in this conversation that are no longer in context " +
	"(e.g. content marked [PRUNED: ...]), use the session_search tool to recover the original — never fabricate it."

// SystemPrompt 三段式系统提示词
// Stable:  身份定义 + 工具指导（长期不变）
// Context: 用户消息 + 上下文文件（随项目变化）
// Volatile: 记忆快照 + 时间戳（随时间变化）
type SystemPrompt struct {
	Stable   string // 身份 + 工具指导
	Context  string // 用户消息 + 上下文文件
	Volatile string // 记忆快照 + 时间戳
}

// BuildSystemPrompt 构建三段式提示词
// 三段用 \n\n 分隔；空段（含只有空白的段）自动跳过。
//
// 过滤是必要的：调用方常传 []string{""}（记忆为空、会话无额外上下文），
// 不过滤会在提示词中间留下多余空行 —— 既浪费 token，也让上游前缀缓存
// 对不齐（同样内容在不同会话里拼出的空白数不同）。
func BuildSystemPrompt(stable []string, context []string, volatile []string) string {
	var parts []string

	for _, group := range [][]string{stable, context, volatile} {
		joined := strings.Join(nonEmptyStrings(group), "\n\n")
		if joined != "" {
			parts = append(parts, joined)
		}
	}

	return strings.Join(parts, "\n\n")
}

// nonEmptyStrings 丢弃空白元素，并对保留的元素做首尾裁剪。
func nonEmptyStrings(items []string) []string {
	out := make([]string, 0, len(items))

	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// buildVolatilePrompt 拼 volatile 段：每会话固定的元信息行在前，记忆快照在后。
func buildVolatilePrompt(meta []string, memory string) string {
	items := append([]string{}, meta...)
	if memory != "" {
		items = append(items, memory)
	}

	return BuildSystemPrompt(nil, nil, items)
}

// NewSystemPrompt 创建三段式提示词结构
func NewSystemPrompt(stable, context, volatile []string) *SystemPrompt {
	return &SystemPrompt{
		Stable:   BuildSystemPrompt(stable, nil, nil),
		Context:  BuildSystemPrompt(nil, context, nil),
		Volatile: BuildSystemPrompt(nil, nil, volatile),
	}
}

// Full 返回完整的三段式提示词
func (sp *SystemPrompt) Full() string {
	return BuildSystemPrompt(
		[]string{sp.Stable},
		[]string{sp.Context},
		[]string{sp.Volatile},
	)
}
