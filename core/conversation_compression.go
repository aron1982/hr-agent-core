package core

// ConversationCompressor 对话级压缩器，在跨回合间压缩消息历史
type ConversationCompressor struct {
	targetTokens int // 目标 token 数
}

// NewConversationCompressor 创建对话压缩器
func NewConversationCompressor(targetTokens int) *ConversationCompressor {
	return &ConversationCompressor{
		targetTokens: targetTokens,
	}
}

// CompressHistory 压缩对话历史，保留关键信息
func (cc *ConversationCompressor) CompressHistory(messages []Message) []Message {
	// 占位实现：保留全量消息
	return messages
}
