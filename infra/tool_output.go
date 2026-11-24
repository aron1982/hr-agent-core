// 工具增量输出通道。
//
// 有些工具（终端命令）运行时间长、输出持续增长，宿主需要边跑边推给客户端。
// 但工具执行函数签名固定为 (ctx, args) (string, error)，没有额外参数位，
// 因此用 context 作为载体：核心引擎在执行前注入回调，工具侧按需取用。
//
// 没有注入时 EmitToolOutput 是空操作 —— 工具无需判断自己是否被观测。
package infra

import "context"

// ToolOutputEmitter 工具向宿主推送一段增量输出。
type ToolOutputEmitter func(chunk string)

// toolOutputEmitterKey 是 context 中增量输出回调的键。
type toolOutputEmitterKey struct{}

// WithToolOutputEmitter 把增量输出回调放入 context。
func WithToolOutputEmitter(ctx context.Context, emit ToolOutputEmitter) context.Context {
	return context.WithValue(ctx, toolOutputEmitterKey{}, emit)
}

// HasToolOutputEmitter 报告 context 中是否存在增量输出回调。
//
// 工具侧据此选择"边跑边推"还是"一次性缓冲"：无观察者时逐块回调纯属浪费。
func HasToolOutputEmitter(ctx context.Context) bool {
	_, ok := ctx.Value(toolOutputEmitterKey{}).(ToolOutputEmitter)

	return ok
}

// EmitToolOutput 推送一段增量输出；无回调或空串时静默忽略。
func EmitToolOutput(ctx context.Context, chunk string) {
	if chunk == "" {
		return
	}
	if emit, ok := ctx.Value(toolOutputEmitterKey{}).(ToolOutputEmitter); ok {
		emit(chunk)
	}
}
