package core

// PluginLLM 插件 LLM 调用器，用于特定场景的轻量级 LLM 请求
type PluginLLM struct {
	model  string
	apiKey string
}

// NewPluginLLM 创建插件 LLM 调用器
func NewPluginLLM(model, apiKey string) *PluginLLM {
	return &PluginLLM{
		model:  model,
		apiKey: apiKey,
	}
}

// Call 执行一次 LLM 调用
func (p *PluginLLM) Call(prompt string) (string, error) {
	// 占位实现
	_ = prompt
	return "", nil
}
