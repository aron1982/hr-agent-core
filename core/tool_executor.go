package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errNoToolRegistry 未注入工具注册表时的执行错误。
var errNoToolRegistry = errors.New("工具注册表未初始化")

// Invocation 一次工具调用的原始执行结果（未做展示与历史处理）。
//
// 并发执行时上层需要"先并发拿到原始结果、再按原始顺序提交"（见
// ConversationLoop.runToolSegmentConcurrent），因此这里不直接返回 ToolResult
// —— 提交顺序必须与模型给出的 tool_calls 顺序一致。
type Invocation struct {
	Output     string
	Err        error
	DurationMs int64
}

// ExecuteSequential 顺序执行一批工具调用（保序返回）。
func ExecuteSequential(ctx context.Context, registry *ToolRegistry, calls []ToolCallRef) []Invocation {
	out := make([]Invocation, len(calls))
	for i, call := range calls {
		out[i] = invokeOne(ctx, registry, call)
	}

	return out
}

// ExecuteConcurrent 并发执行一批工具调用（保序返回）。
//
// 只用于**只读、无共享状态**的工具 —— 调用方负责筛选（见
// planToolBatchSegments）；有状态工具（terminal / 写文件 / 交互类）并发会
// 产生难以复现的相互干扰。maxParallel <= 1 时退化为串行。
func ExecuteConcurrent(ctx context.Context, registry *ToolRegistry, calls []ToolCallRef, maxParallel int) []Invocation {
	out := make([]Invocation, len(calls))
	if len(calls) == 0 {
		return out
	}
	if maxParallel <= 1 {
		return ExecuteSequential(ctx, registry, calls)
	}

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for i := range calls {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			// 取令牌失败（ctx 取消）时同样写入结果，保证每个下标都有值
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = Invocation{Err: ctx.Err()}

				return
			}

			// 不同下标的切片元素并发写入是安全的（Go 内存模型允许）
			out[i] = invokeOne(ctx, registry, calls[i])
		}(i)
	}
	wg.Wait()

	return out
}

// invokeOne 执行单个工具调用并记录耗时。
func invokeOne(ctx context.Context, registry *ToolRegistry, call ToolCallRef) Invocation {
	if registry == nil {
		return Invocation{Err: errNoToolRegistry}
	}

	start := time.Now()
	output, err := registry.Execute(ctx, call.Function.Name, call.Function.Arguments)

	return Invocation{
		Output:     output,
		Err:        err,
		DurationMs: time.Since(start).Milliseconds(),
	}
}
