package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aron1982/hr-agent-core/infra"
)

// ToolObserver 观测工具执行过程。
//
// 核心引擎不关心事件如何被消费 —— CLI 可以忽略它，gateway 借它向
// desktop 推送 tool.start / tool.complete 事件。接口定义在 core 侧、
// 实现留在外层，避免 core 反向依赖传输层。
type ToolObserver interface {
	// OnToolStart 在工具执行前触发。
	OnToolStart(toolCallID, name, arguments string)
	// OnToolComplete 在工具执行结束后触发（含失败）。
	OnToolComplete(toolCallID, name, output string, isError bool, durationMs int64)
}

// SetToolObserver 设置工具执行观测器，nil 表示不观测。
func (c *ConversationLoop) SetToolObserver(observer ToolObserver) {
	c.observer = observer
}

// ConversationLoop 主对话循环，管理消息历史、预算和回合执行
type ConversationLoop struct {
	// mu 保护 messages 与 retrySafeFrom。
	//
	// 同一会话的回合由 serve 的 turnMu 串行，但历史还会被回合之外的路径
	// 读取：落盘快照（任一会话的 save 都会遍历全部会话的消息）、
	// session.history / session.status、重试可行性查询。此前这些读与
	// 回合内的 AddMessage 争用同一个 slice（数据竞争，可能读到撕裂状态）。
	mu           sync.RWMutex
	messages     []Message
	budget       *IterationBudget
	trajectory   *Trajectory
	toolRegistry *ToolRegistry
	guardrail    *ToolCallGuardrail
	observer     ToolObserver
	// imageStore 工具产出图片的落库出口（宿主注入；nil = 不落库）。
	imageStore ToolImageStore
	// haltReason 非空表示本回合已被工具护栏熔断（受 mu 保护）。
	haltReason string

	// suspendingTools 终结型工具名集合（仅推进任务会话配置，见 serve 的
	// guard 装配）。模型调用这类工具意味着"本回合到此为止，稍后由宿主
	// 唤醒续跑"，因此本轮不再继续调用 LLM，写入历史时也拆成两条：
	// assistant(正文) + assistant(空正文, tool_calls)，正文不会因带工具调用
	// 被客户端当成过程旁白折叠，工具痕迹则保留给下一轮模型看。
	// 空集合 = 不启用（普通会话行为不变）。
	suspendingTools map[string]bool
	// noteTools 终结型工具里"改写为正文"的子集（推进任务的 guard_finish）：
	// 同样结束回合，但不保留工具卡 —— 工具输出被改写成一条 assistant 消息，
	// 客户端直接按正文显示（"已记录完成，本次推进任务结束。"）。
	noteTools map[string]bool
	// notedCalls 已被改写为 assistant 消息的工具调用 id（受 mu 保护）。
	notedCalls map[string]bool

	// retrySafeFrom 检查点：可安全回滚的最早消息下标。
	//
	// 工具一旦执行就可能已有副作用（写数据、发通知），重跑不安全，所以
	// 每遇到一条工具相关消息（带 ToolCalls 的 assistant 或 tool 结果）
	// 就把检查点推到它之后。重试/重新生成只允许截断检查点之后的区间。
	// 从历史推导即可，不落库；加载历史与压缩回写都经 SetMessages 重算。
	retrySafeFrom int

	// rev 历史变更计数：消息被追加 / 整体替换 / 截断时自增（受 mu 保护）。
	//
	// 供落盘侧判断"这个会话要不要重写"。比"当前有多少条消息"更可靠：
	// 回退后再续跑可能条数相同而内容不同。单调递增，比较相等即可。
	rev uint64
}

// NewConversationLoop 创建对话循环实例
func NewConversationLoop(budget *IterationBudget, tools *ToolRegistry, guard *ToolCallGuardrail) *ConversationLoop {
	return &ConversationLoop{
		messages:     make([]Message, 0),
		budget:       budget,
		trajectory:   &Trajectory{},
		toolRegistry: tools,
		guardrail:    guard,
	}
}

// SetSuspendingTools 配置终结型工具（宿主装配推进任务会话时注入，见 serve）。
//
// 这些工具一旦出现在某轮的 tool_calls 里，本回合就不再继续调用 LLM；该轮
// 的正文与工具调用会拆成两条消息落库，正文因此不会在历史重载时被折叠。
func (c *ConversationLoop) SetSuspendingTools(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.suspendingTools = make(map[string]bool, len(names))
	for _, n := range names {
		c.suspendingTools[n] = true
	}
}

// SetNoteSuspendingTools 配置"改写为正文"的终结型工具（推进任务的
// guard_finish）：同样结束回合，但工具调用与结果都换成一条 assistant 消息，
// 内容取工具输出。
func (c *ConversationLoop) SetNoteSuspendingTools(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.suspendingTools == nil {
		c.suspendingTools = make(map[string]bool)
	}
	if c.noteTools == nil {
		c.noteTools = make(map[string]bool)
	}
	for _, n := range names {
		c.suspendingTools[n] = true
		c.noteTools[n] = true
	}
}

// IsNoteTurn 判断本轮 tool_calls 是否全部由"改写为正文"的工具组成。
func (c *ConversationLoop) IsNoteTurn(calls []ToolCallRef) bool {
	if len(calls) == 0 {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.noteTools) == 0 {
		return false
	}
	for _, tc := range calls {
		if !c.noteTools[tc.Function.Name] {
			return false
		}
	}

	return true
}

// markNoted 记录这些调用将被改写成 assistant 消息。
func (c *ConversationLoop) markNoted(calls []ToolCallRef) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.notedCalls == nil {
		c.notedCalls = make(map[string]bool, len(calls))
	}
	for _, tc := range calls {
		c.notedCalls[tc.ID] = true
	}
}

// wasNoted 判断某个工具调用是否要改写成 assistant 消息。
func (c *ConversationLoop) wasNoted(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.notedCalls[id]
}

// IsSuspendingTurn 判断本轮 tool_calls 是否全部由终结型工具组成。
// 只有"全部"才拆分 —— 混合调用若拆分会让工具卡与正文的先后关系失真。
func (c *ConversationLoop) IsSuspendingTurn(calls []ToolCallRef) bool {
	if len(calls) == 0 {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.suspendingTools) == 0 {
		return false
	}
	for _, tc := range calls {
		if !c.suspendingTools[tc.Function.Name] {
			return false
		}
	}

	return true
}

// TurnResult 单个对话回合的结果
type TurnResult struct {
	Content      string        // LLM 返回的文本内容
	ToolCalls    []ToolCallRef // 模型发起的工具调用
	ToolResults  []ToolResult  // 工具执行结果
	FinishReason string        // 结束原因：stop, tool_calls, budget_exhausted, error
	Usage        TokenUsage    // token 用量
}

// TokenUsage token 用量统计
type TokenUsage struct {
	PromptTokens     int // 提示词 token 数
	CompletionTokens int // 补全 token 数
	TotalTokens      int // 总 token 数
	// CachedTokens 命中上游前缀缓存（prompt cache）的提示词 token 数。
	// llm 层已按渠道差异归一化（OpenAI 的 prompt_tokens_details.cached_tokens
	// / DeepSeek 的 prompt_cache_hit_tokens），这里只做累加与透传 —— 会话层
	// 与客户端据此判断"前缀是否还热"。
	CachedTokens int
}

// RunTurn 执行一个对话回合
// 流程: 用户输入 → 构建消息 → LLM调用 → 解析响应 → 工具执行 → 添加结果 → 返回
func (c *ConversationLoop) RunTurn(ctx context.Context, turnCtx *TurnContext, llmCall func(messages []Message) (string, []ToolCallRef, error)) (*TurnResult, error) {
	// 1. 添加用户消息（附件只带定位信息，字节在构造请求时读取）
	userMsg := Message{
		Role:        RoleUser,
		Content:     turnCtx.UserMessage,
		Attachments: turnCtx.Attachments,
	}
	c.AddMessage(userMsg)

	// 回合级护栏复位：计数不跨回合累计（跨回合累计会把"用户本来就在反复试"
	// 误判为死循环），同时清掉上一回合的熔断标记。
	if c.guardrail != nil {
		c.guardrail.ResetForTurn()
	}
	c.clearHalt()

	// 2. 检查预算
	if c.budget != nil && c.budget.IsExhausted() {
		return &TurnResult{
			FinishReason: "budget_exhausted",
		}, nil
	}

	// 3. 调用 LLM
	content, toolCalls, err := llmCall(c.GetMessages())
	if err != nil {
		return nil, fmt.Errorf("llm call failed: %w", err)
	}

	// 4. 添加 assistant 消息；有工具调用则执行（终结型工具会结束本回合）
	result := c.addAssistantTurn(ctx, content, toolCalls)

	return result, nil
}

// addAssistantTurn 写入本轮 assistant 消息，并按需执行工具调用。
//
// 终结型工具（推进任务的 wait / guard_finish）调用后本回合即结束。这一轮
// 不能把正文和 tool_calls 写进同一条消息 —— 历史重载时客户端会按"带
// tool_calls 的 assistant 文本 = 过程旁白"把它折叠掉（2026-11-20：《看秤》
// 被折进工具卡的问题）。所以拆成两条：
//
//	assistant(正文, 无 tool_calls)   ← 客户端按正文渲染
//	assistant(空正文, tool_calls)    ← 客户端按工具卡渲染
//	tool(结果)                        ← 与上一条配对，协议合法
//
// 保留工具痕迹是有意的：模型下一轮能看到"已经等待过"，不会重复调用 wait。
// 终局工具（guard_finish，见 noteTools）不同：它不会再被续跑，"改写为正文"
// 更能贴合"它就是最后一句结果说明"的语义。
func (c *ConversationLoop) addAssistantTurn(ctx context.Context, content string, toolCalls []ToolCallRef) *TurnResult {
	suspending := c.IsSuspendingTurn(toolCalls)
	if suspending {
		// 正文为空时只写工具调用那条，避免历史里留下空 assistant 消息
		if strings.TrimSpace(content) != "" {
			c.AddMessage(Message{
				Role:    RoleAssistant,
				Content: content,
			})
		}
		if c.IsNoteTurn(toolCalls) {
			// 终局工具：调用与结果都换成 assistant 正文（见 commitToolCall）
			c.markNoted(toolCalls)
		} else {
			c.AddMessage(Message{
				Role:      RoleAssistant,
				ToolCalls: toolCalls,
			})
		}
	} else {
		c.AddMessage(Message{
			Role:      RoleAssistant,
			Content:   content,
			ToolCalls: toolCalls,
		})
	}

	result := &TurnResult{
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: "stop",
	}

	if len(toolCalls) > 0 && c.toolRegistry != nil {
		result.FinishReason = "tool_calls"
		result.ToolResults = c.runToolCalls(ctx, toolCalls)
		if suspending {
			// 本回合到此为止：runTurn 的多轮循环据此不再调用 LLM，
			// 由宿主（推进任务主循环）在声明的时长后再唤醒续跑。
			result.FinishReason = "turn_suspended"
		}
	}

	return result
}

// runToolCalls 执行本轮的全部工具调用并收集结果（按原始顺序返回）。
//
// 分段执行（对齐 hermes-agent 的 tool_executor）：连续的可并发调用（只读
// 工具）合成一段并发跑，其余工具独占一段串行跑。结果顺序与模型给出的
// tool_calls 顺序严格一致 —— 历史消息的顺序错了，下一轮 API 会直接 400。
func (c *ConversationLoop) runToolCalls(ctx context.Context, calls []ToolCallRef) []ToolResult {
	results := make([]ToolResult, len(calls))

	for _, seg := range planToolBatchSegments(calls) {
		switch len(seg) {
		case 0:
			continue
		case 1:
			results[seg[0]] = c.executeToolCall(ctx, calls[seg[0]])
		default:
			c.runToolSegmentConcurrent(ctx, calls, seg, results)
		}
	}

	// 图片在全部工具结果落库之后统一追加：工具结果必须连续成组，
	// 在中间插 user 消息会破坏 tool_call 与 tool 结果的配对。
	c.appendToolImages(results)

	return results
}

// runToolSegmentConcurrent 并发执行一段可并发的调用。
//
// 只有"真正执行工具"这一步并行：闸门判定、轨迹、事件与历史消息的提交都在
// 调用方 goroutine 里按原始顺序进行，因此护栏计数与历史顺序与串行时完全
// 一致，被并行化的只有耗时。
func (c *ConversationLoop) runToolSegmentConcurrent(ctx context.Context, calls []ToolCallRef, seg []int, results []ToolResult) {
	pending := make([]ToolCallRef, 0, len(seg))
	pendingIdx := make([]int, 0, len(seg))

	for _, idx := range seg {
		tc := calls[idx]

		if c.observer != nil {
			c.observer.OnToolStart(tc.ID, tc.Function.Name, tc.Function.Arguments)
		}

		if c.guardrail != nil {
			if action, reason := c.guardrail.BeforeCall(tc.Function.Name, tc.Function.Arguments); action == ToolGuardBlock {
				results[idx] = c.blockedToolResult(tc, reason)

				continue
			}
		}

		pending = append(pending, tc)
		pendingIdx = append(pendingIdx, idx)
	}

	outs := ExecuteConcurrent(ctx, c.toolRegistry, pending, maxParallelToolCalls)
	for i, idx := range pendingIdx {
		results[idx] = c.commitToolCall(calls[idx], outs[i])
	}
}

// executeToolCall 串行执行单个工具调用：护栏闸门 → 执行 → 提交结果。
func (c *ConversationLoop) executeToolCall(ctx context.Context, tc ToolCallRef) ToolResult {
	if c.observer != nil {
		c.observer.OnToolStart(tc.ID, tc.Function.Name, tc.Function.Arguments)
	}

	// 执行前闸门：判为死循环 / 连续失败超限时不执行，直接把原因交回模型
	if c.guardrail != nil {
		if action, reason := c.guardrail.BeforeCall(tc.Function.Name, tc.Function.Arguments); action == ToolGuardBlock {
			return c.blockedToolResult(tc, reason)
		}
	}

	start := time.Now()
	// 观察者额外实现了增量输出能力时（终端命令等长任务），把回调注入
	// context 供工具侧边跑边推；未实现则保持原样，工具走一次性返回。
	execCtx := ctx
	if obs, ok := c.observer.(ToolOutputObserver); ok {
		execCtx = infra.WithToolOutputEmitter(ctx, func(chunk string) {
			obs.OnToolOutput(tc.ID, chunk)
		})
	}
	output, err := c.toolRegistry.Execute(execCtx, tc.Function.Name, tc.Function.Arguments)

	return c.commitToolCall(tc, Invocation{
		Output:     output,
		Err:        err,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// commitToolCall 把一次执行结果落到护栏观察 / 轨迹 / 事件 / 历史，并返回结果。
//
// 必须按原始工具调用顺序调用（串行路径与并发段的提交阶段都满足这一点）。
func (c *ConversationLoop) commitToolCall(tc ToolCallRef, inv Invocation) ToolResult {
	name := tc.Function.Name

	// 执行后观察：返回需要追加到结果的护栏提示（熔断时同时记下原因）
	notice := c.noteToolOutcome(name, tc.Function.Arguments, inv.Output, inv.Err != nil)

	content := inv.Output
	if inv.Err != nil {
		content = inv.Err.Error()
	}

	// 图片侧信道：工具在输出里内联的图片不能随 tool 消息下发（协议只接受
	// 字符串），先剥离出来，稍后由 appendToolImages 改挂到一条 user 消息上。
	// 错误输出不解析 —— 出错时返回的 JSON 只是一段错误说明。
	var images []Attachment
	if inv.Err == nil {
		content, images = extractToolImages(content)
		// 入库越早越好：附件 id 要随本回合的落盘一起进库，否则重启后
		// 这条消息的图片会凭空消失（无 id 的引用会被落盘侧跳过）。
		images = c.persistToolImages(images)
	}

	content = appendGuardrailNotice(content, notice)

	// 记录轨迹（保留原始输出，护栏提示只进消息）
	c.trajectory.AddStep(TrajectoryStep{
		Action: "tool_call",
		Input: map[string]any{
			"tool_name": name,
			"arguments": tc.Function.Arguments,
		},
		Output: map[string]any{
			"content": inv.Output,
		},
		Duration: inv.DurationMs,
	})

	if c.observer != nil {
		c.observer.OnToolComplete(tc.ID, name, content, inv.Err != nil, inv.DurationMs)
	}

	// 失败也必须补一条结果消息，两个理由：
	//   1. 否则历史里会留下一条带 tool_calls 的 assistant 消息却没有
	//      配对的 tool 结果 —— 违反 OpenAI 的消息配对约定，真机 API
	//      下一轮会直接 400；
	//   2. 模型看不到失败原因（被拒绝 / 超时 / 文件不存在），只能瞎猜
	//      或盲目重试同一个工具。
	//
	// 例外：终局工具（guard_finish）没有 tool_calls 落库，这里把工具输出
	// 改写成 assistant 正文 —— 聊天窗口里直接显示它，而不是工具卡。
	if c.wasNoted(tc.ID) {
		c.AddMessage(Message{
			Role:    RoleAssistant,
			Content: content,
		})
	} else {
		c.AddMessage(Message{
			Role:       RoleTool,
			Content:    content,
			ToolCallID: tc.ID,
			Name:       name,
		})
	}

	return ToolResult{
		ToolCallID:        tc.ID,
		Name:              name,
		Content:           content,
		IsError:           inv.Err != nil,
		Images:            images,
		EffectDisposition: ClassifyEffectDisposition(name, inv.Output, inv.Err != nil),
	}
}

// noteToolOutcome 调护栏判定并返回要追加到结果的提示（可为空）。
func (c *ConversationLoop) noteToolOutcome(name, args, output string, isError bool) string {
	if c.guardrail == nil {
		return ""
	}

	action, reason := c.guardrail.AfterCall(name, args, output, isError)
	switch action {
	case ToolGuardWarn:
		return reason
	case ToolGuardHalt:
		c.setHalt(reason)

		return reason
	default:
		return ""
	}
}

// blockedToolResult 闸门阻断：不执行工具，把原因作为工具结果交回模型。
func (c *ConversationLoop) blockedToolResult(tc ToolCallRef, reason string) ToolResult {
	content := "[护栏阻断] " + reason

	if c.observer != nil {
		c.observer.OnToolComplete(tc.ID, tc.Function.Name, content, true, 0)
	}
	c.AddMessage(Message{
		Role:       RoleTool,
		Content:    content,
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
	})

	return ToolResult{
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
		Content:    content,
		IsError:    true,
		// 工具没执行，不存在副作用
		EffectDisposition: EffectNone,
	}
}

// appendGuardrailNotice 把护栏提示追加到工具结果末尾。
func appendGuardrailNotice(content, notice string) string {
	if notice == "" {
		return content
	}

	return content + "\n\n[护栏提示] " + notice
}

// GuardrailHalt 返回本回合的熔断原因（空 = 未熔断）。
// 由 runTurn 在工具轮之间检查，用于提前结束回合。
func (c *ConversationLoop) GuardrailHalt() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.haltReason
}

// setHalt 记录熔断原因（并发工具执行时可能同时写，故加锁）。
func (c *ConversationLoop) setHalt(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haltReason == "" {
		c.haltReason = reason
	}
}

// clearHalt 清除熔断标记（回合开始时调用）。
func (c *ConversationLoop) clearHalt() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.haltReason = ""
}

// GetMessages 返回消息历史的副本，调用方可安全持有并并发读取。
func (c *ConversationLoop) GetMessages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return cloneMessages(c.messages)
}

// AddMessage 添加一条消息到历史
func (c *ConversationLoop) AddMessage(msg Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 时间戳在唯一入口兜底：user / assistant / tool 三条追加路径都不必各自
	// 记时间；已带值的（从库里加载后回写）保持原值。
	if msg.CreatedAt == 0 {
		msg.CreatedAt = time.Now().Unix()
	}
	c.messages = append(c.messages, msg)
	// 工具相关消息推高检查点：它之后的消息才是可安全回滚的区间
	if isToolRelated(msg) {
		c.retrySafeFrom = len(c.messages)
	}
	c.rev++
}

// Rev 历史变更计数，见 ConversationLoop.rev。落盘侧据此做增量判断。
func (c *ConversationLoop) Rev() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.rev
}

// SetMessages 整体替换消息历史（上下文压缩后回写）。
func (c *ConversationLoop) SetMessages(messages []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setMessagesLocked(messages)
}

// SetLastTurnDuration 把本轮耗时（毫秒）写到最新一条 assistant 消息上，
// 供客户端在气泡下显示"耗时 12s"。
//
// 找不到 assistant 消息时（回合在产出回复前就失败/被中断）什么都不做。
// 写成功要自增 rev：落盘侧靠它判断历史有变，否则这一字段不会进库。
func (c *ConversationLoop) SetLastTurnDuration(durationMs int64) {
	if durationMs <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role != RoleAssistant {
			continue
		}
		if c.messages[i].DurationMs == durationMs {
			return
		}
		c.messages[i].DurationMs = durationMs

		c.rev++

		return
	}
}

// SetLastAssistantReasoning 把单次 LLM 调用的思考链写到最新一条 assistant
// 消息上，供历史重载后回看（见 core.Message.Reasoning）。
//
// 找不到 assistant 消息或思考为空时什么都不做；写成功要自增 rev，落盘侧
// 靠它判断历史有变，否则这一字段不会进库。
func (c *ConversationLoop) SetLastAssistantReasoning(reasoning string) {
	if reasoning == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role != RoleAssistant {
			continue
		}
		if c.messages[i].Reasoning == reasoning {
			return
		}
		c.messages[i].Reasoning = reasoning

		c.rev++

		return
	}
}

// setMessagesLocked 替换历史并重算检查点，调用方须已持有写锁。
func (c *ConversationLoop) setMessagesLocked(messages []Message) {
	c.messages = cloneMessages(messages)
	c.recomputeRetrySafeFromLocked()
	c.rev++
}

// cloneMessages 复制消息切片：回合内会持续 append，调用方拿到的必须是
// 独立副本，否则可能读到扩容中的半成品 slice。
//
// 空输入返回空切片而非 nil —— 会话历史会直接序列化进 RPC 响应，
// nil 会变成 JSON null（客户端按数组处理的代码会崩）。
func cloneMessages(in []Message) []Message {
	out := make([]Message, len(in))
	copy(out, in)

	return out
}

// isToolRelated 判断消息是否与工具执行有关。
// 失败的工具调用同样计入：写入类工具报错时副作用可能已经发生。
func isToolRelated(msg Message) bool {
	return len(msg.ToolCalls) > 0 || msg.Role == RoleTool
}

// recomputeRetrySafeFromLocked 按当前历史重算检查点，调用方须已持有锁。
// 整体替换历史（加载持久化 / 压缩回写）后必须重算，否则检查点会与历史脱节。
func (c *ConversationLoop) recomputeRetrySafeFromLocked() {
	c.retrySafeFrom = 0
	for i, msg := range c.messages {
		if isToolRelated(msg) {
			c.retrySafeFrom = i + 1
		}
	}
}

// SafeRollbackStart 返回检查点（可安全回滚的最早消息下标）。
func (c *ConversationLoop) SafeRollbackStart() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.retrySafeFrom
}

// LastUserIndex 返回最后一条 user 消息的下标，没有则返回 -1。
func (c *ConversationLoop) LastUserIndex() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.lastUserIndexLocked()
}

// lastUserIndexLocked 的最后一条 user 消息下标，调用方须已持有读锁。
func (c *ConversationLoop) lastUserIndexLocked() int {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == RoleUser {
			return i
		}
	}

	return -1
}

// LastAssistantIndex 返回最后一条 assistant 消息的下标，没有则返回 -1。
func (c *ConversationLoop) LastAssistantIndex() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.lastAssistantIndexLocked()
}

// lastAssistantIndexLocked 的最后一条 assistant 消息下标，调用方须已持有读锁。
func (c *ConversationLoop) lastAssistantIndexLocked() int {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == RoleAssistant {
			return i
		}
	}

	return -1
}

// lastMessageIsAssistantLocked 判断历史尾部是否为 assistant 回答，须已持有读锁。
func (c *ConversationLoop) lastMessageIsAssistantLocked() bool {
	n := len(c.messages)

	return n > 0 && c.messages[n-1].Role == RoleAssistant
}

// CanRetry 判断当前是否允许重试：存在用户消息且它位于检查点之后。
// 该回合一旦调用过工具，目标就落在检查点之前，重试会被拒绝。
func (c *ConversationLoop) CanRetry() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := c.lastUserIndexLocked()

	return idx >= 0 && idx >= c.retrySafeFrom
}

// CanRegenerate 判断当前是否允许重新生成：尾部是一条未使用工具的 assistant 回答。
func (c *ConversationLoop) CanRegenerate() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.lastMessageIsAssistantLocked() {
		return false
	}
	n := len(c.messages)

	return n-1 >= c.retrySafeFrom
}

// RollbackForRetry 回滚到最后一个用户回合之前（重试语义）。
// ok 为 false 表示不允许回滚（没有用户消息，或该回合已执行过工具）。
//
// 判据与截断分两次取锁，不是原子操作 —— 调用方（serve 的重试路径）由
// turnMu 串行，不存在同一会话的并发截断；锁在这里只保证可见性与快照不撕裂。
func (c *ConversationLoop) RollbackForRetry() ([]Message, bool) {
	c.mu.RLock()
	idx := c.lastUserIndexLocked()
	ok := idx >= 0 && idx >= c.retrySafeFrom
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	return c.truncateFrom(idx), true
}

// RollbackForRegenerate 截掉最后一条回答（重新生成语义）。
//
// 尾部是用户消息（上一条回答还没产生：失败或被中断）时返回 (nil, true)——
// 历史本身就是要重跑的状态，无需截断。尾部是工具结果时返回 false：没有
// "最后一条回答"可重跑，且任何截断点都会跨过检查点。回答执行过工具时同样
// 返回 false。
func (c *ConversationLoop) RollbackForRegenerate() ([]Message, bool) {
	c.mu.RLock()
	n := len(c.messages)
	if n == 0 {
		c.mu.RUnlock()

		return nil, false
	}
	lastRole := c.messages[n-1].Role
	// 尾部是 assistant 时才谈得上"可重新生成"：回答本身与检查点都要通过
	regeneratable := lastRole == RoleAssistant && n-1 >= c.retrySafeFrom
	c.mu.RUnlock()

	switch {
	case lastRole == RoleUser:
		// 尾部是用户消息：上一条回答还没产生（失败或被中断），
		// 历史本身就是要重跑的状态，无需截断
		return nil, true
	case regeneratable:
		return c.truncateFrom(n - 1), true
	default:
		return nil, false
	}
}

// RollbackToUserTurn 只保留前 keep 条用户消息（含第 keep 条），其后所有
// assistant / tool 消息整段丢弃，返回被丢弃的消息。
//
// 这是「从界面上某条回答处重新生成」的锚点截断：客户端传来的是该回答之前
// 的用户气泡序号（1 起），而不是消息总条数 —— 界面里可能存在未落盘的助手
// 气泡（失败 / 中断），总条数与服务端历史对不齐。
//
// 截断点固定落在用户消息之后，不会切断工具调用与结果的配对。ok 为 false
// 表示 keep 越界（历史里没有这么多条用户消息）。
func (c *ConversationLoop) RollbackToUserTurn(keep int) ([]Message, bool) {
	if keep <= 0 {
		return nil, false
	}

	c.mu.RLock()
	idx := -1
	seen := 0
	for i, m := range c.messages {
		if m.Role != RoleUser {
			continue
		}
		seen++
		if seen == keep {
			idx = i

			break
		}
	}
	c.mu.RUnlock()

	if idx < 0 {
		return nil, false
	}

	return c.truncateFrom(idx + 1), true
}

// truncateFrom 丢弃 [idx, end) 并返回被丢弃的消息。
// 调用方须先用检查点校验 idx —— 本方法只做边界保护。
func (c *ConversationLoop) truncateFrom(idx int) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	if idx < 0 || idx > len(c.messages) {
		return nil
	}

	dropped := cloneMessages(c.messages[idx:])
	c.setMessagesLocked(c.messages[:idx])

	return dropped
}

// ContinueTurn 在已有对话基础上继续调用 LLM（不添加新的用户消息）。
// 用于工具调用后获取最终回答。
func (c *ConversationLoop) ContinueTurn(ctx context.Context, llmCall func(messages []Message) (string, []ToolCallRef, error)) (*TurnResult, error) {
	// 检查预算
	if c.budget != nil && c.budget.IsExhausted() {
		return &TurnResult{
			FinishReason: "budget_exhausted",
		}, nil
	}

	// 调用 LLM
	content, toolCalls, err := llmCall(c.GetMessages())
	if err != nil {
		return nil, fmt.Errorf("llm call failed: %w", err)
	}

	// 添加 assistant 消息；有工具调用则执行（终结型工具会结束本回合）
	return c.addAssistantTurn(ctx, content, toolCalls), nil
}
