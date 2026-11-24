package core

import (
	"context"
	"strings"
	"testing"

	"github.com/aron1982/hr-agent-core/llm"
)

// testCompress 无摘要通道的压缩（走 legacy 降级），供测试复用。
func testCompress(maxTokens int, messages []Message) []Message {
	return Compress(context.Background(), CompactionRequest{
		Messages:  messages,
		MaxTokens: maxTokens,
	}).Messages
}

// toolCallRef 构造一个工具调用引用。
func toolCallRef(id, name string) ToolCallRef {
	var tc ToolCallRef
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = "{}"

	return tc
}

// TestSanitizeToolPairsDropsOrphanResult 发起者已被截掉的结果必须丢弃。
func TestSanitizeToolPairsDropsOrphanResult(t *testing.T) {
	in := []Message{
		{Role: RoleTool, ToolCallID: "gone", Content: "孤儿结果"},
		{Role: RoleAssistant, Content: "调用中", ToolCalls: []ToolCallRef{toolCallRef("c1", "echo"), toolCallRef("c2", "echo")}},
		{Role: RoleTool, ToolCallID: "c1", Content: "结果1"},
		{Role: RoleAssistant, Content: "普通回答"},
	}

	out, _ := sanitizeToolPairs(in)
	if len(out) != 3 {
		t.Fatalf("期望丢弃 1 条孤儿结果，实际剩 %d 条: %+v", len(out), out)
	}
	if got := len(out[0].ToolCalls); got != 1 || out[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("c2 无对应结果应被剥离，实际 tool_calls=%+v", out[0].ToolCalls)
	}
}

// TestSanitizeToolPairsDropsEmptyAssistant 剥离后只剩空壳的 assistant 消息应整条丢弃。
func TestSanitizeToolPairsDropsEmptyAssistant(t *testing.T) {
	out, _ := sanitizeToolPairs([]Message{
		{Role: RoleAssistant, ToolCalls: []ToolCallRef{toolCallRef("c1", "echo")}},
	})
	if len(out) != 0 {
		t.Fatalf("空壳 assistant 消息应被丢弃，实际 %+v", out)
	}
}

// TestSanitizeToolPairsKeepsIntactOnes 配对完整的消息不应被改动。
func TestSanitizeToolPairsKeepsIntactOnes(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, ToolCalls: []ToolCallRef{toolCallRef("c1", "echo")}},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}

	if out, _ := sanitizeToolPairs(in); len(out) != len(in) {
		t.Fatalf("完整配对不应被裁剪，实际 %d 条: %+v", len(out), out)
	}
}

// TestCompressSanitizesToolPairs 端到端：截断把 assistant.tool_calls 与它的
// 结果拆开时，压缩产物里不能留下孤儿 tool 消息（端点会 400）。
func TestCompressSanitizesToolPairs(t *testing.T) {
	// 阈值极小：头部那次 assistant 说明（含 tool_calls）装不下，结果留下
	messages := []Message{
		{Role: RoleUser, Content: "start"},
		{Role: RoleAssistant, Content: strings.Repeat("说明", 500), ToolCalls: []ToolCallRef{toolCallRef("c1", "echo")}},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}

	compressed := testCompress(600, messages)
	if len(compressed) == 0 {
		t.Fatal("压缩后不应为空")
	}
	assertToolPairsIntact(t, compressed)
}

// TestCompressKeepsIntactHistoryUnchanged 未超阈值时压缩器不应改动历史。
func TestCompressKeepsIntactHistoryUnchanged(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, ToolCalls: []ToolCallRef{toolCallRef("c1", "echo")}},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}

	compressed := testCompress(10000, messages)
	if len(compressed) != len(messages) {
		t.Fatalf("未超阈值不应改动历史，实际 %d 条", len(compressed))
	}
}

// TestCompressThresholdPrefersModelCaps 阈值优先取渠道目录的模型能力。
func TestCompressThresholdPrefersModelCaps(t *testing.T) {
	withCaps := compressThresholdFor(AgentConfig{
		ContextWindow: 200000,
		Caps:          llm.ModelCaps{MaxInputTokens: 1000000},
	})
	if withCaps != 666666 {
		t.Fatalf("应优先用模型 caps 的 2/3，实际 %d", withCaps)
	}

	withoutCaps := compressThresholdFor(AgentConfig{ContextWindow: 200000})
	if withoutCaps != 133333 {
		t.Fatalf("无 caps 时应用 ContextWindow 的 2/3，实际 %d", withoutCaps)
	}
}

// assertToolPairsIntact 校验 tool 结果与 tool_calls 一一配对。
func assertToolPairsIntact(t *testing.T, messages []Message) {
	t.Helper()

	calls := make(map[string]bool)
	results := make(map[string]bool)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			calls[tc.ID] = true
		}
		if msg.Role == RoleTool {
			results[msg.ToolCallID] = true
		}
	}

	for id := range results {
		if !calls[id] {
			t.Fatalf("压缩后残留孤儿工具结果 %q: %+v", id, messages)
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("压缩后残留无结果的 tool_call %q: %+v", id, messages)
		}
	}
}
