package core

import (
	"strings"
	"testing"
)

// pngAttachment 构造一个只用于降级判定的附件（不读盘、不编码）。
func pngAttachment(id, name string) Attachment {
	return Attachment{
		ID:     id,
		Name:   name,
		Source: "/tmp/" + id + ".png",
		MIME:   "image/png",
		Size:   1024,
	}
}

// TestDegradeOldImagesKeepsRecentTurns 默认窗口内（最近 keepTurns 轮 user）的
// 图片必须原样保留：追问几乎总是围绕最近一两轮的图。
func TestDegradeOldImagesKeepsRecentTurns(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "第一张", Attachments: []Attachment{pngAttachment("a1", "old.png")}},
		{Role: RoleAssistant, Content: "看到了"},
		{Role: RoleUser, Content: "第二张", Attachments: []Attachment{pngAttachment("a2", "mid.png")}},
		{Role: RoleAssistant, Content: "看到了"},
		{Role: RoleUser, Content: "第三张", Attachments: []Attachment{pngAttachment("a3", "new.png")}},
	}

	got := degradeOldImages(messages, defaultImageHistoryTurns)

	if len(got[0].Attachments) != 0 {
		t.Fatalf("最早一轮的图应被降级，实际仍有 %d 张", len(got[0].Attachments))
	}
	if !strings.Contains(got[0].Content, "old.png") {
		t.Fatalf("降级后正文应带占位标记，实际 %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "第一张") {
		t.Fatalf("降级不能丢掉原正文，实际 %q", got[0].Content)
	}

	for _, i := range []int{2, 4} {
		if len(got[i].Attachments) != 1 {
			t.Fatalf("窗口内第 %d 条应保留原图，实际 %d 张", i, len(got[i].Attachments))
		}
		if strings.Contains(got[i].Content, "已省略") {
			t.Fatalf("窗口内第 %d 条不应出现占位标记: %q", i, got[i].Content)
		}
	}
}

// TestDegradeOldImagesIsPure 降级必须是函数式的：不回写对话历史（前端还要显示
// 原图），且同一份历史重复调用结果一致 —— 否则每轮都会改写前缀，prompt cache
// 会一直失效。
func TestDegradeOldImagesIsPure(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "第一张", Attachments: []Attachment{pngAttachment("a1", "old.png")}},
		{Role: RoleUser, Content: "第二张", Attachments: []Attachment{pngAttachment("a2", "mid.png")}},
	}

	first := degradeOldImages(messages, 1)
	second := degradeOldImages(messages, 1)

	if len(messages[0].Attachments) != 1 || messages[0].Content != "第一张" {
		t.Fatalf("入参历史被就地改写: %+v", messages[0])
	}
	if first[0].Content != second[0].Content || len(first[0].Attachments) != len(second[0].Attachments) {
		t.Fatalf("同一份历史两次降级结果不一致:\n%q\n%q", first[0].Content, second[0].Content)
	}
	if len(first[0].Attachments) != 0 {
		t.Fatalf("第一轮应被降级，实际仍有 %d 张", len(first[0].Attachments))
	}
}

// TestDegradeOldImagesZeroTurns 窗口为 0（配置为负数时的归一化结果）表示全部
// 降级 —— 用于不支持视觉或极端省流量的场景。
func TestDegradeOldImagesZeroTurns(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "第一张", Attachments: []Attachment{pngAttachment("a1", "one.png")}},
		{Role: RoleUser, Content: "第二张", Attachments: []Attachment{pngAttachment("a2", "two.png")}},
	}

	got := degradeOldImages(messages, 0)

	for i, msg := range got {
		if len(msg.Attachments) != 0 {
			t.Fatalf("第 %d 条应被降级，实际仍有 %d 张", i, len(msg.Attachments))
		}
		if !strings.Contains(msg.Content, "已省略") {
			t.Fatalf("第 %d 条缺少占位标记: %q", i, msg.Content)
		}
	}

	// 负数入口同样按全部降级处理，不允许出现"负数等于不降级"的误读
	negative := degradeOldImages(messages, -1)
	if len(negative[0].Attachments) != 0 {
		t.Fatalf("负数窗口应全部降级，实际仍有 %d 张", len(negative[0].Attachments))
	}
}

// TestDegradeOldImagesPureImageMessage 纯图片消息降级后正文不能为空：严格端点
// 会拒绝 content 为空的 user 消息。
func TestDegradeOldImagesPureImageMessage(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Attachments: []Attachment{pngAttachment("a1", "shot.png")}},
		{Role: RoleUser, Content: "接着看"},
	}

	got := degradeOldImages(messages, 0)

	if strings.TrimSpace(got[0].Content) == "" {
		t.Fatal("纯图片消息降级后正文不能为空")
	}
	if !strings.Contains(got[0].Content, "shot.png") {
		t.Fatalf("占位标记应带原始文件名，实际 %q", got[0].Content)
	}
}

// TestDegradeOldImagesNoAttachmentsUntouched 无附件的历史不做任何分配，直接原样
// 返回：绝大多数文本会话不该为这个特性付出拷贝成本。
func TestDegradeOldImagesNoAttachmentsUntouched(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "你好"},
		{Role: RoleAssistant, Content: "你好"},
		{Role: RoleUser, Content: "在吗"},
	}

	got := degradeOldImages(messages, 0)

	if len(got) != len(messages) || &got[0] != &messages[0] {
		t.Fatal("无附件历史应原样返回同一底层数组")
	}

	if out := degradeOldImages(nil, 2); out != nil {
		t.Fatalf("空历史应直接返回，实际 %v", out)
	}
}

// TestImageHistoryLimit 配置归一化：0 = 默认轮数，负数 = 全部降级，正数原样透传。
func TestImageHistoryLimit(t *testing.T) {
	cases := []struct {
		configured int
		want       int
	}{
		{0, defaultImageHistoryTurns},
		{1, 1},
		{5, 5},
		{-1, 0},
		{-100, 0},
	}

	for _, c := range cases {
		agent := &Agent{imageHistoryTurns: c.configured}
		if got := agent.imageHistoryLimit(); got != c.want {
			t.Fatalf("imageHistoryTurns=%d 期望 %d，实际 %d", c.configured, c.want, got)
		}
	}
}

// TestAttachmentDisplayName 占位标记的显示名回退顺序：文件名 → 附件 ID → 泛称。
func TestAttachmentDisplayName(t *testing.T) {
	cases := []struct {
		att  Attachment
		want string
	}{
		{Attachment{Name: "报表.png", ID: "att-1"}, "报表.png"},
		{Attachment{Name: "  ", ID: "att-2"}, "att-2"},
		{Attachment{}, "图片"},
	}

	for _, c := range cases {
		if got := attachmentDisplayName(c.att); got != c.want {
			t.Fatalf("期望 %q，实际 %q", c.want, got)
		}
	}
}
