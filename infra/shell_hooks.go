package infra

import "sync"

// ShellHook 定义 shell 钩子
// 在 shell 命令执行前后调用自定义逻辑
type ShellHook struct {
	// BeforeCommand 在命令执行前调用
	// 接收原始命令，返回（可能修改后的）命令
	BeforeCommand func(cmd string) string

	// AfterCommand 在命令执行后调用
	// 接收命令、输出和可能发生的错误
	AfterCommand func(cmd string, output string, err error)
}

// 全局钩子注册表
var (
	hooksMu sync.RWMutex
	hooks   []*ShellHook
)

// RegisterShellHook 注册 shell 钩子
// 钩子按注册顺序调用
func RegisterShellHook(hook *ShellHook) {
	if hook == nil {
		return
	}
	hooksMu.Lock()
	defer hooksMu.Unlock()
	hooks = append(hooks, hook)
}

// GetShellHooks 获取所有已注册的钩子
// 返回副本以避免并发修改问题
func GetShellHooks() []*ShellHook {
	hooksMu.RLock()
	defer hooksMu.RUnlock()

	result := make([]*ShellHook, len(hooks))
	copy(result, hooks)
	return result
}

// ClearShellHooks 清除所有已注册的钩子
// 主要用于测试
func ClearShellHooks() {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	hooks = nil
}

// InvokeBeforeHooks 调用所有已注册的 BeforeCommand 钩子
// 链式处理：每个钩子的输出作为下一个钩子的输入
func InvokeBeforeHooks(cmd string) string {
	currentCmd := cmd
	for _, hook := range GetShellHooks() {
		if hook.BeforeCommand != nil {
			currentCmd = hook.BeforeCommand(currentCmd)
		}
	}
	return currentCmd
}

// InvokeAfterHooks 调用所有已注册的 AfterCommand 钩子
func InvokeAfterHooks(cmd string, output string, err error) {
	for _, hook := range GetShellHooks() {
		if hook.AfterCommand != nil {
			hook.AfterCommand(cmd, output, err)
		}
	}
}
