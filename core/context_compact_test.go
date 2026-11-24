package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// testCompactor 用会话级状态容器模拟 Agent 的压缩执行路径。
type testCompactor struct {
	store     *CompactStore
	maxTokens int
	summarize SummarizeFunc
}

func newTestCompactor(maxTokens int, summarize SummarizeFunc) *testCompactor {
	return &testCompactor{store: NewCompactStore(), maxTokens: maxTokens, summarize: summarize}
}

// Compress 执行一次压缩并把新状态写回容器（与 Agent.compactOnce 同构）。
func (c *testCompactor) Compress(messages []Message) ([]Message, error) {
	state, hist := c.store.Snapshot()
	res := Compress(context.Background(), CompactionRequest{
		Messages:  messages,
		State:     state,
		History:   hist,
		MaxTokens: c.maxTokens,
		Summarize: c.summarize,
	})
	c.store.Replace(res.State)
	return res.Messages, nil
}

// stubSummarize 构造一个假的摘要通道：回放固定输出并统计调用次数。
func stubSummarize(output string, calls *int) SummarizeFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		if calls != nil {
			*calls++
		}
		return output, nil
	}
}

// validSegmentOutput 一份合法的段摘要输出（含回改块）。
func validSegmentOutput(segNo int, keyPoint string) string {
	return reviseMarker + "\n" + unchangedMark + "\n" + summaryMarker + "\n" +
		secKeyPoints + "：" + keyPoint + " [锚点:张三]\n" +
		secDecisions + "：走猎聘渠道\n" +
		secCausality + "：初筛通过 → 约面\n" +
		secTodos + "：等新简历\n" +
		secUserWords + "：「薪资上限30k」\n" +
		secStateDelta + "：张三·面试状态 未开始→约面\n"
}

// seedEntries 构造 n 条 user/assistant 交替的原文消息；pad 控制单条体量
// （字符数），保证总估算 token 能越过测试阈值。
func seedEntries(n, pad int) []Message {
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			msgs = append(msgs, Message{Role: RoleUser, Content: fmt.Sprintf("用户消息 %d：候选人张三，查一下进度 %s", i, strings.Repeat("文", pad))})
		} else {
			msgs = append(msgs, Message{Role: RoleAssistant, Content: fmt.Sprintf("助手回复 %d：已查询，进度正常 %s", i, strings.Repeat("告", pad))})
		}
	}
	return msgs
}

// TestCompactAssemblesZones 组装式压缩的主路径：产出应含快照/段摘要 system
// 消息、锚点（user 原文）与保留尾。
func TestCompactAssemblesZones(t *testing.T) {
	compressor := newTestCompactor(1200, nil)
	calls := 0
	compressor.summarize = stubSummarize(validSegmentOutput(1, "张三进度正常"), &calls)

	messages := seedEntries(60, 100)
	out, err := compressor.Compress(messages)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if calls == 0 {
		t.Fatal("应至少调用一次摘要通道")
	}
	if out[0].Role != RoleSystem || !strings.Contains(out[0].Content, "[历史分段摘要]") {
		t.Fatalf("首条应为摘要区 system 消息，实际 %+v", out[0])
	}
	if !strings.Contains(out[0].Content, "[当前状态快照]") {
		t.Fatalf("快照区缺失: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "张三·面试状态：约面") {
		t.Fatalf("状态变化未被折叠进快照: %q", out[0].Content)
	}

	// 首条 user 消息必须以锚点原文形态保留
	found := false
	for _, m := range out {
		if m.Role == RoleUser && strings.Contains(m.Content, "用户消息 0") {
			found = true
		}
	}
	if !found {
		t.Fatal("首条 user 消息（硬锚点）应在压缩产物中保留原文")
	}
	assertToolPairsIntact(t, out)
}

// TestCompactReusesSegmentsOnSecondCall 第二次触发压缩时，无新段则不再调用
// 摘要通道（段冻结 + 幂等）。
func TestCompactReusesSegmentsOnSecondCall(t *testing.T) {
	compressor := newTestCompactor(1200, nil)
	calls := 0
	compressor.summarize = stubSummarize(validSegmentOutput(1, "张三进度正常"), &calls)

	messages := seedEntries(60, 100)
	if _, err := compressor.Compress(messages); err != nil {
		t.Fatalf("第一次压缩失败: %v", err)
	}
	first := calls

	// 同一批消息再次压缩（仍超阈值）：段已覆盖，不应产生新的摘要调用
	if _, err := compressor.Compress(messages); err != nil {
		t.Fatalf("第二次压缩失败: %v", err)
	}
	if calls != first {
		t.Fatalf("无新段不应再次调用摘要通道，首次 %d 次、二次后 %d 次", first, calls)
	}
}

// TestCompactFallsBackWhenSummarizerFails 摘要通道失败必须降级为旧行为，
// 不能丢回合、不能产出空历史。
func TestCompactFallsBackWhenSummarizerFails(t *testing.T) {
	compressor := newTestCompactor(1200, nil)
	compressor.summarize = func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("模拟 LLM 故障")
	}

	messages := seedEntries(60, 200)
	out, err := compressor.Compress(messages)
	if err != nil {
		t.Fatalf("降级路径不应报错: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("降级产物不应为空")
	}
	// 旧行为的摘要消息以 [Context summary] 开头
	if out[0].Role != RoleSystem || !strings.Contains(out[0].Content, "[Context summary]") {
		t.Fatalf("降级产物应为机械截断摘要，实际首条 %+v", out[0])
	}
}

// TestCompactRealignmentInvalidatesSegments 段尾指纹对不齐时，该段及之后
// 作废并重新总结（幂等重总结，原文都在）。
func TestCompactRealignmentInvalidatesSegments(t *testing.T) {
	entries := seedEntries(20, 10)

	segs := []SegmentSummary{
		{Summary: "[S1]\n" + secKeyPoints + "：旧段", EndRole: "assistant", EndText: "不存在的内容"},
	}
	got, frontier := realignSegments(segs, entries)
	if len(got) != 0 {
		t.Fatalf("指纹对不齐的段应作废，实际保留 %d 段", len(got))
	}
	if frontier != 0 {
		t.Fatalf("作废后前沿应回到 0，实际 %d", frontier)
	}

	ok := []SegmentSummary{
		{Summary: "[S1]\n" + secKeyPoints + "：好段", EndRole: "assistant", EndText: "助手回复 3"},
	}
	got, frontier = realignSegments(ok, entries)
	if len(got) != 1 || frontier != 4 {
		t.Fatalf("指纹命中应保留该段并对齐到段尾之后，实际 %d 段前沿 %d", len(got), frontier)
	}
}

// TestParseSegmentOutput 输出契约解析：缺 NEW_SUMMARY 块视为失败；回改块
// 不合法时保留旧摘要。
func TestParseSegmentOutput(t *testing.T) {
	good := reviseMarker + "\n" + unchangedMark + "\n" + summaryMarker + "\n" + secKeyPoints + "：ok"
	revised, summary, ok := parseSegmentOutput(good)
	if !ok || revised != "" || !strings.Contains(summary, secKeyPoints) {
		t.Fatalf("合法输出解析失败: ok=%v revised=%q summary=%q", ok, revised, summary)
	}

	noSummary := reviseMarker + "\n" + unchangedMark
	if _, _, ok := parseSegmentOutput(noSummary); ok {
		t.Fatal("缺 NEW_SUMMARY 块应解析失败")
	}

	badRevise := reviseMarker + "\n垃圾内容\n" + summaryMarker + "\n" + secKeyPoints + "：ok"
	revised, _, ok = parseSegmentOutput(badRevise)
	if !ok || revised != "" {
		t.Fatalf("回改块不合法应静默保留旧摘要: ok=%v revised=%q", ok, revised)
	}
}

// TestFoldSnapshot 状态快照折叠：按段序应用状态变化行。
func TestFoldSnapshot(t *testing.T) {
	prev := "· 张三·意向薪资：25k"
	segs := []SegmentSummary{
		{Summary: secKeyPoints + "：x\n" + secStateDelta + "：张三·意向薪资 25k→30k\n" + secStateDelta + "：职位·JD 草稿→定稿"},
		{Summary: secKeyPoints + "：y\n" + secStateDelta + "：张三·面试状态 未开始→约面"},
	}
	got := foldSnapshot(prev, segs)
	for _, want := range []string{"张三·意向薪资：30k", "职位·JD：定稿", "张三·面试状态：约面"} {
		if !strings.Contains(got, want) {
			t.Fatalf("快照缺 %q，实际:\n%s", want, got)
		}
	}
}

// TestDecaySegment 远段衰减只留关键点行；解析不出时整段保留。
func TestDecaySegment(t *testing.T) {
	seg := SegmentSummary{Summary: "[S1]\n" + secKeyPoints + "：要点一\n" + secDecisions + "：决策一"}
	decayed := decaySegment(seg, true)
	if !strings.Contains(decayed, "要点一") || strings.Contains(decayed, "决策一") {
		t.Fatalf("远段应只保留关键点行，实际:\n%s", decayed)
	}

	bare := SegmentSummary{Summary: "[S1]\n自由发挥的文本"}
	if decaySegment(bare, true) != bare.Summary {
		t.Fatal("解析不出关键点的段应整段保留")
	}
}

// TestCompactWithoutSummarizerUsesLegacy 未注入摘要通道时保持旧行为
// （机械截断摘要），CLI / 测试环境不受影响。
func TestCompactWithoutSummarizerUsesLegacy(t *testing.T) {
	compressor := newTestCompactor(600, nil)
	messages := []Message{
		{Role: RoleUser, Content: strings.Repeat("长", 3000)},
		{Role: RoleAssistant, Content: strings.Repeat("复", 3000)},
	}
	out, err := compressor.Compress(messages)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if out[0].Role != RoleSystem || !strings.Contains(out[0].Content, "[Context summary]") {
		t.Fatalf("无摘要通道应走旧行为，实际首条 %+v", out[0])
	}
}
