package core

import (
	"fmt"
	"sync"
	"testing"
)

// 回滚 / 重试（检查点）相关的单元测试。
//
// 检查点 retrySafeFrom = 可安全回滚的最早消息下标：工具一旦执行就可能已有
// 副作用，重跑不安全，所以它之前的消息一律不许回滚。

func newRollbackTestLoop() *ConversationLoop {
	return NewConversationLoop(NewIterationBudget(10), nil, nil)
}

func addTestMessages(c *ConversationLoop, msgs ...Message) {
	for _, m := range msgs {
		c.AddMessage(m)
	}
}

func testUser(text string) Message {
	return Message{Role: RoleUser, Content: text}
}

func testAssistant(text string) Message {
	return Message{Role: RoleAssistant, Content: text}
}

func testAssistantWithToolCall() Message {
	return Message{
		Role:      RoleAssistant,
		ToolCalls: []ToolCallRef{{ID: "call_1", Type: "function"}},
	}
}

func testToolResult() Message {
	return Message{Role: RoleTool, Content: "ok", ToolCallID: "call_1"}
}

// TestRetrySafeFromTracksToolMessages 验证检查点随工具消息推进。
func TestRetrySafeFromTracksToolMessages(t *testing.T) {
	c := newRollbackTestLoop()
	if got := c.SafeRollbackStart(); got != 0 {
		t.Fatalf("空历史的检查点应为 0，实际 %d", got)
	}

	// 纯文本往返不推高检查点：整段历史都可回滚
	addTestMessages(c, testUser("你好"), testAssistant("你好，有什么可以帮你"))
	if got := c.SafeRollbackStart(); got != 0 {
		t.Fatalf("纯文本历史的检查点应为 0，实际 %d", got)
	}

	// 带工具调用的 assistant 会把检查点推到它之后
	addTestMessages(c, testUser("查一下"), testAssistantWithToolCall())
	if got, want := c.SafeRollbackStart(), 4; got != want {
		t.Fatalf("工具调用后检查点应为 %d，实际 %d", want, got)
	}

	// 工具结果同样计入（失败的工具调用也走这条路径）
	addTestMessages(c, testToolResult())
	if got, want := c.SafeRollbackStart(), 5; got != want {
		t.Fatalf("工具结果后检查点应为 %d，实际 %d", want, got)
	}
}

// TestSetMessagesRecomputesRetrySafeFrom 验证整体替换历史后检查点被重算
//（加载持久化历史与上下文压缩回写都走这条路）。
func TestSetMessagesRecomputesRetrySafeFrom(t *testing.T) {
	c := newRollbackTestLoop()
	addTestMessages(c, testUser("一"), testAssistantWithToolCall(), testToolResult())

	// 压缩回写为纯文本摘要：检查点必须回落到 0，否则新历史会被误判为不可回滚
	c.SetMessages([]Message{
		{Role: RoleSystem, Content: "早前对话摘要"},
		testUser("二"),
		testAssistant("好的"),
	})
	if got := c.SafeRollbackStart(); got != 0 {
		t.Fatalf("纯文本历史的检查点应为 0，实际 %d", got)
	}

	// 反向：替换为带工具的历史，检查点必须重新推进
	c.SetMessages([]Message{testUser("三"), testAssistantWithToolCall(), testToolResult()})
	if got, want := c.SafeRollbackStart(), 3; got != want {
		t.Fatalf("含工具历史的检查点应为 %d，实际 %d", want, got)
	}
}

// TestRollbackForRetry 验证重试回滚：截断到最后一个用户回合之前，
// 该回合用过工具时拒绝。
func TestRollbackForRetry(t *testing.T) {
	// 干净历史：允许，且只剩到上一条用户消息之前
	c := newRollbackTestLoop()
	addTestMessages(c, testUser("第一问"), testAssistant("第一答"), testUser("第二问"))
	dropped, ok := c.RollbackForRetry()
	if !ok {
		t.Fatal("纯文本回合应允许重试")
	}
	if len(dropped) != 1 || len(c.GetMessages()) != 2 {
		t.Fatalf("应只丢弃最后一条用户消息：dropped=%d left=%d", len(dropped), len(c.GetMessages()))
	}

	// 该回合用过工具：拒绝，历史原样不动
	c2 := newRollbackTestLoop()
	addTestMessages(c2, testUser("第一问"), testAssistant("第一答"),
		testUser("查一下"), testAssistantWithToolCall(), testToolResult())
	if _, ok := c2.RollbackForRetry(); ok {
		t.Fatal("用过工具的回合不得回滚")
	}
	if got := len(c2.GetMessages()); got != 5 {
		t.Fatalf("拒绝回滚时历史应保持不变，实际 %d 条", got)
	}

	// 没有用户消息：拒绝
	c3 := newRollbackTestLoop()
	addTestMessages(c3, testAssistant("没有用户消息"))
	if _, ok := c3.RollbackForRetry(); ok {
		t.Fatal("没有用户消息时应拒绝回滚")
	}
}

// TestRollbackForRegenerate 验证重新生成：截掉最后一条回答。
func TestRollbackForRegenerate(t *testing.T) {
	// 尾部是纯文本回答：允许，截掉它
	c := newRollbackTestLoop()
	addTestMessages(c, testUser("问题"), testAssistant("回答"))
	dropped, ok := c.RollbackForRegenerate()
	if !ok || len(dropped) != 1 || len(c.GetMessages()) != 1 {
		t.Fatalf("应截掉最后一条回答：ok=%v dropped=%d left=%d", ok, len(dropped), len(c.GetMessages()))
	}

	// 尾部是用户消息（上一轮失败/中断）：无需截断，直接重跑
	c2 := newRollbackTestLoop()
	addTestMessages(c2, testUser("问题"), testAssistant("回答"), testUser("又提问"))
	dropped, ok = c2.RollbackForRegenerate()
	if !ok || dropped != nil || len(c2.GetMessages()) != 3 {
		t.Fatalf("尾部是用户消息时不该截断：ok=%v dropped=%v left=%d", ok, dropped, len(c2.GetMessages()))
	}

	// 尾部是工具结果：既没有可重跑的回答，也不存在安全的截断点
	c3 := newRollbackTestLoop()
	addTestMessages(c3, testUser("查一下"), testAssistantWithToolCall(), testToolResult())
	if _, ok := c3.RollbackForRegenerate(); ok {
		t.Fatal("尾部是工具结果时应拒绝重新生成")
	}

	// 空历史
	c4 := newRollbackTestLoop()
	if _, ok := c4.RollbackForRegenerate(); ok {
		t.Fatal("空历史应拒绝重新生成")
	}
}

// TestCanRetryAndCanRegenerate 验证下发给客户端的可行性判断。
func TestCanRetryAndCanRegenerate(t *testing.T) {
	// 纯文本回答：两个入口都可用
	c := newRollbackTestLoop()
	addTestMessages(c, testUser("问题"), testAssistant("回答"))
	if !c.CanRetry() || !c.CanRegenerate() {
		t.Fatalf("纯文本回答应同时支持重试与重新生成：retry=%v regen=%v", c.CanRetry(), c.CanRegenerate())
	}
	if c.LastUserIndex() != 0 || c.LastAssistantIndex() != 1 {
		t.Fatalf("下标定位错误：user=%d assistant=%d", c.LastUserIndex(), c.LastAssistantIndex())
	}

	// 工具轮中途失败的形态：都不允许
	c2 := newRollbackTestLoop()
	addTestMessages(c2, testUser("查一下"), testAssistantWithToolCall(), testToolResult())
	if c2.CanRetry() || c2.CanRegenerate() {
		t.Fatalf("含工具的历史不得支持回滚：retry=%v regen=%v", c2.CanRetry(), c2.CanRegenerate())
	}

	// 空历史
	c3 := newRollbackTestLoop()
	if c3.CanRetry() || c3.CanRegenerate() {
		t.Fatal("空历史不得支持回滚")
	}
}

// TestIterationBudgetResetPerTurn 验证预算是"每回合"语义。
//
// 回归背景：预算实例是 per-Agent 的，此前从不重置 —— 长会话累计跑满
// MaxIterations 后，后续每个回合都会在预算检查处直接返回空结果
//（静默空回复且不发 error 事件）。
func TestIterationBudgetResetPerTurn(t *testing.T) {
	b := NewIterationBudget(2)
	if !b.Consume() || !b.Consume() {
		t.Fatal("预算内应可消费")
	}
	if b.Consume() {
		t.Fatal("超出上限后不得再消费")
	}
	if !b.IsExhausted() {
		t.Fatal("跑满后应为耗尽状态")
	}

	// 新回合开始重置，恢复可用
	b.Reset()
	if b.IsExhausted() || b.Used() != 0 || b.Remaining() != 2 {
		t.Fatalf("Reset 后应回到初始状态：used=%d remaining=%d", b.Used(), b.Remaining())
	}
	if !b.Consume() {
		t.Fatal("Reset 后应可继续消费")
	}
}

// TestConversationLoopHistoryCopyIsIsolated 验证历史读取返回独立副本。
//
// 落盘快照与 session.history 都会读历史，必须拿到稳定副本：直接暴露
// 底层 slice 会与回合内的 append 争用同一数组。
func TestConversationLoopHistoryCopyIsIsolated(t *testing.T) {
	c := newRollbackTestLoop()
	addTestMessages(c, testUser("一"))

	snapshot := c.GetMessages()
	if got, want := len(snapshot), 1; got != want {
		t.Fatalf("快照条数应为 %d，实际 %d", want, got)
	}

	// 外部改写快照不得影响内部历史
	snapshot[0] = testUser("被篡改")
	if got := c.GetMessages()[0].Content; got != "一" {
		t.Fatalf("外部改写快照污染了内部历史：%q", got)
	}

	// 内部追加也不应改变已发出的快照长度
	addTestMessages(c, testAssistant("二"))
	if got := len(snapshot); got != 1 {
		t.Fatalf("快照应保持独立，实际长度 %d", got)
	}

	// 空历史返回空切片而非 nil：会直接进 RPC 响应，nil 会被序列化为 null
	if got := newRollbackTestLoop().GetMessages(); got == nil {
		t.Fatal("空历史应返回空切片（JSON 为 []），不能是 nil")
	}
}

// TestConversationLoopConcurrentHistoryAccess 并发读写历史不 panic。
//
// 真正的竞态检测需要 -race；本用例保证并发下结构与逻辑自洽（条数不丢）。
func TestConversationLoopConcurrentHistoryAccess(t *testing.T) {
	c := newRollbackTestLoop()

	const writers, rounds = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				c.AddMessage(testUser(fmt.Sprintf("u-%d-%d", i, j)))
				_ = c.GetMessages()
				_ = c.CanRetry()
				_ = c.SafeRollbackStart()
				_ = c.LastAssistantIndex()
			}
		}(i)
	}
	wg.Wait()

	if got, want := len(c.GetMessages()), writers*rounds; got != want {
		t.Fatalf("并发追加后条数应为 %d，实际 %d", want, got)
	}
}
