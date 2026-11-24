package core

import (
	"regexp"
	"strings"
)

// 误性命令的正则模式（源自 Python 实现中的 _DESTRUCTIVE_PATTERNS）
var destructivePattern = regexp.MustCompile(
	`(?:^|\s|&&|\|\||;|` + "`" + `)(?:` +
		`rm\s|rmdir\s|` +
		`cp\s|install\s|` +
		`mv\s|` +
		`sed\s+-i|` +
		`truncate\s|` +
		`dd\s|` +
		`shred\s|` +
		`git\s+(?:reset|clean|checkout)\s` +
		`)`,
)

// 输出重定向覆盖（> 但不是 >>）
var redirectOverwrite = regexp.MustCompile(`[^>]>[^>]|^>[^>]`)

// IsDestructiveCommand 检测终端命令是否为破坏性命令（可能修改/删除文件）
//
// 基于正则模式匹配 rm、rmdir、mv、sed -i、dd、git reset/clean 等危险操作。
// 同时也检测输出重定向覆盖（> 而非 >>）。
func IsDestructiveCommand(cmd string) bool {
	if cmd == "" {
		return false
	}
	// 额外检测显式高危命令字面
	explosivePrefixes := []string{
		"rm -rf /",
		"rm -rf ~",
		"dd if=",
		"mkfs.",
		":(){ :|:& };:",
		"format c:",
	}
	cmdLower := strings.ToLower(strings.TrimSpace(cmd))
	for _, p := range explosivePrefixes {
		if strings.HasPrefix(cmdLower, p) {
			return true
		}
	}
	if destructivePattern.MatchString(cmd) {
		return true
	}
	if redirectOverwrite.MatchString(cmd) {
		return true
	}
	return false
}

// maxParallelToolCalls 单批并发执行的工具调用上限
// （对齐 hermes-agent 的 _MAX_TOOL_WORKERS=8）。
const maxParallelToolCalls = 8

// parallelSafeTools 可安全并发的内置工具。
//
// 对齐 hermes-agent 的 _PARALLEL_SAFE_TOOLS：只放明确的只读工具。
// terminal / curl / write_file / delete_path / memory 以及一切 MCP 远端工具
// （语义未知）都不在这里 —— 并发执行有状态工具会产生难以复现的相互干扰，
// 而"少数几次并行读"带来的收益不值这个风险。
var parallelSafeTools = map[string]bool{
	"read_file":      true,
	"list_directory": true,
	"vision_analyze": true,
}

// isParallelSafe 判断单个调用是否可以并发执行。
func isParallelSafe(name string) bool {
	return parallelSafeTools[name]
}

// planToolBatchSegments 把一批工具调用切成若干执行段。
//
// 连续的可并发调用合成一段（段内并发），任何不可并发的调用独占一段（串行）；
// 返回每段对应的原始下标。切段的语义与 hermes-agent 的
// _plan_tool_batch_segments 一致：段内无相互依赖，段间保持顺序。
func planToolBatchSegments(calls []ToolCallRef) [][]int {
	segments := make([][]int, 0, len(calls))

	var current []int
	flush := func() {
		if len(current) > 0 {
			segments = append(segments, current)
			current = nil
		}
	}

	for i, call := range calls {
		if isParallelSafe(call.Function.Name) {
			current = append(current, i)

			continue
		}

		flush()
		segments = append(segments, []int{i})
	}
	flush()

	return segments
}

// IsMultimodalToolResult 检测结果是否为多模态工具结果
//
// 多模态结果以特定 JSON 格式返回（包含 "_multimodal":true 和 "content" 数组）。
// 在 Go 版本中，由于 Message.Content 为 string 类型，此函数检测字符串是否
// 为多模态结果的 JSON 编码形式。
func IsMultimodalToolResult(result string) bool {
	// 快速检查：非空且以 { 开头
	if len(result) == 0 || result[0] != '{' {
		return false
	}
	// 串行扫描检测关键字段，避免完整 JSON 解析的开销
	if !strings.Contains(result, `"_multimodal"`) {
		return false
	}
	return strings.Contains(result, `"_multimodal":true`) || strings.Contains(result, `"_multimodal": true`)
}

// MakeToolResultMessage 构建工具结果消息
//
// 返回 role=tool 的 Message，包含工具调用 ID、工具名称和输出内容。
func MakeToolResultMessage(callID, name, output string, isError bool) Message {
	// 对错误输出添加标记前缀
	content := output
	if isError {
		content = "[error] " + output
	}
	return Message{
		Role:       RoleTool,
		Content:    content,
		ToolCallID: callID,
		Name:       name,
	}
}

// MultimodalTextSummary 从多模态结果中提取纯文本摘要
//
// 如果结果是多模态格式，尝试提取 text_summary 字段或拼接所有 text 类型内容块。
// 对于普通字符串，直接返回。
func MultimodalTextSummary(content string) string {
	if content == "" {
		return ""
	}
	if !IsMultimodalToolResult(content) {
		return content
	}
	// 尝试查找 text_summary 字段（简单的子串搜索）
	// 格式: "text_summary": "..."
	const key = `"text_summary"`
	idx := strings.Index(content, key)
	if idx >= 0 {
		// 跳过 key + ":"
		rest := content[idx+len(key):]
		rest = strings.TrimLeft(rest, " \t")
		if len(rest) > 0 && rest[0] == ':' {
			rest = rest[1:]
			rest = strings.TrimLeft(rest, " \t")
			if len(rest) > 0 && rest[0] == '"' {
				// 提取双引号字符串（简单实现，足够处理正常 JSON）
				rest = rest[1:]
				end := strings.Index(rest, `"`)
				if end >= 0 {
					return rest[:end]
				}
			}
		}
	}
	return "[multimodal tool result]"
}
