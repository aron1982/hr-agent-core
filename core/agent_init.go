// Agent 核心结构体定义和初始化入口。
//
// 封装所有 HrBuddy Agent 运行时组件（凭据、传输、对话循环、记忆、压缩等），
// 提供统一的创建和配置接口。

package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aron1982/hr-agent-core/auth"
	"github.com/aron1982/hr-agent-core/infra"
	"github.com/aron1982/hr-agent-core/llm"
)

// Agent 是 HrBuddy Agent 的核心结构体，聚合所有运行时组件
type Agent struct {
	// 基础配置
	ModelName   string
	MaxTokens   int
	Temperature float64
	BaseURL     string
	APIKey      string
	// ExtraBody 供应商私有的请求体扩展字段，发请求时合并进顶层 body
	// （见 callLLM；AgentConfig.ExtraBody 传入）
	ExtraBody map[string]any

	// 系统提示词按三段式组织，只有 volatile 段会变：
	//
	//	stable   身份 + 工具指导（构造后不变）
	//	context  会话级上下文（当前为空，预留）
	//	volatile 会话起始日期 / 模型行 + 记忆快照（SetMemory 更新）
	//
	// 变化的内容一律进 volatile 段，上游的 prefix cache 就只需为后缀付费
	// （对齐 hermes-agent 的 stable→context→volatile 段序设计）。
	// promptFull 是拼装结果缓存，避免每个回合重复 Join。
	promptMu     sync.RWMutex
	promptParts  *SystemPrompt
	promptMeta   []string // volatile 的固定项：会话起始日期、模型/供应商
	promptMemory string   // volatile 的记忆快照
	promptFull   string

	// 核心组件
	CredentialPool *auth.CredentialPool
	// LLM 是唯一的转发客户端（只支持 OpenAI Chat Completions）。
	// 供应商差异已由 serve 层解析为 llm.Config，core 内不再有 provider 分支。
	LLM *llm.Client
	// llmConfig 转发客户端的配置副本：凭证轮换后用它重建客户端。
	llmConfig llm.Config
	// maxRetries 单次 LLM 调用的重试上限（0 = 不重试）。
	maxRetries   int
	Conversation *ConversationLoop
	Memory       *MemoryManager
	// compressMaxTokens 压缩阈值（模型窗口 × 2/3），随 agent 配置固定。
	compressMaxTokens int
	// store 压缩状态容器：Coordinator 为空（CLI / REPL）时用这一份；
	// serve 注入 Coordinator 后以会话容器的指针为准。
	store atomic.Pointer[CompactStore]
	// Coordinator 压缩执行权协调（serve 注入；nil = 单进程直连，不协调）。
	// 同一会话同时只允许一个压缩在跑，避免状态被并发覆盖。
	Coordinator CompactCoordinator
	Budget      *IterationBudget

	// 工具相关
	ToolRegistry *ToolRegistry
	ToolSchemas  []map[string]any // LLM API 工具定义
	Guardrail    *ToolCallGuardrail

	// 历史观察钩子（可选）：每次调用 LLM 前触发，携带当前完整历史副本。
	// 供持久化层在上下文压缩前归档原文（压缩会改写消息，之后拿不到原文）。
	// 未注册时零开销 —— 桌面/REPL 路径不注册。
	hookMu   sync.RWMutex
	turnHook func([]Message)

	// imageHistoryTurns 发给模型时历史保留原图的用户轮数（见 degradeOldImages）。
	// 0 = 用默认值，负数 = 全部降级为占位文本。
	imageHistoryTurns int

	// Provider 渠道标识（slug），仅用于展示与凭据查找，不参与协议选择。
	Provider string

	// HTTP 客户端
	HTTPClient *http.Client

	// 日志
	Logger *slog.Logger
}

// AgentConfig Agent 初始化配置。
//
// 协议差异（ExtraHeaders / ChannelProfile / ModelProfile / Caps）由 serve 层
// 从渠道目录解析后传入，零值即"标准 OpenAI 行为"。
type AgentConfig struct {
	ModelName     string  // 模型名称
	MaxTokens     int     // 最大输出 token 数
	Temperature   float64 // 采样温度
	MaxIterations int     // 最大迭代次数
	// MaxRetries 单次 LLM 调用的重试上限：0 = 默认 3 次，负数 = 关闭重试。
	MaxRetries    int    // 失败重试上限
	ContextWindow int    // 上下文窗口大小
	Provider      string // 渠道标识（slug），仅用于展示与凭据查找
	SystemPrompt  string // 自定义系统提示词
	BaseURL       string // API 基础地址，需写到版本段（如 https://api.deepseek.com/v1）
	APIKey        string // API 密钥

	// ExtraBody 供应商私有的请求体扩展字段，发请求时合并进顶层 body，
	// 跳过 model/messages/tools 三个结构性键，其余覆盖内置默认值。
	ExtraBody map[string]any // 请求体扩展字段（可选）

	// 以下由渠道目录解析后传入（见 agent/llm/catalog）。
	ExtraHeaders   map[string]string  // 渠道要求的额外请求头
	ChannelProfile llm.ChannelProfile // 渠道级协议差异
	ModelProfile   llm.ModelProfile   // 模型级协议差异
	Caps           llm.ModelCaps      // 模型能力
	Thinking       *bool              // 思考开关，nil = 不干预

	// Logger 可选：为 nil 时用进程默认 logger。协作子 agent 用它绑定自己的
	// agent_id —— 多个子 agent 并发时，否则日志无法归因到具体子任务。
	Logger *slog.Logger

	// ImageHistoryTurns 发给模型时历史保留图片原形态的用户轮数。
	// 0 = 用默认值（2 轮）；负数 = 全部降级为占位文本。
	//
	// 只影响"发给模型的请求"，不改变对话历史本身（前端仍显示原图）。
	ImageHistoryTurns int

	Tools       map[string]ToolExecutor // 初始工具注册表
	ToolSchemas []map[string]any        // LLM API 工具定义
}

// defaultBaseURL 未配置任何渠道时的兜底端点（含 /v1 完整前缀）。
//
// 必须写完整前缀：转发层只追加 "/chat/completions"，不做任何版本段猜测
// —— 各家路径层级不同（智谱 /api/paas/v4、火山 /api/v3、DashScope
// /compatible-mode/v1），任何启发式都会拼错其中一部分。
const defaultBaseURL = "https://api.deepseek.com/v1"

// 回合内 LLM 调用的重试策略默认值。
//
// 对齐 hermes-agent：max_retries 默认 3、指数退避 2s 起、60s 上限、去相关
// 抖动；上游给了 Retry-After 时以它为准（llm 层已把离谱值限到 10 分钟）。
const (
	defaultMaxRetries = 3
	retryBaseDelay    = 2 * time.Second
	retryMaxDelay     = 60 * time.Second
	retryJitter       = 0.2

	// llmStreamIdleTimeout 流式 LLM 调用的空闲上限：连续这么久没有任何
	// 增量（正文/思考/工具调用片段）就判定连接停滞并取消本次调用。
	// 没有它，provider 端「挂着连接不发数据也不报错」的停滞会永久挂住
	// 回合 —— 重试逻辑只在出错时触发，读停滞不触发。
	llmStreamIdleTimeout = 15 * time.Minute
)

// errLLMStreamIdle 空闲看门狗触发的哨兵错误（errors.Is 可判）。
var errLLMStreamIdle = errors.New("LLM 流式响应长时间无输出，已取消本次调用")

// llmNonStreamTimeout 非流式 LLM 调用的请求上限。
//
// 流式调用有 idle 看门狗兜底（llmStreamIdleTimeout），非流式没有增量信号、
// 看门狗刻意不启用；而 transport 的 ResponseHeaderTimeout 只覆盖**首字节** ——
// 上游「已回响应头、随后 body 停摆既不报错也不断开」时 io.ReadAll 会永久阻塞，
// 只能等协作子 agent 的 30 分钟墙钟（实测发生过：一个子 agent 静默挂住，
// 日志既无 LLM 调用完成也无任何错误）。这里给非流式请求一个整体上限，
// 到点按「连接停滞」立即失败，交由上层重派/上报。
//
// 用 var 而非 const：测试要把它调小来验证超时链路。
var llmNonStreamTimeout = 5 * time.Minute

// errLLMNonStreamTimeout 非流式调用停滞的哨兵错误。
// 与 errLLMStreamIdle 同类：命中后不再重试（同一 provider 大概率再次停滞）。
var errLLMNonStreamTimeout = errors.New("LLM 非流式响应停滞（超过请求上限），已取消本次调用")

// defaultHTTPTransport 进程级共享的 HTTP 传输（所有 Agent 复用同一连接池）。
//
// 此前每个 Agent 都是裸 &http.Client{}（默认 transport、零超时），模型端
// 挂起不响应时回合会永久卡住 —— serve 的回合 ctx 没有 deadline。
// 这里只设连接建立/首字节类超时，**不设 http.Client.Timeout**：后者覆盖
// 整个请求（含流式响应体读取），会打断长回答。
var defaultHTTPTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	// 首字节上限：给推理/思考模型留足时间，同时避免无限等待
	ResponseHeaderTimeout: 5 * time.Minute,
}

// NewAgent 创建并初始化 Agent 实例
func NewAgent(ctx context.Context, cfg AgentConfig) (*Agent, error) {
	// 1. 创建凭据池
	pool := auth.NewCredentialPool()

	// 2. 创建核心组件
	budget := NewIterationBudget(cfg.MaxIterations)
	tools := NewToolRegistry()
	guardrail := NewToolCallGuardrail(DefaultToolGuardrailConfig())

	// 3. 注册用户提供的工具
	for name, fn := range cfg.Tools {
		tools.Register(name, fn)
	}

	// 4. 构建系统提示词（三段式，见 Agent.promptParts 的说明）
	memory := NewMemoryManager()
	identity := cfg.SystemPrompt
	if identity == "" {
		// core 自己兜底：调用方（CLI / 网关）漏传时不能生成空身份
		identity = DefaultAgentIdentity
	}
	// 工具指导只在确实挂了工具时注入 —— 没工具却讲工具怎么用会误导模型
	stableParts := []string{identity}
	if len(cfg.Tools) > 0 || len(cfg.ToolSchemas) > 0 {
		stableParts = append(stableParts, ToolGuidance)
	}
	promptParts := NewSystemPrompt(stableParts, nil, nil)
	promptMeta := promptMetaItems(cfg)
	promptMemory := memory.Snapshot()
	promptParts.Volatile = buildVolatilePrompt(promptMeta, promptMemory)

	// 5. 创建对话循环
	convLoop := NewConversationLoop(budget, tools, guardrail)

	// 6. API Key 优先用直接传入的，其次从凭据池获取
	apiKey := cfg.APIKey
	if apiKey == "" {
		if cred, err := pool.GetCredential(cfg.Provider); err == nil {
			apiKey = cred.Value
		}
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	httpClient := &http.Client{Transport: defaultHTTPTransport}
	logger := slog.Default()
	if cfg.Logger != nil {
		// 协作子 agent 等场景注入带 agent_id 的 logger，便于日志归因
		logger = cfg.Logger
	}

	// 7. 组装唯一的转发客户端。供应商差异全部来自 cfg 上已解析好的协议字段，
	// 这里不做任何 provider 分支 —— 新增供应商只需要目录加一条数据。
	llmConfig := llm.Config{
		BaseURL:         baseURL,
		APIKey:          apiKey,
		Model:           cfg.ModelName,
		MaxInputTokens:  cfg.Caps.MaxInputTokens,
		MaxOutputTokens: cfg.Caps.MaxOutputTokens,
		Caps:            cfg.Caps,
		ExtraHeaders:    cfg.ExtraHeaders,
		Profile:         cfg.ChannelProfile,
		ModelProfile:    cfg.ModelProfile,
		ExtraBody:       cfg.ExtraBody,
		Thinking:        cfg.Thinking,
	}
	client := llm.NewClient(llmConfig, httpClient, logger)

	// 8. 重试策略：0 = 默认 3 次；负数 = 显式关闭重试。
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultMaxRetries
	}
	if maxRetries < 0 {
		maxRetries = 0
	}

	agent := &Agent{
		ModelName:      cfg.ModelName,
		MaxTokens:      cfg.MaxTokens,
		Temperature:    cfg.Temperature,
		BaseURL:        baseURL,
		APIKey:         apiKey,
		CredentialPool: pool,
		LLM:            client,
		llmConfig:      llmConfig,
		maxRetries:     maxRetries,
		Conversation:   convLoop,
		Memory:         memory,
		compressMaxTokens: compressThresholdFor(cfg),
		Budget:         budget,
		ToolRegistry:   tools,
		ToolSchemas:    cfg.ToolSchemas,
		Guardrail:      guardrail,
		// 历史图片窗口：只影响发给模型的请求，不改变历史本身
		imageHistoryTurns: cfg.ImageHistoryTurns,
		Provider:       cfg.Provider,
		ExtraBody:      cfg.ExtraBody,
		promptParts:    promptParts,
		promptMeta:     promptMeta,
		promptMemory:   promptMemory,
		promptFull:     promptParts.Full(),
		HTTPClient:     httpClient,
		Logger:         logger,
	}
	// 压缩状态容器：CLI / REPL 路径用它；serve 装配时注入 Coordinator，
	// 改以会话容器为准。摘要通道直接用 agent.compactSummarize（随模型走）。
	agent.store.Store(NewCompactStore())

	return agent, nil
}

// Run 启动 Agent 对话回合，支持多步工具调用循环（一次性返回最终回复）。
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	reply, err := a.runTurn(ctx, userInput, nil, nil, false)
	return reply, err
}

// RunWithAttachments 与 Run 相同，但把图片附件挂在本条用户消息上。
func (a *Agent) RunWithAttachments(ctx context.Context, userInput string, attachments []Attachment) (string, error) {
	reply, err := a.runTurn(ctx, userInput, attachments, nil, false)
	return reply, err
}

// RunStream 以流式运行一个对话回合。
//
// onChunk 非 nil 时，LLM 文本块**即时**回调（真流式）；工具中间轮的文本
// 不是最终回复，会先转发后在判定为工具轮时回调 onReset 撤回（客户端收到
// 后清空已显示内容）。token 用量跨整回合汇总返回。
func (a *Agent) RunStream(ctx context.Context, userInput string, onChunk func(string), onReasoning func(string), onReset func()) (string, TokenUsage, error) {
	var usage TokenUsage
	reply, err := a.runTurn(ctx, userInput, nil, &streamCollector{onText: onChunk, onReasoning: onReasoning, onReset: onReset, usage: &usage}, false)
	return reply, usage, err
}

// RunStreamWithAttachments 与 RunStream 相同，但把图片附件挂在本条用户消息上。
func (a *Agent) RunStreamWithAttachments(ctx context.Context, userInput string, attachments []Attachment, onChunk func(string), onReasoning func(string), onReset func()) (string, TokenUsage, error) {
	var usage TokenUsage
	reply, err := a.runTurn(ctx, userInput, attachments, &streamCollector{onText: onChunk, onReasoning: onReasoning, onReset: onReset, usage: &usage}, false)
	return reply, usage, err
}

// SetSuspendingTools 配置终结型工具（仅推进任务会话装配，见 serve 的 guard）。
// 调用这类工具后本回合即结束，该轮正文与工具调用拆成两条消息落库：正文按
// 正文呈现，工具痕迹留给下一轮模型。
func (a *Agent) SetSuspendingTools(names ...string) {
	if a.Conversation != nil {
		a.Conversation.SetSuspendingTools(names...)
	}
}

// SetNoteSuspendingTools 配置"改写为正文"的终结型工具（推进任务的
// guard_finish）：工具输出落成一条 assistant 消息，不保留工具卡。
func (a *Agent) SetNoteSuspendingTools(names ...string) {
	if a.Conversation != nil {
		a.Conversation.SetNoteSuspendingTools(names...)
	}
}

// Messages 返回当前会话消息的副本。
//
// 供归档层与外部宿主读取；调用方修改返回值不影响会话历史。
func (a *Agent) Messages() []Message {
	if a.Conversation == nil {
		return nil
	}

	return cloneMessages(a.Conversation.GetMessages())
}

// SetTurnHook 注册历史观察钩子，传 nil 注销。
//
// 触发时机：每次调用 LLM 前（早于上下文压缩），携带当前完整消息序列的副本。
// 持久化层据此归档"压缩前原文"，实现与 serve 路径一致的归档式压缩。
func (a *Agent) SetTurnHook(fn func([]Message)) {
	a.hookMu.Lock()
	a.turnHook = fn
	a.hookMu.Unlock()
}

// notifyTurnHook 触发历史观察钩子；未注册时零开销。
func (a *Agent) notifyTurnHook(messages []Message) {
	a.hookMu.RLock()
	fn := a.turnHook
	a.hookMu.RUnlock()

	if fn != nil {
		fn(cloneMessages(messages))
	}
}

// RunContinue 在已有历史基础上继续一个回合（不追加用户消息）。
// 供"重新生成"使用：调用前历史尾部应是待重跑的那条用户消息。
func (a *Agent) RunContinue(ctx context.Context) (string, error) {
	reply, err := a.runTurn(ctx, "", nil, nil, true)
	return reply, err
}

// RunStreamContinue 以流式继续一个回合（不追加用户消息），语义同 RunContinue。
func (a *Agent) RunStreamContinue(ctx context.Context, onChunk func(string), onReasoning func(string), onReset func()) (string, TokenUsage, error) {
	var usage TokenUsage
	reply, err := a.runTurn(ctx, "", nil, &streamCollector{onText: onChunk, onReasoning: onReasoning, onReset: onReset, usage: &usage}, true)
	return reply, usage, err
}

// streamCollector 回合内 LLM 文本流收集器。
//
// 文本块即时转发（真流式）；onReasoning 思考链同样即时转发。
// 工具轮的文本不构成最终回复：runTurn 在该轮判定出 tool_calls 后回调
// onReset，由客户端清空已显示内容 —— 撤回语义取代了原先的"先缓冲、
// 回合末批量转发"（后者让正文变成回合末一次性到达，失去流式观感）。
type streamCollector struct {
	onText func(string) // 非 nil 时启用流式（transport 支持时走 SSE）
	// onReasoning 思考链回调，即时转发不缓冲
	onReasoning func(string)
	// onReset 工具轮撤回回调：本轮已转发的文本作废，客户端应清空显示
	onReset func()
	usage   *TokenUsage // 累加整回合所有 LLM 调用的 token 用量
}

// runTurn 是 Run/RunStream/RunContinue 共用的回合主循环。
//
// attachments 为本次用户消息携带的图片附件（可空）。
//
// continueOnly 为 true 时跳过"追加用户消息"这一步，直接用已有历史继续调用
// LLM —— "重新生成"用它重跑最后一轮：此时历史尾部已经是那条用户消息，若
// 再走 RunTurn 会在历史里多出一条重复的用户消息。
func (a *Agent) runTurn(ctx context.Context, userInput string, attachments []Attachment, sc *streamCollector, continueOnly bool) (string, error) {
	// 预算是「每回合」语义：进入回合先归零。否则 per-Agent 的计数器会在
	// 长会话里累计跑满 MaxIterations，此后每个回合都在预算检查处直接
	// 返回空结果（静默空回复，且不发 error 事件）。
	if a.Budget != nil {
		a.Budget.Reset()
	}

	turnCtx := &TurnContext{
		TurnID:      generateTurnID(),
		UserMessage: userInput,
		Attachments: attachments,
		ModelName:   a.ModelName,
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
	}

	// 当前 LLM 轮的文本块即时转发（真流式）；工具轮文本在本轮返回
	// tool_calls 后经 onReset 撤回。
	//
	// reasoningBuf 按"单次 LLM 调用"累积思考链：调用返回后写到刚追加的
	// assistant 消息上（见 SetLastAssistantReasoning），由会话层随历史落库。
	var reasoningBuf strings.Builder
	llmCall := func(messages []Message) (string, []ToolCallRef, error) {
		// 归档钩子必须在压缩前触发：压缩会改写消息，之后就拿不到原文了
		a.notifyTurnHook(messages)

		prepared := a.prepareMessages(ctx, messages)

		if sc == nil {
			return a.callLLMWithRetry(ctx, prepared, nil, nil, nil, a.ToolSchemas)
		}

		reasoningBuf.Reset()
		onReasoning := func(chunk string) {
			reasoningBuf.WriteString(chunk)
			if sc.onReasoning != nil {
				sc.onReasoning(chunk)
			}
		}
		text, calls, callErr := a.callLLMWithRetry(ctx, prepared, func(chunk string) {
			if sc.onText != nil {
				sc.onText(chunk)
			}
		}, onReasoning, sc.usage, a.ToolSchemas)
		// 工具轮：本轮已转发的文本不构成最终回复，通知客户端撤回。
		// 终结型工具（推进任务的 wait）例外：它就是本轮的交付内容，撤回会
		// 把刚讲完的正文收进过程区（2026-11-20：《看秤》被折叠的问题）。
		if callErr == nil && len(calls) > 0 && sc.onReset != nil &&
			!(a.Conversation != nil && a.Conversation.IsSuspendingTurn(calls)) {
			sc.onReset()
		}

		return text, calls, callErr
	}

	// 每次调用 LLM 前先按需压缩历史，避免长对话顶到模型上下文上限。
	// 压缩结果需要回写到对话循环，否则下一轮又会从压缩前的历史继续膨胀。
	// 压缩逻辑见 prepareMessages —— 上述 llmCall 已统一处理。

	// 第一轮：常规提交在此追加用户消息；continueOnly 直接从已有历史继续
	var (
		result *TurnResult
		err    error
	)
	if continueOnly {
		result, err = a.Conversation.ContinueTurn(ctx, llmCall)
	} else {
		result, err = a.Conversation.RunTurn(ctx, turnCtx, llmCall)
	}
	if err != nil {
		return "", err
	}
	a.Conversation.SetLastAssistantReasoning(reasoningBuf.String())

	// 多轮工具调用循环：模型调用工具后，继续调用 LLM 获取最终回答。
	//
	// 预算由 IterationBudget 统一控制 —— 此前这里是硬编码的
	// maxToolRounds=3，与 MaxIterations=25 并存且互相矛盾：
	// IterationBudget 的计数从未被消耗，实际生效的只有那个魔数。
	//
	// turn_suspended：本轮调用了终结型工具（推进任务的 wait / guard_finish），
	// 本回合到此为止，由宿主在声明时长后唤醒续跑。
	for len(result.ToolCalls) > 0 && result.FinishReason != "turn_suspended" {
		// 工具护栏熔断：同一工具反复失败 / 连续无进展时提前结束回合
		if reason := a.Conversation.GuardrailHalt(); reason != "" {
			a.Logger.Warn("工具调用护栏熔断，提前结束回合", "reason", reason)
			result.FinishReason = "guardrail_halt"

			break
		}

		if a.Budget != nil && !a.Budget.Consume() {
			a.Logger.Warn("迭代预算耗尽，提前结束回合",
				"max_iterations", a.Budget.MaxIterations)
			result.FinishReason = "budget_exhausted"

			break
		}

		// 工具已由 RunTurn/ContinueTurn 执行，结果已添加到消息历史，
		// 这里继续调用 LLM 获取最终回答。
		next, continueErr := a.Conversation.ContinueTurn(ctx, llmCall)
		if continueErr != nil {
			return "", continueErr
		}
		a.Conversation.SetLastAssistantReasoning(reasoningBuf.String())

		result = next
	}

	// 回合收尾：因预算耗尽 / 护栏熔断而没有最终回复时，补一次**不带工具**
	// 的总结调用（对齐 hermes-agent 的 _handle_max_iterations）—— 让模型把
	// 已经拿到的信息讲清楚，而不是给用户一句干巴巴的中断提示。
	output := collectTurnOutput(result)
	if needsTurnSummary(result) {
		if summary, sumErr := a.summarizeTurn(ctx, sc); sumErr != nil {
			a.Logger.Warn("回合收尾总结失败", "error", sumErr)
		} else if summary != "" {
			output = summary
		}
	}

	// 总结也没能产出内容时兜底给一句可读提示：否则客户端只会收到空文本的
	// message.complete，看起来像"模型没反应"。
	if output == "" && result != nil && result.FinishReason == "budget_exhausted" && a.Budget != nil {
		output = fmt.Sprintf("（已达本次回合的最大工具迭代次数 %d，已提前结束）", a.Budget.MaxIterations)
	}

	return output, nil
}

// needsTurnSummary 判断是否需要补一次收尾总结。
//
// 回合因预算耗尽 / 护栏熔断结束、且模型还没给出面向用户的回答时需要 ——
// 否则用户只看到一堆工具日志或一句中断提示。
func needsTurnSummary(result *TurnResult) bool {
	if result == nil || result.Content != "" {
		return false
	}

	switch result.FinishReason {
	case "budget_exhausted", "guardrail_halt":
		return true
	default:
		return false
	}
}

// summarizeTurn 在不带工具的情况下再调一次 LLM，产出面向用户的收尾总结。
//
// 去掉工具是有意的：此刻历史里已堆了大量工具结果，再给工具只会继续绕圈；
// 这一步的目标是"把已经拿到的信息讲清楚"。总结本身写入历史，下一回合模型
// 能看到它，不会出现"模型没说过这句话"的历史断层。
func (a *Agent) summarizeTurn(ctx context.Context, sc *streamCollector) (string, error) {
	prompt := Message{
		Role: RoleUser,
		Content: "请基于以上已完成的工具调用与结果，简洁总结当前进展与结论；" +
			"不要再调用任何工具，也不要原样重复工具输出。",
	}
	messages := append(a.Conversation.GetMessages(), prompt)

	var (
		content string
		err     error
	)
	// 收尾总结也可能带思考链，与常规回复同样落库（历史重载后可回看）
	var reasoningBuf strings.Builder
	onReasoning := func(chunk string) {
		reasoningBuf.WriteString(chunk)
		if sc.onReasoning != nil {
			sc.onReasoning(chunk)
		}
	}
	if sc == nil || sc.onText == nil {
		content, _, err = a.callLLMWithRetry(ctx, messages, nil, nil, nil, nil)
	} else {
		// 总结与正文同为最终回复，同样即时转发（真流式）
		content, _, err = a.callLLMWithRetry(ctx, messages, sc.onText, onReasoning, sc.usage, nil)
	}
	if err != nil {
		return "", err
	}

	a.Conversation.AddMessage(Message{
		Role:      RoleAssistant,
		Content:   content,
		Reasoning: reasoningBuf.String(),
	})

	return content, nil
}

// prepareMessages 在调用 LLM 前对消息历史做上下文压缩。
//
// 未超出阈值时 Compress 原样返回，无副作用。压缩生效时同步回写对话循环，
// 保证后续轮次基于压缩后的历史继续。压缩失败不阻断回合 —— 宁可让模型
// 自己撞上上下文上限，也不能因为压缩出错丢掉整段对话。
func (a *Agent) prepareMessages(ctx context.Context, messages []Message) []Message {
	// 配对修复必须无条件执行（不依赖压缩阈值）：历史里可能残留"有 tool_calls
	// 却没有配对的 tool 结果"的 assistant 消息（典型场景：工具执行时把后端进程
	// 自己杀掉、结果未及落库），OpenAI 兼容端点遇到它会一直 400。原先只在
	// Compress 内修复，短会话永远达不到压缩阈值，会永久卡死。修复后回写历史，
	// 下一次落盘即完成自愈。
	if fixed, changed := sanitizeToolPairs(messages); changed {
		a.Logger.Info("修复断裂的工具调用配对",
			"before", len(messages),
			"after", len(fixed),
		)
		a.Conversation.SetMessages(fixed)
		messages = fixed
	}

	maxTokens := a.CompactThreshold()

	// Phase-1 式修剪先于压缩：旧工具输出是历史膨胀大头，在更低阈值
	// （压缩阈值的一半）就提前收走，大块工具结果不必等到全量压缩才被
	// 机械截断。修剪结果回写对话循环，后续轮次基于修剪后的历史继续。
	if pruned, count := PruneToolResults(messages, maxTokens/2); count > 0 {
		a.Logger.Info("已修剪旧工具结果",
			"count", count,
			"messages", len(messages),
		)
		a.Conversation.SetMessages(pruned)
		messages = pruned
	}

	// 压缩执行权：同一会话同时只允许一个压缩（前台等待后台，或反过来）。
	// 等待被取消时走机械兜底——绝不能把超长历史直接发给模型。
	if a.Coordinator != nil {
		release, err := a.Coordinator.Acquire(ctx)
		if err != nil {
			a.Logger.Info("等待压缩执行权被取消，走机械压缩兜底")
			out, _ := legacyCompress(messages, maxTokens)
			if len(out) > 0 && len(out) < len(messages) {
				a.Conversation.SetMessages(out)
			}
			return out
		}
		defer release()
	}

	res := a.compactOnce(ctx, messages, maxTokens)
	if !res.Applied || len(res.Messages) >= len(messages) {
		return messages
	}

	a.Logger.Info("上下文已压缩",
		"before", len(messages),
		"after", len(res.Messages),
	)
	a.Conversation.SetMessages(res.Messages)

	return res.Messages
}

// compactOnce 用会话状态与当前模型参数执行一次压缩，并把新状态写回容器。
func (a *Agent) compactOnce(ctx context.Context, messages []Message, maxTokens int) CompactionResult {
	store := a.compactStore()
	state, hist := store.Snapshot()

	res := Compress(ctx, CompactionRequest{
		Messages:  messages,
		State:     state,
		History:   hist,
		MaxTokens: maxTokens,
		Summarize: a.compactSummarize,
	})
	if len(res.State.Segments) != len(state.Segments) || res.State.Snapshot != state.Snapshot {
		store.Replace(res.State)
	}
	return res
}

// compactStore 返回当前使用的状态容器：有 Coordinator 时以会话为准。
func (a *Agent) compactStore() *CompactStore {
	if a.Coordinator != nil {
		return a.Coordinator.Store()
	}
	if s := a.store.Load(); s != nil {
		return s
	}
	s := NewCompactStore()
	a.store.Store(s)
	return s
}

// BindStore 绑定压缩状态容器（serve 装配时调用；有 Coordinator 时以它为准）。
func (a *Agent) BindStore(store *CompactStore) {
	if store != nil {
		a.store.Store(store)
	}
}

// CompactState 返回压缩状态副本（落盘序列化用）。
func (a *Agent) CompactState() CompactState {
	st, _ := a.compactStore().Snapshot()
	return st
}

// SetCompactState 恢复持久化状态（会话恢复装配时调用）。
func (a *Agent) SetCompactState(st CompactState) {
	a.compactStore().Replace(st)
}

// SetHistorySource 注入归档原文数据源（CLI / REPL 装配时调用）。
func (a *Agent) SetHistorySource(fn HistorySourceFunc) {
	a.compactStore().SetHistory(fn)
}

// CompactThreshold 对外暴露压缩阈值（模型窗口 × 2/3），供 serve 判定预热。
func (a *Agent) CompactThreshold() int {
	if a.compressMaxTokens <= 0 {
		return 100000
	}
	return a.compressMaxTokens
}

// EstimatedTokens 当前内存历史的估算 token 数。
func (a *Agent) EstimatedTokens() int {
	if a.Conversation == nil {
		return 0
	}
	return estimateTotalTokens(a.Conversation.GetMessages())
}

// WarmCompact 后台预热一次压缩（只更新状态，不改写历史）。
// 执行权互斥由调用方（serve 的 compactSem）保证。
func (a *Agent) WarmCompact(ctx context.Context) error {
	if a.Conversation == nil {
		return nil
	}
	a.compactOnce(ctx, a.Conversation.GetMessages(), a.CompactThreshold())
	return nil
}

// collectTurnOutput 将回合结果整理为展示文本。
//
// 模型给出了文字回复时直接返回；只有工具结果时拼接成可读摘要。
func collectTurnOutput(result *TurnResult) string {
	if result == nil {
		return ""
	}

	if result.Content != "" {
		return result.Content
	}

	var output string
	for _, tr := range result.ToolResults {
		if tr.IsError {
			output += fmt.Sprintf("[工具 %s 错误: %s]\n", tr.Name, tr.Content)
		} else {
			output += fmt.Sprintf("[工具 %s 结果: %s]\n", tr.Name, tr.Content)
		}
	}

	return output
}

// callLLM 调用模型 API。
//
// 协议细节（端点拼接、鉴权、请求体构造、SSE 解析、错误分类）全部在
// agent/llm 里 —— 这里只负责把 core 的 Message 转成 OpenAI 格式、调用，
// 再把结果转回 core 的类型。
//
// onChunk 非 nil 时启用流式（请求体带 stream:true，响应按 SSE 帧逐块回调）；
// usage 非 nil 时把本次调用的 token 用量累加进去。
func (a *Agent) callLLM(ctx context.Context, messages []Message, onChunk func(string), onReasoning func(string), usage *TokenUsage) (string, []ToolCallRef, error) {
	return a.callLLMWithTools(ctx, messages, onChunk, onReasoning, usage, a.ToolSchemas)
}

// compactSummarizeTimeout 摘要小请求的整体上限：比回合内非流式调用
// （llmNonStreamTimeout）紧得多——摘要走降级路径即可，不值得等。
const compactSummarizeTimeout = 60 * time.Second

// compactSummarize 压缩器的摘要通道（Compress 经 SummarizeFunc 调用）。
//
// 独立小请求：不带系统提示词与工具定义（messagesToOpenAI 传空 system）、
// 低温度、非流式。失败由压缩器统一降级（legacyCompress），此处不重试——
// 重试语义只值得花在主回合上。
func (a *Agent) compactSummarize(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, compactSummarizeTimeout)
	defer cancel()

	result, err := a.llmOrDefault().Chat(callCtx, llm.Options{
		Messages:    messagesToOpenAI([]Message{{Role: RoleUser, Content: prompt}}, "", false),
		Temperature: 0.2,
		MaxTokens:   4096,
	})
	if err != nil {
		return "", err
	}

	return result.Content, nil
}

// callLLMWithTools 与 callLLM 相同，但可显式指定工具定义。
//
// 回合收尾总结传 nil（不给工具）：此刻历史里已堆了大量工具结果，再给工具
// 只会继续绕圈，而这一步的目标是把已经拿到的信息讲清楚。
func (a *Agent) callLLMWithTools(ctx context.Context, messages []Message, onChunk func(string), onReasoning func(string), usage *TokenUsage, tools []map[string]any) (string, []ToolCallRef, error) {
	// 非流式调用没有增量信号、看门狗不启用 —— 给它一个整体上限，
	// 避免上游「回了响应头之后 body 停摆」把调用永久挂住（见 llmNonStreamTimeout）。
	callCtx := ctx
	if onChunk == nil {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, llmNonStreamTimeout)
		defer cancel()
	}

	a.Logger.Info("LLM 调用开始",
		"model", a.ModelName,
		"stream", onChunk != nil,
		"messages", len(messages),
		"tools", len(tools),
	)
	// 历史图片降级：超出窗口的旧图不再携带像素。放在这里而不是 prepareMessages
	// 里，是为了让所有 LLM 出口（含回合收尾总结）走同一条规则。
	requestMessages := degradeOldImages(messages, a.imageHistoryLimit())

	result, err := a.llmOrDefault().Chat(callCtx, llm.Options{
		Messages:    messagesToOpenAI(requestMessages, a.systemPrompt(), a.SupportsVision()),
		Tools:       tools,
		Temperature: a.Temperature,
		MaxTokens:   a.MaxTokens,
		Stream:      onChunk != nil,
		OnChunk:     onChunk,
		OnReasoning: onReasoning,
	})
	if err != nil {
		// 只有我们自己加的上限触发才算停滞：回合 ctx 已取消时按原错误上报
		if onChunk == nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return "", nil, fmt.Errorf("%w: %v", errLLMNonStreamTimeout, err)
		}

		return "", nil, err
	}

	// 记录 token 用量并回填调用方（会话层用它推 usage 给客户端）
	a.Logger.Info("LLM 调用完成",
		"prompt_tokens", result.Usage.PromptTokens,
		"cached_tokens", result.Usage.CachedTokens,
		"completion_tokens", result.Usage.CompletionTokens,
		"tool_calls", len(result.ToolCalls),
	)

	if usage != nil {
		usage.PromptTokens += result.Usage.PromptTokens
		usage.CompletionTokens += result.Usage.CompletionTokens
		usage.TotalTokens += result.Usage.TotalTokens
		usage.CachedTokens += result.Usage.CachedTokens
	}

	return result.Content, toToolCallRefs(result.ToolCalls), nil
}

// llmOrDefault 返回 Agent 的转发客户端；未注入时按当前配置兜底构造一个，
// 保证零配置路径（只给 base-url 与 api-key）依然可用。
func (a *Agent) llmOrDefault() *llm.Client {
	if a.LLM != nil {
		return a.LLM
	}

	return llm.NewClient(llm.Config{
		BaseURL: a.BaseURL,
		APIKey:  a.APIKey,
		Model:   a.ModelName,
	}, a.HTTPClient, a.Logger)
}

// callLLMWithRetry 在 callLLM 之外包一层分类重试。
//
// 对齐 hermes-agent 的回合内重试语义：
//   - 只有分类为可重试的失败（429 / 5xx / 网络 / 超时 / 空响应）才重试；
//   - 鉴权 / 计费 / 限流类失败先尝试换一个凭证（池里存在其它 key 时）；
//   - 上游给了 Retry-After 就按它等待（llm 层已把离谱值限到 10 分钟）；
//   - **流式已推过正文就不重试**：客户端已经收到 message.delta，重试会重复输出；
//   - ctx 取消（用户中断）立即返回，不重试。
// streamIdleGuard 流式 LLM 调用的空闲看门狗：每次增量（正文/思考）喂狗
// 续期，超过 llmStreamIdleTimeout 没有任何增量则取消派生的 ctx，
// transport 的读会随 ctx 中止 —— 把「provider 停滞」从永久挂起变成可恢复的错误。
//
// 非流式调用（onChunk == nil）没有增量信号，看门狗不启用：
// 协作子 agent 由 30 分钟墙钟兜底，父回合始终是流式。
type streamIdleGuard struct {
	ctx         context.Context // 派生 ctx：本次 LLM 调用应使用它
	cancel      func()          // 释放看门狗（defer 用）
	cancelCause context.CancelCauseFunc
	fired       atomic.Bool
	feed        chan struct{} // 增量信号；nil = 未启用
	logger      *slog.Logger
}

// newStreamIdleGuard 创建看门狗。streaming=false 时为直通壳（无看门狗）。
func (a *Agent) newStreamIdleGuard(ctx context.Context, streaming bool) *streamIdleGuard {
	g := &streamIdleGuard{logger: a.Logger}
	if !streaming {
		g.ctx = ctx
		g.cancel = func() {}
		g.cancelCause = func(error) {}
		return g
	}
	watchCtx, cancelCause := context.WithCancelCause(ctx)
	g.ctx = watchCtx
	g.cancel = func() { cancelCause(nil) }
	g.cancelCause = cancelCause
	g.feed = make(chan struct{}, 1)
	go func() {
		timer := time.NewTimer(llmStreamIdleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-g.feed:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(llmStreamIdleTimeout)
			case <-timer.C:
				g.fired.Store(true)
				a.Logger.Warn("LLM 流式调用空闲超时，取消本次调用",
					"idle_timeout", llmStreamIdleTimeout.String())
				cancelCause(errLLMStreamIdle)
				return
			}
		}
	}()
	return g
}

// wrapChunk / wrapReasoning 把增量回调接到喂狗通道（透传原回调）。
func (g *streamIdleGuard) wrapChunk(f func(string)) func(string) {
	if f == nil || g.feed == nil {
		return f
	}
	return func(s string) {
		select {
		case g.feed <- struct{}{}:
		default:
		}
		f(s)
	}
}

func (g *streamIdleGuard) wrapReasoning(f func(string)) func(string) {
	if f == nil || g.feed == nil {
		return f
	}
	return func(s string) {
		select {
		case g.feed <- struct{}{}:
		default:
		}
		f(s)
	}
}

// fired 看门狗是否已触发（触发过的调用按非重试错误处理）。
func (g *streamIdleGuard) firedFlag() bool { return g.fired.Load() }

func (a *Agent) callLLMWithRetry(
	ctx context.Context,
	messages []Message,
	onChunk func(string),
	onReasoning func(string),
	usage *TokenUsage,
	tools []map[string]any,
) (string, []ToolCallRef, error) {
	if a.maxRetries <= 0 {
		return a.callLLMWithTools(ctx, messages, onChunk, onReasoning, usage, tools)
	}

	cfg := infra.RetryConfig{
		MaxRetries: a.maxRetries,
		BaseDelay:  retryBaseDelay,
		MaxDelay:   retryMaxDelay,
		Jitter:     retryJitter,
	}

	// 空闲看门狗：覆盖全部重试尝试（每次尝试的增量都会喂狗续期）
	guard := a.newStreamIdleGuard(ctx, onChunk != nil)
	defer guard.cancel()

	var lastErr error
	for attempt := 0; ; attempt++ {
		// 本次尝试是否已经产出正文：产物一旦推给客户端就不能重试
		emitted := false
		chunk := onChunk
		if onChunk != nil {
			chunk = func(text string) {
				emitted = true
				onChunk(text)
			}
		}

		content, calls, err := a.callLLMWithTools(guard.ctx, messages, guard.wrapChunk(chunk), guard.wrapReasoning(onReasoning), usage, tools)
		if err == nil {
			return content, calls, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return "", nil, lastErr
		}

		// 空闲超时是我们主动取消的：同一 provider 大概率再次停滞，重试无意义
		if guard.firedFlag() {
			return "", nil, fmt.Errorf("%w: %v", errLLMStreamIdle, err)
		}

		// 同理：非流式请求停滞超时也不重试（否则 4 次尝试 × 上限 = 用户干等）
		if errors.Is(err, errLLMNonStreamTimeout) {
			return "", nil, err
		}

		// 换凭证先于重试判定执行：它可能正是失败的原因
		if llm.ReasonOf(err).ShouldRotateCredential() {
			a.tryRotateCredential()
		}

		if !llm.IsRetryable(err) {
			return "", nil, lastErr
		}
		if emitted {
			a.Logger.Warn("流式响应已开始输出，放弃重试",
				"reason", llm.ReasonOf(err))

			return "", nil, lastErr
		}
		if attempt >= a.maxRetries {
			break
		}

		delay := infra.BackoffDelay(cfg, attempt)
		if after := llm.RetryAfterOf(err); after > 0 {
			delay = after
		}

		a.Logger.Warn("LLM 调用失败，准备重试",
			"attempt", attempt+1,
			"max_retries", a.maxRetries,
			"delay", delay,
			"reason", llm.ReasonOf(err),
			"error", err,
		)

		select {
		case <-ctx.Done():
			return "", nil, lastErr
		case <-time.After(delay):
		}
	}

	return "", nil, lastErr
}

// tryRotateCredential 换用凭证池里的下一个 key 并重建转发客户端。
//
// 池里没有第二个可用 key 时不做任何事 —— 换到同一个 key 重试只会白费一次
// 调用。hermes-agent 的 should_rotate_credential 语义相同，区别是它的池带
// 轮换状态，这里用"当前 key 的下一个"近似。
func (a *Agent) tryRotateCredential() bool {
	if a.CredentialPool == nil {
		return false
	}

	creds := a.CredentialPool.ListCredentials(a.Provider)
	if len(creds) < 2 {
		return false
	}

	for i := range creds {
		if creds[i].Value != a.APIKey {
			continue
		}

		next := creds[(i+1)%len(creds)]
		if next.Value == "" || next.Value == a.APIKey {
			return false
		}

		a.APIKey = next.Value
		a.rebuildLLMClient()
		a.Logger.Warn("已切换备选凭证", "provider", a.Provider)

		return true
	}

	return false
}

// rebuildLLMClient 用当前字段重建转发客户端（凭证轮换后调用）。
func (a *Agent) rebuildLLMClient() {
	cfg := a.llmConfig
	cfg.APIKey = a.APIKey
	a.llmConfig = cfg
	a.LLM = llm.NewClient(cfg, a.HTTPClient, a.Logger)
}

// UpdateAPIKey 更新凭据并重建转发客户端。
//
// 内置模型的 hb_ Key 会在服务端重签（rotate）后变化：serve 层在回合开始
// 对齐快照、或 401 自愈重签成功后调用此方法，下一次请求即用新 Key。
// 与凭证池轮换（rotateCredential）走同一条 rebuildLLMClient 路径。
func (a *Agent) UpdateAPIKey(key string) {
	if key == "" || key == a.APIKey {
		return
	}
	a.APIKey = key
	a.rebuildLLMClient()
}

// toToolCallRefs 把 llm 层的工具调用转成历史消息使用的引用格式。
func toToolCallRefs(calls []llm.ToolCall) []ToolCallRef {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCallRef, 0, len(calls))
	for _, call := range calls {
		ref := ToolCallRef{ID: call.ID, Type: "function"}
		ref.Function.Name = call.Name
		ref.Function.Arguments = call.Arguments
		out = append(out, ref)
	}

	return out
}

// SupportsVision 当前模型是否接受图片输入。
//
// 能力由 serve 从渠道配置/目录解析后写入 AgentConfig.Caps，core 内不做
// 任何 provider 猜测 —— 猜错的代价是严格渠道直接 400，且模型也看不见图。
func (a *Agent) SupportsVision() bool {
	return a.llmConfig.Caps.SupportsVision
}

// messagesToOpenAI 将内部 Message 列表转换为 OpenAI 格式。
//
// supportsVision 为 true 时，带图片附件的 user 消息 content 展开为 parts
// 数组（见 buildUserContent）；false 时附件降级为一段文本说明，不盲发。
func messagesToOpenAI(msgs []Message, systemPrompt string, supportsVision bool) []map[string]any {
	result := make([]map[string]any, 0, len(msgs)+1)

	// 系统消息放在最前
	if systemPrompt != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	for _, msg := range msgs {
		m := map[string]any{
			"role":    string(msg.Role),
			"content": buildUserContent(msg, supportsVision),
		}
		if msg.Name != "" {
			m["name"] = msg.Name
		}
		if msg.ToolCallID != "" {
			m["tool_call_id"] = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			tcs := make([]map[string]any, len(msg.ToolCalls))
			for i, tc := range msg.ToolCalls {
				tcs[i] = map[string]any{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				}
			}
			m["tool_calls"] = tcs
		}
		result = append(result, m)
	}

	return result
}

// systemPrompt 返回当前系统提示词（拼装结果在段变化时才重算）。
func (a *Agent) systemPrompt() string {
	a.promptMu.RLock()
	defer a.promptMu.RUnlock()

	return a.promptFull
}

// SetMemory 设置记忆条目并刷新 volatile 段。
//
// 只重建 volatile 段：stable（身份 + 工具指导）与 context 段原样保留，
// 上游的 prefix cache 不会因为写一次记忆就整段失效。此前是整串重建。
func (a *Agent) SetMemory(key, value string) {
	a.Memory.Set(key, value)

	a.promptMu.Lock()
	defer a.promptMu.Unlock()

	a.promptMemory = a.Memory.Snapshot()
	a.promptParts.Volatile = buildVolatilePrompt(a.promptMeta, a.promptMemory)
	a.promptFull = a.promptParts.Full()
}

// compressThresholdFor 返回触发上下文压缩的 token 阈值。
//
// 窗口优先取渠道目录给出的模型能力（Caps.MaxInputTokens，与真实模型一致）：
// AgentConfig.ContextWindow 是调用方的兜底，serve 侧长期传 200k 的固定默认值，
// 用在 1M 窗口的模型上会导致过早压缩。
//
// 阈值取窗口的 2/3 而非更晚：压缩是"重新组装上下文"（见 context_compressor.go），
// 组装产物 = 快照 + 段摘要 + 锚点 + 保留尾，触发后要给后续工作留出 1/3 以上
// 空间，否则刚压完马上又要压。估算公式（4 字符 ≈ 1 token）对中文偏乐观，
// 这层误差也靠提前触发对冲。
func compressThresholdFor(cfg AgentConfig) int {
	window := cfg.ContextWindow
	if cfg.Caps.MaxInputTokens > 0 {
		window = cfg.Caps.MaxInputTokens
	}

	return window * 2 / 3
}

// promptMetaItems 生成 volatile 段里每会话固定的元信息行。
//
// 日期刻意只到「日」：分钟级时间戳会让每次重建都改变前缀，上游缓存整段
// 失效 —— hermes-agent 出于同样理由把时间精度压到日。
func promptMetaItems(cfg AgentConfig) []string {
	items := []string{
		"Conversation started: " + time.Now().Format("Monday, January 02, 2006"),
	}
	if cfg.ModelName != "" {
		line := "Model: " + cfg.ModelName
		if cfg.Provider != "" {
			line += " | Provider: " + cfg.Provider
		}
		items = append(items, line)
	}

	return items
}

// generateTurnID 生成唯一回合标识
func generateTurnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("turn_%x", b)
}
