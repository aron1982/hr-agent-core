package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// 上下文压缩：在消息历史超出阈值时重新组装上下文。
//
// Phase-1 设计（CONTEXT_COMPRESSION_DESIGN.md）：压缩不再是"就地截断"，
// 而是"重新组装"——
//
//	[状态快照] + [分段摘要] + [硬锚点(原文)] + [保留尾(原文)]
//
// 三道防线各杀一个失效模式：
//   - 单跳杀累积：每段摘要只从归档原文一跳生成，生成后冻结、只增不改
//     （仅允许随下一段总结回改最近一段，见设计文档 §4.6）；禁止"摘要的摘要"。
//   - 锚点杀遗漏：user 消息等高价值信息免检保留原文；段摘要强制携带检索锚点词。
//   - 校验兜底（Phase 2，未实施）：写手不判卷，由廉价判断模型批量校验。
//
// 原文来源：HistorySource（serve 注入，读 message_archive 归档，append-only，
// 压缩永不污染）。未注入时回退为当前内存消息（CLI / 测试路径）。
//
// 本文件只保留无状态函数：状态由会话级 CompactStore 持有，模型参数
//（阈值 / 摘要通道）由调用方每次传入。

// CompactStore 会话级压缩状态容器：跨 agent 重建共享。
//
// 状态只由会话持有，agent 每次压缩时取快照、写回结果 —— 切模型重建 agent
// 不会丢段，也不需要做状态序列化迁移。
type CompactStore struct {
	mu      sync.Mutex
	state   CompactState
	history HistorySourceFunc
}

// CompactCoordinator 由 serve 实现：协调"谁来执行本会话的压缩"。
// Acquire 供前台阻塞等待，TryAcquire 供后台非阻塞抢占。
type CompactCoordinator interface {
	Acquire(ctx context.Context) (release func(), err error)
	TryAcquire() (release func(), ok bool)
	Store() *CompactStore
}

// CompactionRequest 一次压缩的全部输入（无状态）。
//
// 模型相关参数（MaxTokens / Summarize）由调用方每次现取，因此切模型无需
// 热更新任何配置。
type CompactionRequest struct {
	Messages  []Message         // 当前内存历史（组装与保留尾的来源）
	State     CompactState      // 会话级状态副本
	History   HistorySourceFunc // 归档原文；nil 时回退 Messages
	MaxTokens int               // 阈值 = 模型窗口 × 2/3
	Summarize SummarizeFunc     // 摘要通道；nil → 直接 legacy
}

// CompactionResult 压缩结果。
type CompactionResult struct {
	Messages []Message     // 组装后的上下文（后台路径可忽略）
	State    CompactState  // 新状态（含本次新增段）
	Applied  bool          // 是否真的产出了更短的上下文
}

// 预算常量。单位均为估算 token（4 字符 ≈ 1 token）。
const (
	// segmentMaxTokens 单段原文的估算上限：约 50 轮对话的体量。
	segmentMaxTokens = 6000
	// segmentMinTokens 已有段的情况下，末段不足此数并入前一段（避免碎段）。
	segmentMinTokens = 2500
	// overlapEntries / overlapMaxTokens 跨段重叠：总结段 N 时带上段 N-1 的
	// 尾部原文，避免因果链被段边界切断（设计文档 §4.6）。
	overlapEntries   = 8
	overlapMaxTokens = 2000
	// tailReserveRatio 序列尾部预留比例的分母：这部分大概率仍以原文形态
	// 留在内存（保留尾 + 锚点），不为它们切段。预留 = 阈值 / 该值。
	//
	// 取 3 而非 2：必须与 assembleContext 的 tailBudget（阈值 / 3）一致。
	// 预留区大于保留尾预算时，中间那截既不会被总结、也不会被原文保留
	// （assistant / tool 消息直接丢失）。
	tailReserveRatio = 3
	// segmentPromptChars 单条消息进摘要 prompt 的截断长度：摘要要的是语义，
	// 不是全文回显；超长工具输出在归档里有原文，靠锚点词可检索。
	segmentPromptChars = 2000
	// farDecayNear 近段数量：最近的 N 段摘要在上下文里保留全文，更早的
	// 只保留关键点行（远段衰减，设计文档 §4.7）。
	farDecayNear = 3
	// summaryBudgetRatio 段摘要渲染预算的分母：摘要最多占阈值的 1/4。
	// 超长会话段数会持续增长，超出预算时从最早的中间段开始折叠，
	// 只收窄“送给模型的上下文”，不动 state 与归档原文。
	summaryBudgetRatio = 4
)

// HistorySourceFunc 提供未被修剪/压缩污染的会话原始历史（归档原文序列）。
//
// serve 启动装配时按会话注入（与 SetSessionSearchBackend 同一套注入模式）：
// 读 message_archive，按 (role, tool_name, content_hash) 首现去重后按 rowid
// 排序——压缩回写的锚点/保留尾是原文的重复行，首现去重正好滤掉。
type HistorySourceFunc func() ([]Message, error)

// SummarizeFunc 由 Agent 注入的摘要通道：复用会话模型的转发客户端，
// 独立小请求（无系统提示词、无工具定义），不经过回合循环。
type SummarizeFunc func(ctx context.Context, prompt string) (string, error)

// SegmentSummary 单段的冻结摘要。
type SegmentSummary struct {
	// Summary 段摘要全文（含五区 schema，见 prompt_compact.go）。
	Summary string `json:"summary"`
	// EndRole / EndText 段尾消息指纹（内容前缀截断存储）：重启/回退后据此
	// 在归档原文序列中重新对齐段边界。对不齐则该段及之后按未总结处理
	// （重新总结，原文都在，幂等安全）。
	EndRole string `json:"end_role"`
	EndText string `json:"end_text"`
	// Version 生成时的 prompt 版本（prompt_compact.go compactPromptVersion）。
	Version int `json:"version"`
}

// CompactState 压缩器的可持久化状态（sessions.compact_meta）。
type CompactState struct {
	// Segments 按序排列的段摘要。相邻段可能有重叠（fix-up 回改去重后）。
	Segments []SegmentSummary `json:"segments,omitempty"`
	// Snapshot 当前状态快照：从各段的"状态变化"行按段序折叠推导，
	// 可随时重算（设计文档 §4.7）。
	Snapshot string `json:"snapshot,omitempty"`
}

// NewCompactStore 创建会话级状态容器。
func NewCompactStore() *CompactStore {
	return &CompactStore{}
}

// Snapshot 返回状态副本与归档数据源（只取函数值，不在锁内读库）。
func (s *CompactStore) Snapshot() (CompactState, HistorySourceFunc) {
	if s == nil {
		return CompactState{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCompactState(s.state), s.history
}

// Replace 整体替换状态（压缩结果写回）。
func (s *CompactStore) Replace(st CompactState) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.state = cloneCompactState(st)
	s.mu.Unlock()
}

// SetHistory 注入归档原文数据源（会话装配时调用一次）。
func (s *CompactStore) SetHistory(fn HistorySourceFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.history = fn
	s.mu.Unlock()
}

func cloneCompactState(st CompactState) CompactState {
	out := CompactState{Snapshot: st.Snapshot}
	if len(st.Segments) > 0 {
		out.Segments = make([]SegmentSummary, len(st.Segments))
		copy(out.Segments, st.Segments)
	}
	return out
}

// SummarizedTokens 估算状态已覆盖的原文 token 量（段数 × 单段上限，粗估）。
// 仅供调度层判断“新增内容是否值得再预热”，不参与压缩算法本身。
func (st CompactState) SummarizedTokens() int {
	return len(st.Segments) * segmentMaxTokens
}

// Compress 压缩入口（无状态）：所有输入来自 req，结果通过返回值交回调用方。
func Compress(ctx context.Context, req CompactionRequest) CompactionResult {
	if len(req.Messages) == 0 {
		return CompactionResult{Messages: req.Messages, State: req.State}
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 100000
	}

	// 未超出阈值，无需压缩
	if estimateTotalTokens(req.Messages) <= req.MaxTokens {
		return CompactionResult{Messages: req.Messages, State: req.State}
	}

	if req.Summarize != nil {
		if assembled, st, ok := compact(ctx, req); ok {
			return CompactionResult{Messages: assembled, State: st, Applied: true}
		}
	}

	out, _ := legacyCompress(req.Messages, req.MaxTokens)
	return CompactionResult{Messages: out, State: req.State, Applied: true}
}

// compact 组装式压缩（无状态）。返回 ok=false 表示本次走降级路径。
func compact(ctx context.Context, req CompactionRequest) ([]Message, CompactState, bool) {
	if req.Summarize == nil {
		return nil, req.State, false
	}

	entries := sourceEntries(req.History, req.Messages)
	if len(entries) == 0 {
		return nil, req.State, false
	}

	// 1) 段边界重对齐：回退/重生成会让归档序列分叉，按段尾指纹把已有段
	//    重新锚定到当前序列上；对不齐的段连同其后全部作废（幂等重总结）。
	st := cloneCompactState(req.State)
	segs, frontier := realignSegments(st.Segments, entries)
	st.Segments = segs

	// 2) 找出本次要总结的新段：从对齐前沿到尾部预留边界之间，按预算切段。
	boundary := tailBoundary(entries, req.MaxTokens/tailReserveRatio, estimateEntryTokens)
	ranges := planSegmentRanges(entries, frontier, boundary, len(st.Segments) > 0)
	if len(ranges) == 0 && len(st.Segments) == 0 {
		// 阈值到了却没有段可切（尾部预留吃满了序列）：交给旧行为兜底
		return nil, req.State, false
	}

	for _, r := range ranges {
		seg, revised, err := summarizeSegment(ctx, req.Summarize, entries, st.Segments, r, len(st.Segments))
		if err != nil {
			// 单段失败即整体降级：半新半旧的段序列比纯旧行为更难排查
			return nil, req.State, false
		}
		// 回改成功则替换上一段（宁重复不失真：失败时保留旧摘要原样）
		if len(st.Segments) > 0 && revised != "" {
			st.Segments[len(st.Segments)-1].Summary = revised
		}
		st.Segments = append(st.Segments, seg)
	}

	// 3) 状态快照：从段摘要的"状态变化"行按序折叠（纯机械解析，零 LLM 成本）
	st.Snapshot = foldSnapshot(st.Snapshot, st.Segments)

	// 段状态随结果交回：即使下面组装没有收益，已生成的段也保留，
	// 下次压缩直接复用，不会重复调用 LLM。
	// 4) 组装上下文
	final := assembleContext(req.Messages, st, req.MaxTokens, estimateMessageTokens)
	if len(final) == 0 || estimateTotalTokens(final) >= estimateTotalTokens(req.Messages) {
		// 组装没有带来收益（极少见：阈值刚过 + 消息极少）——原样返回
		return req.Messages, st, true
	}

	return final, st, true
}

// legacyCompress 旧行为：尾部按 token 预算保留，更早的消息机械截断做摘要。
// 作为摘要通道不可用/失败时的降级路径保留。
func legacyCompress(messages []Message, maxTokens int) ([]Message, error) {
	// 从后向前保留消息，直到接近 token 限制
	// 前面的消息用摘要替换
	var kept []Message
	tokenBudget := maxTokens

	// 预留摘要 token（约 500 tokens）
	tokenBudget -= 500
	if tokenBudget < 0 {
		tokenBudget = 0
	}

	// 从尾部开始收集
	for i := len(messages) - 1; i >= 0; i-- {
		msgTokens := estimateMessageTokens(messages[i])
		if msgTokens <= tokenBudget {
			kept = append([]Message{messages[i]}, kept...)
			tokenBudget -= msgTokens
		} else {
			// 无法容纳更多消息，生成摘要并停止。kept 为空（首条就装不下）
			// 也要生成摘要——返回空历史会让调用方整体放弃压缩
			if i >= 0 {
				summary := summarizeMessages(messages[:i+1])
				kept = append([]Message{summary}, kept...)
			}
			break
		}
	}

	// 逐条按 token 预算截断很容易把 assistant.tool_calls 与它的 tool 结果
	// 拆开，而 OpenAI 兼容端点见到孤儿 tool 消息会直接 400 —— 压缩产物必须
	// 成对，否则"压缩成功"换来的是下一次请求失败。
	kept, _ = sanitizeToolPairs(kept)

	return kept, nil
}

// sourceEntries 返回压缩依据的原文序列：归档优先，回退当前消息。
func sourceEntries(history HistorySourceFunc, fallback []Message) []Message {
	if history != nil {
		if entries, err := history(); err == nil && len(entries) > 0 {
			return entries
		}
	}
	return fallback
}

// realignSegments 按段尾指纹把段摘要重新锚定到归档原文序列上。
// 返回有效段（对不齐的段连同其后作废）与对齐前沿（最后一段的段尾下标 + 1）。
func realignSegments(segs []SegmentSummary, entries []Message) ([]SegmentSummary, int) {
	cursor := 0
	for i, seg := range segs {
		found := -1
		for j := cursor; j < len(entries); j++ {
			if string(entries[j].Role) == seg.EndRole && strings.HasPrefix(entries[j].Content, seg.EndText) {
				found = j
				break
			}
		}
		if found < 0 {
			// 边界失效：该段及之后重新总结（原文都在归档里，幂等）
			return segs[:i], cursor
		}
		cursor = found + 1
	}
	return segs, cursor
}

// tailBoundary 从序列尾部预留 reserve 估算 token，返回预留区起始下标。
// 该区域大概率仍以原文形态留在内存（保留尾+锚点），不为它们切段。
func tailBoundary(entries []Message, reserve int, estimate func(Message) int) int {
	acc := 0
	for i := len(entries) - 1; i >= 0; i-- {
		acc += estimate(entries[i])
		if acc > reserve {
			return i + 1
		}
	}
	return 0
}

// planSegmentRanges 在 [frontier, boundary) 内按预算切出段范围。
// 首段无最小限制（首次压缩时再小的头部也必须被总结，否则组装时会丢原文）；
// 已有段的情况下末段过小则并入前一段（末段起点让给前一段延伸）。
//
// 切点按“消息组”对齐：assistant.tool_calls 与它的 tool 结果必须落在同一段。
// 否则段尾是带调用的 assistant、下一段以 tool 开头（或反之），组装后会留下
// 孤立 tool 消息 —— OpenAI 兼容端点会直接 400。
func planSegmentRanges(entries []Message, frontier, boundary int, hasSegments bool) [][2]int {
	if frontier >= boundary {
		return nil
	}

	// alignCut 把候选切点后移，跳过紧跟其后的 tool 结果。
	// 返回的切点保证 entries[cut] 不是 tool 消息（或已到 limit / 序列末尾）。
	alignCut := func(p, limit int) int {
		for p < limit && p < len(entries) && entries[p].Role == RoleTool {
			p++
		}
		return p
	}

	// 贪心切段：累计到 segmentMaxTokens 即收口
	var cuts []int
	acc := 0
	for k := frontier; k < boundary; k++ {
		acc += estimateEntryTokens(entries[k])
		if acc < segmentMaxTokens {
			continue
		}
		// 跳过工具组内部，避免把 assistant 与它的 tool 结果切到两段
		cut := alignCut(k+1, boundary)
		if cut >= boundary {
			// 工具组跨过尾部预留边界：本刀不切，剩余内容统一并入末段
			break
		}
		cuts = append(cuts, cut)
		acc = 0
		k = cut - 1
	}

	// 末段终点也要对齐：boundary 落在工具组内部时向后扩到组结束，
	// 保证最后一段不会以带 tool_calls 的 assistant 收尾。
	tailEnd := alignCut(boundary, len(entries))

	// 收尾：末段过小且已有段时并入前一段
	if len(cuts) > 0 && hasSegments {
		last := cuts[len(cuts)-1]
		rest := estimateEntryRange(entries, last, tailEnd)
		if rest < segmentMinTokens {
			cuts = cuts[:len(cuts)-1]
		}
	}

	cuts = append(cuts, tailEnd)
	ranges := make([][2]int, 0, len(cuts))
	prev := frontier
	for _, end := range cuts {
		if end > prev {
			ranges = append(ranges, [2]int{prev, end})
		}
		prev = end
	}
	return ranges
}

// estimateEntryTokens 估算单条归档原文的 token（归档无附件元数据，按正文算）。
func estimateEntryTokens(m Message) int {
	return EstimateTokensByChars(len(m.Content) + len(m.Name))
}

func estimateEntryRange(entries []Message, start, end int) int {
	total := 0
	for k := start; k < end && k < len(entries); k++ {
		total += estimateEntryTokens(entries[k])
	}
	return total
}

// summarizeSegment 总结一个新段：输入 = 重叠区原文 + 段原文 + 上一段摘要；
// 输出 = 回改后的上一段摘要（可选）+ 新段摘要。
func summarizeSegment(
	ctx context.Context,
	summarize SummarizeFunc,
	entries []Message,
	segs []SegmentSummary,
	r [2]int,
	segCount int,
) (seg SegmentSummary, revisedPrev string, err error) {
	start, end := r[0], r[1]

	// 重叠区：上一段的尾部原文（预算超了从头丢，保尾部——越靠近段边界越相关）
	overlapStart := start - overlapEntries
	if overlapStart < 0 {
		overlapStart = 0
	}
	overlap := entries[overlapStart:start]
	for len(overlap) > 1 && estimateEntryRange(entries, overlapStart, overlapStart+len(overlap)) > overlapMaxTokens {
		overlap = overlap[1:]
		overlapStart++
	}

	var prevSummary string
	if len(segs) > 0 {
		prevSummary = segs[len(segs)-1].Summary
	}

	prompt := buildSegmentPrompt(
		segCount+1,
		prevSummary,
		renderEntriesForPrompt(overlap, "以下是上一段尾部的重叠区（仅供衔接上下文，不重复总结）"),
		renderEntriesForPrompt(entries[start:end], ""),
	)

	out, err := summarize(ctx, prompt)
	if err != nil {
		return SegmentSummary{}, "", err
	}
	revised, summary, ok := parseSegmentOutput(out)
	if !ok {
		return SegmentSummary{}, "", fmt.Errorf("段摘要输出缺少必需结构")
	}

	endMsg := entries[end-1]
	endText := endMsg.Content
	if len(endText) > 256 {
		endText = endText[:256]
	}

	return SegmentSummary{
		Summary: fmt.Sprintf("[S%d]\n%s", segCount+1, summary),
		EndRole: string(endMsg.Role),
		EndText: endText,
		Version: compactPromptVersion,
	}, revised, nil
}

// renderEntriesForPrompt 把原文序列渲染为摘要 prompt 的输入文本。
func renderEntriesForPrompt(entries []Message, header string) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	for _, m := range entries {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if len(content) > segmentPromptChars {
			content = content[:segmentPromptChars] + "…[截断，原文可检索]"
		}
		role := string(m.Role)
		if m.Name != "" {
			role += ":" + m.Name
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, content)
	}
	return b.String()
}

// assembleContext 组装压缩后的上下文：
//
//	[快照+段摘要(system)] + [锚点(user 原文)] + [保留尾(原文)]
//
// 锚点来自当前消息的 head 区（尾部预留区之外），首条 user 必保；保留尾从
// 当前消息尾部按预算截取——两者都带完整消息结构，tool 配对由
// sanitizeToolPairs 统一修复。
func assembleContext(
	messages []Message,
	st CompactState,
	maxTokens int,
	estimate func(Message) int,
) []Message {
	// 尾部：从当前消息末尾按预算往前保留
	tailBudget := maxTokens / 3
	cut := len(messages)
	acc := 0
	for cut > 0 {
		t := estimate(messages[cut-1])
		if acc+t > tailBudget && cut < len(messages) {
			break
		}
		acc += t
		cut--
	}
	head, tail := messages[:cut], messages[cut:]

	// 锚点：head 区的 user 消息，预算 min(阈值/3, 12k)；首条 user 必保
	anchorBudget := maxTokens / 3
	if anchorBudget > 12000 {
		anchorBudget = 12000
	}
	firstUser := -1
	for i := range head {
		if head[i].Role == RoleUser {
			firstUser = i
			break
		}
	}
	var anchors []Message
	aAcc := 0
	for i := range head {
		m := head[i]
		if m.Role != RoleUser || strings.TrimSpace(m.Content) == "" {
			continue
		}
		content := m.Content
		isFirst := i == firstUser
		if isFirst {
			// 首条 user 超长时保首尾，中间让位（原文可检索）
			if len(content) > 1500 {
				content = content[:1000] + "\n…[中略，可用 session_search 检索原文]…\n" + content[len(content)-500:]
			}
		} else {
			if len(content) > 1000 {
				content = content[:500] + "\n…[中略，可用 session_search 检索原文]…\n" + content[len(content)-500:]
			}
			t := EstimateTokensByChars(len(content))
			if aAcc+t > anchorBudget {
				continue
			}
			aAcc += t
		}
		anchors = append(anchors, Message{Role: RoleUser, Content: content})
	}

	summaryMsg := Message{Role: RoleSystem, Content: renderSummaryBlock(st, maxTokens/summaryBudgetRatio)}

	build := func(anchorList []Message) []Message {
		final := make([]Message, 0, 1+len(anchorList)+len(tail))
		final = append(final, summaryMsg)
		final = append(final, anchorList...)
		final = append(final, tail...)
		return final
	}

	final := build(anchors)

	// 超预算的降级顺序：丢低优先级锚点（保首条 user）→ 整体降级旧行为。
	// 快照与段摘要是唯一的历史承载，不参与裁剪。
	if estimateTokensOf(final, estimate) > maxTokens && len(anchors) > 1 {
		trimmed := build(anchors[:1])
		if estimateTokensOf(trimmed, estimate) <= maxTokens {
			final = trimmed
		}
	}

	return final
}

// renderSummaryBlock 渲染快照 + 段摘要。
//
// 段数随压缩次数持续增长，这里按预算折叠早期段，避免摘要挤占保留尾：
// 首段（会话起点）与最近 farDecayNear 段优先保留，中间段从近到远填充
// 剩余预算；被折叠的段只影响“送给模型的上下文”，state 与归档原文都还在，
// 可用 session_search 检索。
func renderSummaryBlock(st CompactState, budget int) string {
	var b strings.Builder
	if st.Snapshot != "" {
		b.WriteString("[当前状态快照]\n" + st.Snapshot + "\n\n")
	}
	if len(st.Segments) > 0 {
		b.WriteString("[历史分段摘要]\n")
		n := len(st.Segments)
		farCount := n - farDecayNear

		texts := make([]string, n)
		tokens := make([]int, n)
		for i, seg := range st.Segments {
			decayed := i < farCount
			if i == 0 {
				decayed = false // 首段是会话起点，始终保留全文
			}
			texts[i] = decaySegment(seg, decayed)
			tokens[i] = EstimateTokensByChars(len(texts[i]))
		}

		// 保留策略：首段 + 最近段优先占预算，中间段从近到远填充。
		keep := make([]bool, n)
		used := 0
		for i := 0; i < n; i++ {
			if i == 0 || i >= farCount {
				keep[i] = true
				used += tokens[i]
			}
		}
		if budget > 0 {
			for i := farCount - 1; i >= 1; i-- {
				if used+tokens[i] > budget {
					break
				}
				keep[i] = true
				used += tokens[i]
			}
		} else {
			for i := 1; i < farCount; i++ {
				keep[i] = true
			}
		}

		skipped := 0
		for i := 0; i < n; i++ {
			if !keep[i] {
				skipped++
				continue
			}
			if skipped > 0 {
				fmt.Fprintf(&b, "…（更早的 %d 段已折叠，可用 session_search 按关键词检索原文）\n", skipped)
				skipped = 0
			}
			b.WriteString(texts[i])
			b.WriteString("\n")
		}
	}
	b.WriteString("以上摘要有损，细节用 session_search 按锚点词检索原文。")
	return strings.TrimSpace(b.String())
}

// decaySegment 远段衰减：只保留关键点行；解析不出关键点时整段保留（宁多勿漏）。
func decaySegment(seg SegmentSummary, decayed bool) string {
	if !decayed {
		return seg.Summary
	}
	lines := keyPointLines(seg.Summary)
	if len(lines) == 0 {
		return seg.Summary
	}
	header := seg.Summary
	if idx := strings.Index(seg.Summary, "\n"); idx >= 0 {
		header = seg.Summary[:idx+1]
	}
	return header + strings.Join(lines, "\n")
}

func estimateTokensOf(messages []Message, estimate func(Message) int) int {
	total := 0
	for _, m := range messages {
		total += estimate(m)
	}
	return total
}

// estimateTotalTokens 估算消息列表的总 token 数
func estimateTotalTokens(messages []Message) int {
	total := 0
	for _, msg := range messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

// estimateMessageTokens 估算单条消息的 token 数（约 4 字符/token）
func estimateMessageTokens(msg Message) int {
	chars := len(msg.Content) + len(msg.Name)
	for _, tc := range msg.ToolCalls {
		chars += len(tc.Function.Name) + len(tc.Function.Arguments)
	}

	// 与 model_metadata.EstimateTokensByChars 共用同一公式（唯一实现）。
	// 图片附件另按固定值估算：视觉 token 由像素决定、与 base64 长度无关，
	// 按字节估会让带图会话被过早压缩。
	return EstimateTokensByChars(chars) + len(msg.Attachments)*imageAttachmentTokens
}

// summarizeMessages 将多条消息压缩为一条摘要消息（legacy 降级路径专用）
func summarizeMessages(messages []Message) Message {
	if len(messages) == 0 {
		return Message{}
	}

	var parts []string
	for _, msg := range messages {
		if msg.Content != "" {
			// 截取每条消息的前 200 字符作为摘要
			content := msg.Content
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			parts = append(parts, fmt.Sprintf("[%s]: %s", msg.Role, content))

			continue
		}

		// 只有图片没有正文的消息：摘要里留一个可读标记。图片本身随摘要丢弃
		//（这正是带图会话的上下文降级路径），但"曾经有图"不该凭空消失。
		if len(msg.Attachments) > 0 {
			parts = append(parts, fmt.Sprintf("[%s]: [%d 张图片]", msg.Role, len(msg.Attachments)))
		}
	}

	return Message{
		Role:    RoleSystem,
		Content: "[Context summary]\n" + strings.Join(parts, "\n"),
	}
}

// sanitizeToolPairs 修正断裂的工具调用配对。
//
//   - tool 结果找不到发起它的 assistant.tool_calls → 丢弃该结果
//   - assistant 的某个 tool_call 没有对应结果 → 从该消息剥离这个 call
//     （剥离后既无文本也无剩余 call 时，整条消息丢弃）
//
// 不保留"in-flight call"：core 的压缩只发生在请求前（prepareMessages），
// 历史里不该有结果尚未返回的调用；真出现了也是剥离更安全 —— 带着无结果的
// tool_calls 请求同样会被端点拒绝。
//
// 第二个返回值表示是否发生了修改：prepareMessages 据此决定要不要回写历史。
// 配对完好的历史（绝大多数请求）原样返回，不产生额外落盘。
func sanitizeToolPairs(messages []Message) ([]Message, bool) {
	if len(messages) == 0 {
		return messages, false
	}

	calls := make(map[string]bool)
	results := make(map[string]bool)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" {
				calls[tc.ID] = true
			}
		}
		if msg.Role == RoleTool && msg.ToolCallID != "" {
			results[msg.ToolCallID] = true
		}
	}

	changed := false
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == RoleTool {
			if msg.ToolCallID != "" && !calls[msg.ToolCallID] {
				changed = true

				continue
			}
			out = append(out, msg)

			continue
		}

		if len(msg.ToolCalls) > 0 {
			kept := make([]ToolCallRef, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				if tc.ID == "" || results[tc.ID] {
					kept = append(kept, tc)
				}
			}
			if len(kept) != len(msg.ToolCalls) {
				changed = true
			}
			if len(kept) == 0 && strings.TrimSpace(msg.Content) == "" {
				changed = true

				continue
			}
			msg.ToolCalls = kept
		}

		out = append(out, msg)
	}

	return out, changed
}
