package core

// TurnContext 回合上下文数据结构，由 turn prologue 产生供 loop 消费
type TurnContext struct {
	TurnID      string         // 唯一回合标识
	UserMessage string         // 用户原始消息
	Attachments []Attachment   // 本条用户消息携带的图片附件（可空）
	ModelName   string         // 当前模型名
	MaxTokens   int            // 最大输出 token 数
	Temperature float64        // 采样温度
	Extra       map[string]any // 扩展字段
}
