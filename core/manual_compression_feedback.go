package core

// ManualCompressionFeedback 手动压缩反馈，允许外部用户触发压缩并接收结果
type ManualCompressionFeedback struct {
	thresholdTokens int // 触发压缩的 token 阈值
	lastCompressed  int // 上次压缩时的消息数
}

// NewManualCompressionFeedback 创建手动压缩反馈
func NewManualCompressionFeedback(thresholdTokens int) *ManualCompressionFeedback {
	return &ManualCompressionFeedback{
		thresholdTokens: thresholdTokens,
	}
}

// NeedsCompression 检查是否需要压缩
func (mcf *ManualCompressionFeedback) NeedsCompression(tokenCount int) bool {
	return tokenCount > mcf.thresholdTokens
}

// RecordCompression 记录一次压缩
func (mcf *ManualCompressionFeedback) RecordCompression(messageCount int) {
	mcf.lastCompressed = messageCount
}
