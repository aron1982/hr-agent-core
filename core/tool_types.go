package core

import (
	"context"
	"fmt"
)

// ToolResult 工具执行结果
type ToolResult struct {
	ToolCallID string // 对应的工具调用 ID
	Name       string // 工具名称
	Content    string // 工具输出内容
	IsError    bool   // 是否为错误结果
	// Images 工具随结果产出的图片，已从 Content 里剥离（见 extractToolImages）。
	//
	// 由 runToolCalls 汇总成一条 user 消息下发 —— role=tool 的 content 只能是
	// 字符串，图片塞不进工具消息本身。
	Images []Attachment
	// EffectDisposition 副作用落地判定（只读工具为 none）。回滚一段历史时
	// 用它判断"这段区间是不是已经改过外部状态"，见 tool_result_classification.go。
	EffectDisposition EffectDisposition
}

// ToolExecutor 工具执行函数签名
type ToolExecutor func(ctx context.Context, args string) (string, error)

// ToolOutputObserver 可选的增量输出观测能力。
//
// 只对"运行时间长、输出持续增长"的工具生效（目前是终端命令）；其余工具
// 一次性返回，走 OnToolComplete 足够。单独定义一个接口而不是给
// ToolObserver 加方法：后者会逼所有既有实现补一个空方法。
type ToolOutputObserver interface {
	// OnToolOutput 工具执行过程中的一段增量输出（stdout/stderr 已合并）。
	OnToolOutput(toolCallID, chunk string)
}

// ToolRegistry 工具注册表，管理已注册的工具及其执行函数
type ToolRegistry struct {
	executors map[string]ToolExecutor
}

// NewToolRegistry 创建工具注册表
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		executors: make(map[string]ToolExecutor),
	}
}

// Register 注册一个工具执行器
func (r *ToolRegistry) Register(name string, executor ToolExecutor) {
	r.executors[name] = executor
}

// Execute 执行指定工具调用，返回工具输出
func (r *ToolRegistry) Execute(ctx context.Context, name string, args string) (string, error) {
	exec, ok := r.executors[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return exec(ctx, args)
}
