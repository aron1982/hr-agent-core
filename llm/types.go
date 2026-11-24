// Package llm 是 hr-buddy 唯一的 LLM 转发实现。
//
// 只支持 OpenAI Chat Completions 协议，不做任何协议转换。所有供应商差异
// （端点、认证头、参数微调、思考字段形状）收敛为声明式配置
// （ChannelProfile / ModelProfile），不写 per-provider 分支代码。
//
// 分层约定：
//
//	core.Agent  → 持有展开后的 Config，不知道 slug / plan / 渠道目录的存在
//	llm         → 请求构造 + 发送 + 流式解析 + 错误分类
//	llm/catalog → 内置渠道目录（只读，随版本升级）
//
// 设计稿见仓库根目录 LLM_LAYER_DESIGN.md。
package llm

// Plan 渠道的套餐类型。
//
// **仅用于 UI 展示与用途提示，不参与任何计算。** 本项目不计费，因此没有
// 价格字段，也不产出成本数字。
//
// 之所以仍要区分：同一个 deepseek-v3 在按量渠道与 Coding Plan 渠道下，
// 可用模型清单不同、使用条款不同，UI 需要如实告知用户。
type Plan string

const (
	PlanPayAsYouGo Plan = "payg"        // 按量（后付费）
	PlanCodingPlan Plan = "coding_plan" // 订阅 · 编码工具
	PlanAgentPlan  Plan = "agent_plan"  // 订阅 · agent / 多模态
	PlanOther      Plan = "other"       // 其他订阅形式
)

// ReasoningShape 供应商思考（reasoning）参数的字段形状。
//
// 各家的表达方式不统一，只能由渠道声明后用同一个 switch 处理，避免在
// 转发路径里散落 provider 名判断。
type ReasoningShape string

const (
	// ReasoningNone 不注入任何思考参数，由模型使用自身默认行为。
	ReasoningNone ReasoningShape = ""

	// ReasoningModelNameSuffix 以模型名后缀表达（如 DeepSeek 的
	// `deepseek-reasoner-thinking`）。转发时剥掉后缀并在请求体加 thinking 字段。
	ReasoningModelNameSuffix ReasoningShape = "model_name_suffix"

	// ReasoningExtraBodyThinking 写入 extra_body.thinking（Kimi / Moonshot）。
	ReasoningExtraBodyThinking ReasoningShape = "extra_body_thinking"

	// ReasoningExtraBodyEnable 写入 extra_body.enable_thinking（Qwen / DashScope）。
	ReasoningExtraBodyEnable ReasoningShape = "extra_body_enable"

	// ReasoningTopLevelThinking 写入顶层 thinking（火山方舟）。
	ReasoningTopLevelThinking ReasoningShape = "top_level_thinking"
)

// FieldPolicy 未知字段策略。
//
// 这是"OpenAI 兼容"的分水岭：严格厂商对未知字段返回 400，宽松厂商静默忽略。
type FieldPolicy string

const (
	// FieldPolicyLenient 原样透传（默认，兼容性最好）。
	FieldPolicyLenient FieldPolicy = ""

	// FieldPolicyStrict 请求前剥离非白名单字段。
	FieldPolicyStrict FieldPolicy = "strict"
)

// ModelProfile 模型级协议差异。零值 = 标准行为。
type ModelProfile struct {
	// TemperatureFixed 大于 0 时强制 temperature 为该值（例：kimi-k2.6 必须 1.0）。
	TemperatureFixed float64

	// OmitTemperature 为 true 时不发送 temperature（例：Kimi K3 由服务端管理）。
	OmitTemperature bool
}

// ChannelProfile 渠道级协议差异，全部是"标准 OpenAI"的偏离项。
// 零值 = 标准行为，因此绝大多数渠道这段是空的。
type ChannelProfile struct {
	// TopPMax 大于 0 时钳制 top_p 上限（例：GLM 需要 <= 0.99）。
	TopPMax float64

	// StripImageDataURLPrefix 剥离图片 base64 的 `data:image/...;base64,` 前缀
	// （GLM 要求，否则报参数错误）。
	StripImageDataURLPrefix bool

	// MaxTokensField 输出上限的键名。空 = "max_tokens"；
	// "max_completion_tokens" 用于 OpenAI o1/gpt-5 系。
	MaxTokensField string

	// ReasoningShape 思考参数的字段形状。
	ReasoningShape ReasoningShape

	// ReasoningSplit 要求上游把思考内容「拆」到独立字段返回（MiniMax 的
	// reasoning_split）。不拆时思考内容混在 content 里带 <think> 标签，
	// 会被当成正文展示给用户。
	//
	// 注意与 ReasoningShape 的区别：ReasoningShape 管「思考开不开、开关
	// 叫什么」，本字段管「思考内容怎么返回」—— 它不影响模型是否思考。
	ReasoningSplit bool

	// ThinkingOnValue 「开启思考」时写入的具体取值，空 = "enabled"。
	//
	// 取值各家不统一，且**不是同义词**：MiniMax 只接受 "adaptive" / "disabled"，
	// 发 "enabled" 直接 400（invalid thinking.type）。因此开启值必须可按系列声明。
	ThinkingOnValue string

	// UnknownFieldPolicy 未知字段策略。
	UnknownFieldPolicy FieldPolicy

	// StreamIncludeUsage 流式请求时下发 stream_options.include_usage=true。
	//
	// 标准 OpenAI 默认就在流式尾帧带 usage，但 MiniMax 不是：它默认每帧
	// "usage": null，必须显式要求才下发 usage 帧（该帧 choices 为空数组）。
	// 不开这个开关时 usage 恒为 0，缓存命中率统计随之全部失真。
	//
	// 零值 = 不下发（标准行为），因此其余渠道不受影响。
	StreamIncludeUsage bool
}

// ModelCaps 模型能力。用于上下文预算与请求前的可行性校验。
type ModelCaps struct {
	MaxInputTokens   int  // 上下文窗口
	MaxOutputTokens  int  // 单次输出上限
	SupportsTools    bool // 是否支持工具调用
	SupportsVision   bool // 是否接受图片输入
	SupportsThinking bool // 是否支持思考
}

// ChannelModel 渠道上的一个模型条目。
//
// 注意：能力挂在 (渠道 × 模型) 上，而不是挂在模型名上 —— 同一个
// deepseek-v3 在官方、火山方舟、腾讯云三处的上下文长度与参数支持都可能不同。
type ChannelModel struct {
	// ModelID 该平台实际使用的 model id（发请求时用这个）。
	ModelID string

	// Aliases 用户可能输入的其他写法，用于匹配。
	Aliases []string

	// Caps 该模型在此渠道上的能力。
	Caps ModelCaps

	// Profile 模型级差异，覆盖渠道级。
	Profile ModelProfile
}

// Channel 一个接入点（= 一个 base_url + 一套认证 + 一组可用模型）。
//
// 同一厂商的按量与订阅计划是不同的 Channel：base_url 不同、API Key 不同、
// 可用模型不同、条款不同。
type Channel struct {
	Slug   string // 内置标识，如 "glm-coding-cn"
	Name   string // 展示名，如 "智谱 GLM Coding Plan"
	Vendor string // 厂商，如 "zhipu"，UI 按此分组
	Region string // "cn" | "intl"
	Plan   Plan   // 套餐标签（仅展示）

	// BaseURL 必须写到版本段（/v1、/v3、/v4），转发时仅追加 "/chat/completions"。
	// 不要在代码里猜测补 /v1 —— 各家路径层级不同（智谱 /api/paas/v4、
	// 火山 /api/v3），猜测必然拼错。
	BaseURL string

	// PlanNote UI 用途提示，如"包月 · 仅限编码工具使用"。
	PlanNote string

	// ExtraHeaders 部分订阅端点要求特定客户端标识才放行（否则 403）。
	// 该标识同时关系到套餐条款合规，不要替换成伪造值。
	ExtraHeaders map[string]string

	// Profile 平台级协议差异。
	Profile ChannelProfile

	Models []ChannelModel
}

// Config 是完全展开后的运行时配置，由 (内置目录 + 用户覆盖) 合并而来。
// core.Agent 只见这一层，不知道 slug / plan / 目录的存在。
type Config struct {
	BaseURL string
	APIKey  string
	Model   string // 已应用别名映射后的实际 model id

	MaxInputTokens  int
	MaxOutputTokens int
	Caps            ModelCaps

	ExtraHeaders map[string]string
	Profile      ChannelProfile // 已合并模型级覆盖
	ModelProfile ModelProfile

	// ExtraBody 用户自定义扩展字段，最高优先级，最后合并进请求体。
	ExtraBody map[string]any

	// Thinking 思考开关。nil = 不干预（由模型自身默认行为决定）。
	Thinking *bool
}

// Usage token 用量。
//
// 本项目不计费 —— usage 只用于两个真实用途：上下文管理（prompt_tokens 是
// 压缩触发与预算判断的输入）与前端展示。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int // 由各渠道的差异字段合并而来
}

// ToolCall 标准化的工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON 字符串
}

// Result 一次 LLM 调用的结果。
type Result struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            Usage
}
