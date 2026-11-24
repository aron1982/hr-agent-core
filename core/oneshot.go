package core

// OneShot 单次调用封装，用于不需要对话历史的独立 LLM 请求
type OneShot struct {
	systemPrompt string
	userMessage  string
}

// NewOneShot 创建单次调用
func NewOneShot(systemPrompt, userMessage string) *OneShot {
	return &OneShot{
		systemPrompt: systemPrompt,
		userMessage:  userMessage,
	}
}

// Messages 返回构建好的消息列表
func (o *OneShot) Messages() []Message {
	var msgs []Message
	if o.systemPrompt != "" {
		msgs = append(msgs, Message{
			Role:    RoleSystem,
			Content: o.systemPrompt,
		})
	}
	msgs = append(msgs, Message{
		Role:    RoleUser,
		Content: o.userMessage,
	})
	return msgs
}
