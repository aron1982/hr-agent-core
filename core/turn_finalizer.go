package core

// TurnFinalizer 回合终止决策器，判断当前回合是否应该停止
type TurnFinalizer struct {
	maxToolCallRounds int      // 最大工具调用轮次
	forceStopPatterns []string // 强制终止模式列表
}

// NewTurnFinalizer 创建回合终止决策器
func NewTurnFinalizer() *TurnFinalizer {
	return &TurnFinalizer{
		maxToolCallRounds: 10,
		forceStopPatterns: []string{
			"stop",
			"finish",
			"done",
		},
	}
}

// ShouldStop 判断是否应该停止回合
// 返回 (是否停止, 停止原因)
func (f *TurnFinalizer) ShouldStop(result *TurnResult, budget *IterationBudget) (bool, string) {
	if result == nil {
		return true, "nil_result"
	}

	// 检查预算是否耗尽
	if budget != nil && budget.IsExhausted() {
		return true, "budget_exhausted"
	}

	// 检查 finish_reason 是否为 stop
	if result.FinishReason == "stop" {
		return true, "llm_stop"
	}

	// 检查 finish_reason 是否为 budget_exhausted
	if result.FinishReason == "budget_exhausted" {
		return true, "budget_exhausted"
	}

	// 检查是否有工具调用 — 无工具调用且非 tool_calls 则停止
	if len(result.ToolCalls) == 0 && result.FinishReason != "tool_calls" {
		return true, "no_tool_calls"
	}

	// 检查工具调用轮次是否超限
	if len(result.ToolResults) >= f.maxToolCallRounds {
		return true, "max_tool_rounds"
	}

	return false, ""
}

// SetMaxToolCallRounds 设置最大工具调用轮次
func (f *TurnFinalizer) SetMaxToolCallRounds(n int) {
	if n > 0 {
		f.maxToolCallRounds = n
	}
}

// AddForceStopPattern 添加强制终止模式
func (f *TurnFinalizer) AddForceStopPattern(pattern string) {
	f.forceStopPatterns = append(f.forceStopPatterns, pattern)
}
