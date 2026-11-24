package core

import "strings"

// Trajectory 轨迹追踪器，记录 agent 每步的操作与结果
type Trajectory struct {
	Steps []TrajectoryStep
}

// TrajectoryStep 单步轨迹记录
type TrajectoryStep struct {
	TurnID   string         // 回合 ID
	Action   string         // 操作名称（如 "llm_call", "tool_call"）
	Input    map[string]any // 操作输入
	Output   map[string]any // 操作输出
	Duration int64          // 耗时（毫秒）
}

// AddStep 追加一个轨迹步骤
func (t *Trajectory) AddStep(step TrajectoryStep) {
	t.Steps = append(t.Steps, step)
}

// HasIncompleteScratchpad 检查轨迹中是否存在未闭合的 <REASONING_SCRATCHPAD> 标签
func (t *Trajectory) HasIncompleteScratchpad() bool {
	for _, step := range t.Steps {
		content, _ := step.Output["content"].(string)
		if hasIncompleteScratchpad(content) {
			return true
		}
	}
	return false
}

// hasIncompleteScratchpad 检查文本中是否有未闭合的 REASONING_SCRATCHPAD 标签
func hasIncompleteScratchpad(content string) bool {
	return strings.Contains(content, "<REASONING_SCRATCHPAD>") &&
		!strings.Contains(content, "</REASONING_SCRATCHPAD>")
}

// ConvertScratchpadToThink 将 <REASONING_SCRATCHPAD> 标签转换为 <think> 标签
func ConvertScratchpadToThink(content string) string {
	if !strings.Contains(content, "<REASONING_SCRATCHPAD>") {
		return content
	}
	s := strings.ReplaceAll(content, "<REASONING_SCRATCHPAD>", "<think>")
	return strings.ReplaceAll(s, "</REASONING_SCRATCHPAD>", "</think>")
}
