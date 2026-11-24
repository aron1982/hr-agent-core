package core

import "sync"

// IterationBudget 线程安全的迭代预算计数器，控制每个 agent 的最大迭代步数。
//
// 语义是**每回合**：计数只在单个回合的工具循环里累加，回合开始由
// runTurn 调用 Reset 归零。此前没有重置（per-Agent 复用同一个实例），
// 长会话累计跑满 MaxIterations 后，后续每个回合都会在预算检查处直接
// 返回空结果 —— 表现为静默空回复且没有 error 事件。
type IterationBudget struct {
	MaxIterations    int // 最大迭代次数
	currentIteration int // 当前已消耗迭代数
	mu               sync.Mutex
}

// NewIterationBudget 创建一个新的迭代预算实例，maxIterations 为上限
func NewIterationBudget(maxIterations int) *IterationBudget {
	if maxIterations <= 0 {
		maxIterations = 90 // 默认上限
	}
	return &IterationBudget{
		MaxIterations: maxIterations,
	}
}

// CanContinue 返回是否还有剩余预算可继续
func (b *IterationBudget) CanContinue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentIteration < b.MaxIterations
}

// IsExhausted 返回预算是否已耗尽
func (b *IterationBudget) IsExhausted() bool {
	return !b.CanContinue()
}

// Consume 消耗一次迭代，返回是否成功
func (b *IterationBudget) Consume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.currentIteration >= b.MaxIterations {
		return false
	}
	b.currentIteration++
	return true
}

// Reset 清零迭代计数（每个回合开始时调用，见 Agent.runTurn）。
func (b *IterationBudget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.currentIteration = 0
}

// Refund 退还一次迭代（如 execute_code 不消耗预算）
func (b *IterationBudget) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.currentIteration > 0 {
		b.currentIteration--
	}
}

// Used 返回已消耗迭代数
func (b *IterationBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentIteration
}

// Remaining 返回剩余迭代数
func (b *IterationBudget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.MaxIterations - b.currentIteration
	if rem < 0 {
		return 0
	}
	return rem
}
