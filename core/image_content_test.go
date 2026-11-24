package core

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestImage 落一张测试图片文件；内容不重要，构造请求只读字节。
func writeTestImage(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写入测试图片失败: %v", err)
	}

	return path
}

// TestBuildUserContentPlainText 无附件时保持字符串形态（不把简单消息变成 parts）。
func TestBuildUserContentPlainText(t *testing.T) {
	got := buildUserContent(Message{Role: RoleUser, Content: "你好"}, true)
	if got != "你好" {
		t.Fatalf("无附件应原样返回文本: %v", got)
	}
}

// TestBuildUserContentNativeParts 支持视觉时展开为 parts，图片编码为标准 data URL。
func TestBuildUserContentNativeParts(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	msg := Message{
		Role:    RoleUser,
		Content: "看看这张图",
		Attachments: []Attachment{{
			ID: "att-1", Source: writeTestImage(t, raw), MIME: "image/png", Size: int64(len(raw)),
		}},
	}

	content := buildUserContent(msg, true)

	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("有附件且支持视觉时应返回 parts 数组: %T", content)
	}
	if len(parts) != 2 {
		t.Fatalf("应为正文 + 图片两个 part，实际 %d", len(parts))
	}

	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "看看这张图" {
		t.Fatalf("首个 part 应为正文: %v", parts[0])
	}

	part, _ := parts[1].(map[string]any)
	imageURL, _ := part["image_url"].(map[string]any)
	url, _ := imageURL["url"].(string)

	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if url != want {
		t.Fatalf("图片应编码为标准 data URL\nwant %s\ngot  %s", want, url)
	}
}

// TestBuildUserContentBareBase64 裸 base64 形态不带 data URL 前缀（智谱 GLM 要求）。
func TestBuildUserContentBareBase64(t *testing.T) {
	raw := []byte{1, 2, 3, 4}
	msg := Message{
		Role: RoleUser,
		Attachments: []Attachment{{
			Source: writeTestImage(t, raw), MIME: "image/png", Mode: ImageParamBareBase64,
		}},
	}

	content := buildUserContent(msg, true)

	parts, ok := content.([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("纯图片消息应只含一个 part: %v", content)
	}

	part, _ := parts[0].(map[string]any)
	imageURL, _ := part["image_url"].(map[string]any)
	if url, _ := imageURL["url"].(string); url != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("GLM 形态应为裸 base64，实际: %v", url)
	}
}

// TestBuildUserContentVisionDisabled 模型未启用图片输入时不盲发（否则严格渠道 400）。
func TestBuildUserContentVisionDisabled(t *testing.T) {
	msg := Message{
		Role:        RoleUser,
		Content:     "看看",
		Attachments: []Attachment{{Source: writeTestImage(t, []byte{1})}},
	}

	content := buildUserContent(msg, false)

	got, ok := content.(string)
	if !ok {
		t.Fatalf("不支持视觉时应降级为文本: %T", content)
	}
	if !strings.Contains(got, "未启用图片输入") {
		t.Fatalf("降级文本应说明原因: %s", got)
	}
	// 不能写成"用户附了 N 张图片"：工具产出的图片（vision_analyze 等）走同一条
	// 路径，那样描述会让模型以为用户没给它看图，转而去问用户要图。
	if strings.Contains(got, "用户附") {
		t.Fatalf("降级文本不应假定图片来自用户: %s", got)
	}
}

// TestBuildUserContentMissingFile 图片文件缺失时降级为说明，不阻断整轮请求。
func TestBuildUserContentMissingFile(t *testing.T) {
	msg := Message{
		Role:        RoleUser,
		Content:     "看看",
		Attachments: []Attachment{{Source: filepath.Join(t.TempDir(), "nope.png")}},
	}

	content := buildUserContent(msg, true)

	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("读盘失败也应保持 parts 形态: %T", content)
	}
	if len(parts) != 2 {
		t.Fatalf("应保留正文并追加失败说明: %v", parts)
	}

	notice, _ := parts[1].(map[string]any)
	if text, _ := notice["text"].(string); !strings.Contains(text, "读取失败") {
		t.Fatalf("应给出读取失败说明: %v", parts[1])
	}
}

// TestMessagesToOpenAIAttachments 附件只在支持视觉时才进入请求体。
func TestMessagesToOpenAIAttachments(t *testing.T) {
	msgs := []Message{{
		Role:    RoleUser,
		Content: "图",
		Attachments: []Attachment{{
			Source: writeTestImage(t, []byte{9, 8, 7}), MIME: "image/jpeg",
		}},
	}}

	withVision := messagesToOpenAI(msgs, "", true)
	if _, ok := withVision[0]["content"].([]any); !ok {
		t.Fatalf("支持视觉时应输出 parts: %T", withVision[0]["content"])
	}

	withoutVision := messagesToOpenAI(msgs, "", false)
	if _, ok := withoutVision[0]["content"].(string); !ok {
		t.Fatalf("不支持视觉时应输出字符串: %T", withoutVision[0]["content"])
	}
}
