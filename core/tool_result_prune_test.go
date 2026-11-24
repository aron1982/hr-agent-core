package core

import (
	"strings"
	"testing"
)

// pruneEstimateForTests 测试用的 token 估算：与生产公式同形（4 字符 ≈ 1 token）。
func pruneEstimateForTests(m Message) int {
	return len(m.Content) / 4
}

func bigContent(n int) string { return strings.Repeat("x", n) }

func asstWithCall(id, name string) Message {
	m := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCallRef{{
			ID:   id,
			Type: "function",
		}},
	}
	m.ToolCalls[0].Function.Name = name
	m.ToolCalls[0].Function.Arguments = `{"path":"main.go"}`
	return m
}

// 旧工具结果被修剪为 stub，活跃回合（最后一条 user 之后）不动，assistant 不动。
func TestPruneOldToolResults(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "看看 main.go"},
		asstWithCall("call-1", "read_file"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(60000)}, // 15000 tok，超出保护尾预算
		{Role: RoleUser, Content: "跑一下测试"},                             // 活跃回合边界
		asstWithCall("call-2", "terminal"),
		{Role: RoleTool, ToolCallID: "call-2", Content: bigContent(4000)}, // 活跃回合内，全保
	}

	out, count := pruneOldToolResults(msgs, pruneEstimateForTests)
	if count != 1 {
		t.Fatalf("应恰好修剪 1 条，实际 %d", count)
	}

	pruned := out[2]
	if pruned.Role != RoleTool || pruned.ToolCallID != "call-1" {
		t.Fatalf("修剪后应保留 role/tool_call_id，实际 %+v", pruned)
	}
	if !strings.Contains(pruned.Content, "read_file") || !strings.Contains(pruned.Content, "60000") {
		t.Fatalf("stub 应含工具名与原长度，实际 %q", pruned.Content)
	}

	// assistant 的 tool_calls 结构与思考载体原样保留（思维链红线）
	for _, idx := range []int{1, 4} {
		if out[idx].Content != msgs[idx].Content || len(out[idx].ToolCalls) != 1 {
			t.Fatalf("assistant 消息不应被改动，idx=%d 实际 %+v", idx, out[idx])
		}
	}
	// 活跃回合的工具结果原样保留
	if out[5].Content != msgs[5].Content {
		t.Fatalf("活跃回合的工具结果不应被修剪")
	}
	// 原切片不被改动
	if len(msgs[2].Content) != 60000 {
		t.Fatalf("不应原地修改入参")
	}
}

// 短结果不修剪：stub 与正文差不多大，剪了反而白费。
func TestPruneSkipsShortResults(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: bigContent(40000)}, // 10000 tok，占满保护尾预算之外的旧区
		asstWithCall("call-1", "list_dir"),
		{Role: RoleTool, ToolCallID: "call-1", Content: "done"}, // 短结果
		{Role: RoleUser, Content: "继续"},
	}

	out, count := pruneOldToolResults(msgs, pruneEstimateForTests)
	if count != 0 {
		t.Fatalf("短结果不应被修剪，实际修剪 %d 条", count)
	}
	if out[2].Content != "done" {
		t.Fatalf("短结果应原样保留，实际 %q", out[2].Content)
	}
}

// 保护尾预算内（但位于最后一条 user 消息之前）的工具结果保持原样。
func TestPruneRespectsTailBudget(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "第一问"},
		asstWithCall("call-1", "read_file"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(30000)}, // 7500 tok，预算内
		{Role: RoleUser, Content: "第二问"},
	}

	out, count := pruneOldToolResults(msgs, pruneEstimateForTests)
	if count != 0 {
		t.Fatalf("保护尾预算内的结果不应被修剪，实际修剪 %d 条", count)
	}
	if out[2].Content != msgs[2].Content {
		t.Fatalf("预算内的工具结果应原样保留")
	}
}

// 无 user 消息（continue-only 恢复的历史）时，从末尾按预算保护。
func TestPruneNoUserMessage(t *testing.T) {
	msgs := []Message{
		asstWithCall("call-1", "terminal"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(60000)}, // 15000 tok，超预算
		{Role: RoleAssistant, Content: "结论"},
	}

	out, count := pruneOldToolResults(msgs, pruneEstimateForTests)
	if count != 1 {
		t.Fatalf("应修剪 1 条，实际 %d", count)
	}
	if out[2].Content != "结论" || len(out[0].ToolCalls) != 1 {
		t.Fatalf("尾部消息应保持原样")
	}
}

// 未到触发阈值（压缩阈值的一半）时不改写历史。
func TestPruneToolResultsTriggerThreshold(t *testing.T) {
	const trigger = 2000 // 压缩阈值 4000 的一半
	small := []Message{
		{Role: RoleUser, Content: bigContent(1000)},
		asstWithCall("call-1", "read_file"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(3000)}, // 750 tok，总量低于阈值
	}

	if _, count := PruneToolResults(small, trigger); count != 0 {
		t.Fatalf("低于阈值不应修剪，实际 %d", count)
	}

	// 超阈值但全部落在保护尾预算内：同样不动
	mid := []Message{
		{Role: RoleUser, Content: bigContent(40000)},
		asstWithCall("call-1", "read_file"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(30000)}, // 7500 tok，预算内
	}
	if _, count := PruneToolResults(mid, trigger); count != 0 {
		t.Fatalf("全在保护尾内不应修剪，实际 %d", count)
	}
}

// 幂等：修剪后再修剪，不再产生新变化（stub 本身短于 minPruneChars）。
func TestPruneIdempotent(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "问"},
		asstWithCall("call-1", "read_file"),
		{Role: RoleTool, ToolCallID: "call-1", Content: bigContent(60000)},
		{Role: RoleUser, Content: "再问"},
	}

	once, _ := pruneOldToolResults(msgs, pruneEstimateForTests)
	twice, count := pruneOldToolResults(once, pruneEstimateForTests)
	if count != 0 {
		t.Fatalf("第二次修剪不应有新变化，实际 %d", count)
	}
	for i := range once {
		if once[i].Content != twice[i].Content {
			t.Fatalf("幂等性被破坏，idx=%d", i)
		}
	}
}
