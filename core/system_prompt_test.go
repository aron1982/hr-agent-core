package core

import (
	"context"
	"strings"
	"testing"
)

// TestBuildSystemPromptSkipsEmptySegments 空段不应在提示词里留下多余空行。
// 调用方常传 []string{""}（记忆为空），不过滤就会拼出连续空行。
func TestBuildSystemPromptSkipsEmptySegments(t *testing.T) {
	got := BuildSystemPrompt([]string{"stable", "   "}, []string{""}, []string{"volatile"})
	if got != "stable\n\nvolatile" {
		t.Fatalf("空段未被正确跳过，实际 %q", got)
	}
}

// TestSetMemoryKeepsStablePrefix 写记忆只能改动 volatile 段尾部：
// stable 段（身份 + 工具指导）与 volatile 的元信息行位置都必须保持原样，
// 否则上游前缀缓存会因为写一次记忆就整段失效。
func TestSetMemoryKeepsStablePrefix(t *testing.T) {
	agent, err := NewAgent(context.Background(), AgentConfig{
		ModelName:    "test-model",
		SystemPrompt: "CUSTOM IDENTITY",
		Tools: map[string]ToolExecutor{
			"echo": func(context.Context, string) (string, error) { return "", nil },
		},
	})
	if err != nil {
		t.Fatalf("NewAgent 失败: %v", err)
	}

	before := agent.systemPrompt()
	if !strings.HasPrefix(before, "CUSTOM IDENTITY") {
		t.Fatalf("stable 段应保留调用方身份，实际 %q", before)
	}
	if !strings.Contains(before, ToolGuidance) {
		t.Fatalf("挂了工具时应注入工具指导，实际 %q", before)
	}

	metaAt := strings.Index(before, "Conversation started")
	if metaAt < 0 {
		t.Fatalf("volatile 段应含会话起始日期，实际 %q", before)
	}

	agent.SetMemory("note", "remember-me")

	after := agent.systemPrompt()
	if !strings.HasPrefix(after, "CUSTOM IDENTITY") {
		t.Fatalf("写记忆后不应重建 stable 段，实际 %q", after)
	}
	if strings.Index(after, "Conversation started") != metaAt {
		t.Fatalf("写记忆改变了前缀（stable/元信息位置移动），会击穿前缀缓存:\nbefore=%q\nafter=%q", before, after)
	}
	if !strings.Contains(after, "remember-me") {
		t.Fatalf("记忆内容未进入提示词，实际 %q", after)
	}
}

// TestNewAgentDefaultsIdentityWithoutTools 未传身份时用默认身份兜底，
// 且没有工具时不注入工具指导。
func TestNewAgentDefaultsIdentityWithoutTools(t *testing.T) {
	agent, err := NewAgent(context.Background(), AgentConfig{ModelName: "test-model"})
	if err != nil {
		t.Fatalf("NewAgent 失败: %v", err)
	}

	prompt := agent.systemPrompt()
	if !strings.HasPrefix(prompt, DefaultAgentIdentity) {
		t.Fatalf("未配置身份时应回落到默认身份，实际 %q", prompt)
	}
	if strings.Contains(prompt, ToolGuidance) {
		t.Fatalf("没有工具时不应注入工具指导，实际 %q", prompt)
	}
}
