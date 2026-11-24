package core

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// MessageRole 消息角色枚举
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// Message 通用消息结构，兼容 OpenAI/Anthropic 等多厂商格式
type Message struct {
	Role       MessageRole   `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []ToolCallRef `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
	// Attachments 本条消息携带的图片附件（当前只有 user 消息使用）。
	//
	// 只存定位信息与元数据，图片字节在构造请求时按需读取并编码成
	// image_url content part（见 buildUserContent）—— 字节若进历史，每轮
	// 请求都要重复搬运数 MB，且 messages 表的全量重写也会跟着付出代价。
	Attachments []Attachment `json:"attachments,omitempty"`

	// CreatedAt 写入历史的 Unix 秒时间戳，0 表示未知（旧库里的历史消息）。
	// 纯展示字段：客户端用它显示气泡时间。不会进模型请求体 ——
	// messagesToOpenAI 是白名单构造，只挑 role / content / tool_calls 等。
	CreatedAt int64 `json:"created_at,omitempty"`

	// DurationMs 该条 assistant 回复的耗时（毫秒），回合结束时由会话层回填。
	// 纯展示字段（客户端在气泡下显示"耗时 12s"），同样不进模型请求体。
	// 0 = 未知：旧库存量消息，或非 assistant 消息。
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Reasoning 该条 assistant 回复的思考链全文（模型的 reasoning_content）。
	// 纯展示字段：客户端在回答上方渲染"思考过程"，历史重载后仍能看到。
	// 不进模型请求体 —— messagesToOpenAI 是白名单构造，只挑 role / content
	// / tool_calls 等。空 = 该轮模型未返回思考，或旧库存量消息。
	Reasoning string `json:"reasoning,omitempty"`
}

// ImageParamMode 图片附件的入参形态，属于渠道级协议差异。
//
// 与 image_routing.go 的 ImageInputMode 是两个维度：后者决定"走原生视觉
// 还是先转文字描述"，这里决定"发出去时用什么形状"。
type ImageParamMode string

const (
	// ImageParamDataURL 标准 data URL（`data:image/png;base64,...`）。零值即此，
	// 因为绝大多数 OpenAI 兼容渠道接受该写法。
	ImageParamDataURL ImageParamMode = ""

	// ImageParamURL 来源本身就是 http(s) 链接，交给上游自行拉取。
	ImageParamURL ImageParamMode = "url"

	// ImageParamBareBase64 裸 base64（智谱 GLM 要求，带 data URL 前缀会报参数错误）。
	ImageParamBareBase64 ImageParamMode = "bare_base64"
)

// Attachment 消息里的一张图片附件。
type Attachment struct {
	// ID 附件在附件库中的标识（由 serve 分配）。core 只透传，不参与请求构造。
	// 落库时只保存它，磁盘绝对路径在加载时现场拼出。
	ID string `json:"id,omitempty"`
	// Source 图片来源：本地文件路径，或 http(s) 链接（Mode 为 URL 时）。
	Source string `json:"source,omitempty"`
	// Name 原始文件名，仅用于展示。
	Name string `json:"name,omitempty"`
	// MIME 图片类型（如 image/png），由落盘方嗅探文件头得到。
	MIME string `json:"mime,omitempty"`
	// Size 图片字节数。
	Size int64 `json:"size,omitempty"`
	// Mode 入参形态；零值 = 标准 data URL。
	Mode ImageParamMode `json:"mode,omitempty"`
}

// imageAttachmentTokens 单张图片的估算 token 数（上下文压缩用）。
//
// 刻意用一个固定值而不是按字节数估算：视觉 token 由图片像素决定，与传输
// 编码（base64 长度）几乎无关，按字节算会让带图会话被过早压缩。
const imageAttachmentTokens = 1000

// buildUserContent 组装一条 user 消息的 content 字段。
//
// 三种结果：
//   - 无附件：返回纯文本（保持字符串形态，不把简单消息变成 parts 数组）；
//   - 有附件且模型支持视觉：返回 parts 数组（文本 part + image_url part）；
//   - 有附件但模型不支持视觉：不发 parts，改为在文本后追加说明 —— 盲发图片
//     会被严格渠道直接 400，而且模型本来也看不见。
func buildUserContent(msg Message, supportsVision bool) any {
	text := msg.Content
	if len(msg.Attachments) == 0 {
		return text
	}

	if !supportsVision {
		// 不写"用户附了"：工具产出的图片（见 appendToolImages）走同一条路径，
		// 那样描述会让模型以为用户传了图，进而得出"用户没给我看图"的错误结论。
		// 这里必须点名视觉能力缺失，让模型能直接告诉用户去开哪个开关。
		return text + fmt.Sprintf(
			"\n\n[附带的 %d 张图片未发送：当前模型未启用图片输入"+
				"（设置 → Providers 中编辑该模型并勾选「图片输入」），模型看不到图片内容]",
			len(msg.Attachments))
	}

	// 用 []any 而非 []map[string]any：content parts 在 JSON 里就是数组，
	// llm 层的渠道差异处理（如 GLM 剥离 data URL 前缀）按 []any 断言，
	// 类型不一致会让那段逻辑静默失效。
	parts := make([]any, 0, len(msg.Attachments)+2)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}

	skipped := 0
	for _, att := range msg.Attachments {
		part, err := imageContentPart(att)
		if err != nil {
			// 单张图片读不出来不能拖垮整轮请求，降级为说明文本。
			skipped++

			continue
		}

		parts = append(parts, part)
	}

	if skipped > 0 {
		parts = append(parts, map[string]any{
			"type": "text",
			"text": fmt.Sprintf("[另有 %d 张图片读取失败，已跳过]", skipped),
		})
	}

	// 图片全部失败且正文为空时补一句兜底，避免发出 content 为空的消息。
	if len(parts) == 0 {
		return "[用户发送了图片，但图片读取失败]"
	}

	return parts
}

// imageContentPart 把一张附件编码成 OpenAI 的 image_url content part。
func imageContentPart(att Attachment) (map[string]any, error) {
	// 链接形态直接透传，不读本地文件。
	if att.Mode == ImageParamURL && isHTTPURL(att.Source) {
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": att.Source},
		}, nil
	}

	data, err := os.ReadFile(att.Source)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("图片内容为空: %s", att.Source)
	}

	encoded := base64.StdEncoding.EncodeToString(data)

	// 智谱 GLM 只接受裸 base64，不能带 data URL 前缀。
	if att.Mode == ImageParamBareBase64 {
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": encoded},
		}, nil
	}

	mime := att.MIME
	if mime == "" {
		mime = "image/png"
	}

	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:" + mime + ";base64," + encoded},
	}, nil
}

// isHTTPURL 判断来源是否为 http(s) 链接。
func isHTTPURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// ToolCallRef 工具调用引用，描述模型发起的一次工具调用
type ToolCallRef struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// FlattenMessageText 从消息内容中提取纯文本（兼容列表型 content 和字符串型 content）
func FlattenMessageText(content any, sep string) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if list, ok := content.([]any); ok {
		var result []byte
		for i, part := range list {
			t := textFromPart(part)
			if t != "" {
				if i > 0 && sep != "" {
					result = append(result, sep...)
				}
				result = append(result, t...)
			}
		}
		return string(result)
	}
	return textFromPart(content)
}

// textFromPart 从消息内容片段中提取文本
func textFromPart(part any) string {
	if part == nil {
		return ""
	}
	if s, ok := part.(string); ok {
		return s
	}
	m, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	// 跳过非文本类型（图片、音频等）
	partType, _ := m["type"].(string)
	switch partType {
	case "image", "image_url", "input_image", "audio", "input_audio":
		return ""
	}
	// 尝试常用文本字段
	for _, key := range []string{"text", "content", "input_text", "output_text", "summary_text"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}
