package core

import (
	"fmt"
	"strings"
)

// defaultImageHistoryTurns 历史里保留原图（像素）的用户轮数。
//
// 取 2 的取舍：追问几乎总是围绕最近一两轮的图，更早的图即便重发也很少被
// 引用；而每多留一轮就多几百 KB 上行与上千视觉 token，且这些成本每轮都会
// 重付一次。
const defaultImageHistoryTurns = 2

// imageOmittedFormat 旧图被省略时写进正文的标记。
const imageOmittedFormat = "[图片已省略：%s]"

// degradeOldImages 返回"发给模型用"的消息副本：超出窗口的历史图片只留一行
// 说明，不再携带像素。
//
// 为什么需要：带图对话每一轮都要把历史图片重新编码进请求（数 MB 上行、每张
// 上千视觉 token），聊得越久越慢 —— 这是"发 URL 还是发 base64"都绕不开的
// 乘法成本。
//
// 刻意**不回写对话历史**：前端气泡仍要显示原图，用户回看旧对话也不能缺图。
// 这里只改发给模型的副本，且是函数式的 —— 同一份历史多次调用结果一致，
// 否则每轮都会改写前缀，prompt cache 会一直失效。
func degradeOldImages(messages []Message, keepTurns int) []Message {
	if keepTurns < 0 {
		keepTurns = 0
	}
	if len(messages) == 0 {
		return messages
	}

	// 从后往前数 user 轮次，定位窗口边界：下标 < boundary 的属于旧轮
	boundary := 0
	seen := 0

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != RoleUser {
			continue
		}

		seen++
		if seen > keepTurns {
			boundary = i + 1

			break
		}
	}

	// 收集需要降级的消息；没有就原样返回（无附件的历史不做任何分配）
	var targets []int
	for i := 0; i < boundary; i++ {
		if len(messages[i].Attachments) > 0 {
			targets = append(targets, i)
		}
	}
	if len(targets) == 0 {
		return messages
	}

	out := make([]Message, len(messages))
	copy(out, messages)

	for _, i := range targets {
		names := make([]string, 0, len(out[i].Attachments))
		for _, att := range out[i].Attachments {
			names = append(names, attachmentDisplayName(att))
		}

		// 只动副本：原历史的 Attachments 与 Content 保持原样
		out[i].Attachments = nil
		out[i].Content = appendImageOmittedNotice(out[i].Content, names)
	}

	return out
}

// imageHistoryLimit 归一化配置值：0 表示用默认轮数，负数表示全部降级。
func (a *Agent) imageHistoryLimit() int {
	switch {
	case a.imageHistoryTurns == 0:
		return defaultImageHistoryTurns
	case a.imageHistoryTurns < 0:
		return 0
	default:
		return a.imageHistoryTurns
	}
}

// attachmentDisplayName 附件在占位标记里的显示名（优先原始文件名）。
func attachmentDisplayName(att Attachment) string {
	if name := strings.TrimSpace(att.Name); name != "" {
		return name
	}
	if att.ID != "" {
		return att.ID
	}

	return "图片"
}

// appendImageOmittedNotice 把"图片已省略"标记附到正文后。
//
// 纯图片消息降级后正文为空，此时标记本身就是正文 —— 不能返回空字符串，
// 否则那条消息会变成 content 为空的 user 消息，严格端点会直接拒绝。
func appendImageOmittedNotice(content string, names []string) string {
	notice := fmt.Sprintf(imageOmittedFormat, strings.Join(names, "、"))
	if strings.TrimSpace(content) == "" {
		return notice
	}

	return content + "\n\n" + notice
}
