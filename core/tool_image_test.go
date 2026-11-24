package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeToolImageStore 假的落库出口：把附件换成带 id 的形式，并记录调用次数。
type fakeToolImageStore struct {
	persisted int
	err       error
}

func (f *fakeToolImageStore) PersistToolImage(att Attachment) (Attachment, error) {
	if f.err != nil {
		return Attachment{}, f.err
	}

	f.persisted++
	att.ID = "att-store-1"
	att.Source = "/store/" + att.Name

	return att, nil
}

// TestRunToolCallsPersistsImages 验证工具图片在进入历史之前先经落库出口，
// 历史里留下的是带附件 id 的引用（否则落盘侧会跳过它，重启后图片消失）。
func TestRunToolCallsPersistsImages(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("screenshot", func(_ context.Context, _ string) (string, error) {
		return `{"_tool_images":[{"path":"/tmp/wx.png","mime":"image/png","name":"wx.png"}]}`, nil
	})

	store := &fakeToolImageStore{}
	c := NewConversationLoop(NewIterationBudget(10), registry, nil)
	c.SetToolImageStore(store)

	call := ToolCallRef{ID: "call_1", Type: "function"}
	call.Function.Name = "screenshot"
	call.Function.Arguments = "{}"

	results := c.runToolCalls(context.Background(), []ToolCallRef{call})

	if store.persisted != 1 {
		t.Fatalf("工具图片应经落库出口一次，实际 %d 次", store.persisted)
	}
	if got := results[0].Images[0].ID; got != "att-store-1" {
		t.Fatalf("工具结果应携带落库后的附件 id，实际 %q", got)
	}

	msgs := c.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("应产生 tool + user 两条消息，实际 %d 条", len(msgs))
	}
	if got := msgs[1].Attachments[0].ID; got != "att-store-1" {
		t.Fatalf("历史里的图片消息应携带附件 id，实际 %q", got)
	}
}

// TestRunToolCallsKeepsImageWhenStoreFails 验证落库失败不丢图：
// 保留原始路径引用，本回合模型仍能看图（只是重启后不再可见）。
func TestRunToolCallsKeepsImageWhenStoreFails(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("screenshot", func(_ context.Context, _ string) (string, error) {
		return `{"_tool_images":[{"path":"/tmp/wx.png","mime":"image/png","name":"wx.png"}]}`, nil
	})

	c := NewConversationLoop(NewIterationBudget(10), registry, nil)
	c.SetToolImageStore(&fakeToolImageStore{err: errors.New("落库失败")})

	call := ToolCallRef{ID: "call_1", Type: "function"}
	call.Function.Name = "screenshot"
	call.Function.Arguments = "{}"

	results := c.runToolCalls(context.Background(), []ToolCallRef{call})

	if len(results[0].Images) != 1 {
		t.Fatalf("落库失败也不应丢图，实际 %+v", results[0].Images)
	}
	if got := results[0].Images[0].Source; got != "/tmp/wx.png" {
		t.Fatalf("应保留原始路径引用，实际 %q", got)
	}
	if results[0].Images[0].ID != "" {
		t.Fatalf("落库失败时不应有附件 id，实际 %q", results[0].Images[0].ID)
	}
}

// TestExtractToolImages 验证图片侧信道的剥离规则：只在输出确实带保留字段
// 且可解析时改写，其余情况原样返回。
func TestExtractToolImages(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantText   string
		wantImages int
	}{
		{
			name:       "无该字段的 JSON 原样返回",
			output:     `{"stdout":"ok"}`,
			wantText:   `{"stdout":"ok"}`,
			wantImages: 0,
		},
		{
			name:       "非 JSON 输出原样返回",
			output:     `_tool_images 只是正文里的一段文字`,
			wantText:   `_tool_images 只是正文里的一段文字`,
			wantImages: 0,
		},
		{
			name:       "带正文的图片输出：剥离字段保留正文",
			output:     `{"text":"截图已保存","_tool_images":[{"path":"/tmp/a.png","mime":"image/png","name":"a.png"}]}`,
			wantText:   `{"text":"截图已保存"}`,
			wantImages: 1,
		},
		{
			name:       "纯图片输出：补占位正文",
			output:     `{"_tool_images":[{"path":"/tmp/a.png"}]}`,
			wantText:   "[工具返回了图片]",
			wantImages: 1,
		},
		{
			name:       "path 为空的条目被过滤",
			output:     `{"_tool_images":[{"path":""},{"path":"/tmp/b.png"}]}`,
			wantText:   "[工具返回了图片]",
			wantImages: 1,
		},
		{
			name:       "非法 JSON 原样返回",
			output:     `{"_tool_images": [`,
			wantText:   `{"_tool_images": [`,
			wantImages: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, images := extractToolImages(tc.output)
			if text != tc.wantText {
				t.Fatalf("正文不符：want %q, got %q", tc.wantText, text)
			}
			if len(images) != tc.wantImages {
				t.Fatalf("图片数不符：want %d, got %d", tc.wantImages, len(images))
			}
		})
	}
}

// TestRunToolCallsAppendsToolImageMessage 验证工具产出的图片被剥离后，
// 作为一条独立的 user 消息追加在工具结果之后（顺序不能颠倒，
// 否则会被严格端点判为 tool_call 配对错误）。
func TestRunToolCallsAppendsToolImageMessage(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("screenshot", func(_ context.Context, _ string) (string, error) {
		return `{"text":"已截图","_tool_images":[{"path":"/tmp/wx.png","mime":"image/png","name":"wx.png"}]}`, nil
	})

	c := NewConversationLoop(NewIterationBudget(10), registry, nil)

	call := ToolCallRef{ID: "call_1", Type: "function"}
	call.Function.Name = "screenshot"
	call.Function.Arguments = "{}"

	results := c.runToolCalls(context.Background(), []ToolCallRef{call})

	if len(results) != 1 || len(results[0].Images) != 1 {
		t.Fatalf("工具结果应携带 1 张图片，实际 %+v", results)
	}

	msgs := c.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("应产生 tool + user 两条消息，实际 %d 条", len(msgs))
	}

	if msgs[0].Role != RoleTool {
		t.Fatalf("第一条应为 tool 消息，实际 %q", msgs[0].Role)
	}
	if strings.Contains(msgs[0].Content, toolImageField) {
		t.Fatalf("tool 消息不应残留图片字段：%s", msgs[0].Content)
	}
	if msgs[0].Content != `{"text":"已截图"}` {
		t.Fatalf("tool 消息正文不符：%s", msgs[0].Content)
	}

	if msgs[1].Role != RoleUser {
		t.Fatalf("第二条应为 user 消息，实际 %q", msgs[1].Role)
	}
	if len(msgs[1].Attachments) != 1 {
		t.Fatalf("user 消息应带 1 个附件，实际 %d 个", len(msgs[1].Attachments))
	}
	if got := msgs[1].Attachments[0].Source; got != "/tmp/wx.png" {
		t.Fatalf("附件路径不符：%s", got)
	}
}

// TestRunToolCallsNoImageMessageForPlainOutput 验证普通工具输出不会
// 凭空多出一条 user 消息。
func TestRunToolCallsNoImageMessageForPlainOutput(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("read_file", func(_ context.Context, _ string) (string, error) {
		return `{"content":"hello"}`, nil
	})

	c := NewConversationLoop(NewIterationBudget(10), registry, nil)

	call := ToolCallRef{ID: "call_1", Type: "function"}
	call.Function.Name = "read_file"
	call.Function.Arguments = "{}"

	c.runToolCalls(context.Background(), []ToolCallRef{call})

	msgs := c.GetMessages()
	if len(msgs) != 1 || msgs[0].Role != RoleTool {
		t.Fatalf("普通输出应只产生 1 条 tool 消息，实际 %+v", msgs)
	}
}

// TestRunToolCallsIgnoresImagesOnError 验证工具报错时不解析图片字段，
// 错误文本原样交给模型。
func TestRunToolCallsIgnoresImagesOnError(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("screenshot", func(_ context.Context, _ string) (string, error) {
		return `{"_tool_images":[{"path":"/tmp/x.png"}]}`, context.DeadlineExceeded
	})

	c := NewConversationLoop(NewIterationBudget(10), registry, nil)

	call := ToolCallRef{ID: "call_1", Type: "function"}
	call.Function.Name = "screenshot"
	call.Function.Arguments = "{}"

	results := c.runToolCalls(context.Background(), []ToolCallRef{call})

	if len(results) != 1 || len(results[0].Images) != 0 {
		t.Fatalf("报错时不应产出图片，实际 %+v", results)
	}

	msgs := c.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("报错时不应追加 user 消息，实际 %d 条", len(msgs))
	}
}
