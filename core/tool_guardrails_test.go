package core

import "testing"

// 工具护栏 / 分段规划 / 副作用判定的单元测试。

func testToolCall(name, args string) ToolCallRef {
	ref := ToolCallRef{ID: "call_" + name, Type: "function"}
	ref.Function.Name = name
	ref.Function.Arguments = args

	return ref
}

// TestToolGuardrailWarnsOnRepeatedFailures 连续失败达到阈值后返回告警提示。
func TestToolGuardrailWarnsOnRepeatedFailures(t *testing.T) {
	g := NewToolCallGuardrail(DefaultToolGuardrailConfig())
	g.ResetForTurn()

	// exact_failure_warn_after = 2：第二次同调用失败才告警
	if action, _ := g.AfterCall("read_file", `{"path":"a"}`, "boom", true); action != ToolGuardAllow {
		t.Fatalf("第一次失败不应告警，实际 %v", action)
	}

	action, reason := g.AfterCall("read_file", `{"path":"a"}`, "boom", true)
	if action != ToolGuardWarn || reason == "" {
		t.Fatalf("第二次同调用失败应告警：action=%v reason=%q", action, reason)
	}
}

// TestToolGuardrailBlocksIdenticalLoop 完全相同的调用达到上限后阻断。
func TestToolGuardrailBlocksIdenticalLoop(t *testing.T) {
	cfg := DefaultToolGuardrailConfig()
	cfg.IdenticalCallCap = 3
	g := NewToolCallGuardrail(cfg)
	g.ResetForTurn()

	args := `{"path":"a"}`
	for i := 0; i < 3; i++ {
		if action, _ := g.BeforeCall("read_file", args); action != ToolGuardAllow {
			t.Fatalf("第 %d 次调用不应被阻断", i+1)
		}

		g.AfterCall("read_file", args, "same", false)
	}

	action, reason := g.BeforeCall("read_file", args)
	if action != ToolGuardBlock || reason == "" {
		t.Fatalf("达到调用上限后应阻断：action=%v reason=%q", action, reason)
	}

	// 参数不同不受影响
	if action, _ := g.BeforeCall("read_file", `{"path":"b"}`); action != ToolGuardAllow {
		t.Fatal("参数不同不应被阻断")
	}
}

// TestToolGuardrailHaltsWhenEnabled 打开熔断后同一工具反复失败会结束回合。
func TestToolGuardrailHaltsWhenEnabled(t *testing.T) {
	cfg := DefaultToolGuardrailConfig()
	cfg.HardStopEnabled = true
	cfg.SameToolFailureHaltAfter = 3
	g := NewToolCallGuardrail(cfg)
	g.ResetForTurn()

	last := ToolGuardAllow
	for i := 0; i < 3; i++ {
		last, _ = g.AfterCall("terminal", `{"command":"x"}`, "err", true)
	}
	if last != ToolGuardHalt {
		t.Fatalf("达到熔断阈值应返回 halt，实际 %v", last)
	}
}

// TestToolGuardrailResetsPerTurn 计数每回合清零（不跨回合累计）。
func TestToolGuardrailResetsPerTurn(t *testing.T) {
	cfg := DefaultToolGuardrailConfig()
	cfg.HardStopEnabled = true
	cfg.SameToolFailureHaltAfter = 2
	g := NewToolCallGuardrail(cfg)

	g.ResetForTurn()
	g.AfterCall("terminal", `{"command":"x"}`, "err", true)
	g.AfterCall("terminal", `{"command":"x"}`, "err", true)

	g.ResetForTurn()
	if action, _ := g.AfterCall("terminal", `{"command":"x"}`, "err", true); action == ToolGuardHalt {
		t.Fatal("重置后不应立即熔断")
	}
}

// TestPlanToolBatchSegments 分段规划：连续只读工具成段，其余各自独占一段。
func TestPlanToolBatchSegments(t *testing.T) {
	calls := []ToolCallRef{
		testToolCall("read_file", "{}"),
		testToolCall("list_directory", "{}"),
		testToolCall("terminal", "{}"),
		testToolCall("read_file", "{}"),
	}

	segments := planToolBatchSegments(calls)
	if len(segments) != 3 {
		t.Fatalf("期望 3 段，实际 %d：%v", len(segments), segments)
	}
	if len(segments[0]) != 2 || len(segments[1]) != 1 || len(segments[2]) != 1 {
		t.Fatalf("分段长度异常：%v", segments)
	}
	if segments[0][0] != 0 || segments[0][1] != 1 || segments[1][0] != 2 || segments[2][0] != 3 {
		t.Fatalf("分段下标异常：%v", segments)
	}
}

// TestEffectDisposition 副作用落地判定。
func TestEffectDisposition(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		output string
		isErr  bool
		want   EffectDisposition
	}{
		{"只读工具", "read_file", "content", false, EffectNone},
		{"写入成功", "write_file", `{"bytes_written":12}`, false, EffectLanded},
		{"写入失败但已落盘", "write_file", `{"bytes_written":3}`, true, EffectLanded},
		{"写入失败无落地标记", "write_file", `{"error":"disk full"}`, true, EffectUnknown},
		{"终端命令", "terminal", "done", false, EffectUnknown},
		{"未知工具成功", "ehr_candidate_list", "[]", false, EffectLanded},
	}

	for _, c := range cases {
		if got := ClassifyEffectDisposition(c.tool, c.output, c.isErr); got != c.want {
			t.Errorf("%s：期望 %s 实际 %s", c.name, c.want, got)
		}
	}
}
