package llm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aron1982/hr-agent-core/infra"
)

// sseDumpBody 一条覆盖"思考与正文交错 + 工具调用 + usage"的 SSE 响应，
// 用来验证转储把原始帧与归一化结果都写下来了。
const sseDumpBody = `data: {"choices":[{"delta":{"reasoning_content":"先看看目录"}}]}

data: {"choices":[{"delta":{"content":"好的"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"ls","arguments":"{\"p\":\"/tmp\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}

data: [DONE]

`

// TestLLMDumpCapturesFrames 转储开启时，请求体、SSE 原始帧、归一化结果
// 都应写入 llm-dump.log。
//
// 关注点是"原始帧"：归一化后 reasoning 被并成单一字符串、工具参数被逐帧
// 累积，事后无法还原模型真实的输出形态 —— 这正是分析对话显示功能需要的。
func TestLLMDumpCapturesFrames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRBUDDY_HOME", home)
	infra.SetEnabledForTest(true)
	defer infra.SetEnabledForTest(false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseDumpBody))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Model: "test-model"}, srv.Client(), nil)
	res, err := c.Chat(t.Context(), Options{
		Messages: []map[string]any{{"role": "user", "content": "列一下 /tmp"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.Content != "好的" {
		t.Fatalf("正文解析错误: %q", res.Content)
	}

	raw, err := os.ReadFile(filepath.Join(home, "logs", "llm-dump.log"))
	if err != nil {
		t.Fatalf("转储文件未生成: %v", err)
	}
	text := string(raw)

	// 原始帧逐帧落盘：能看到思考与正文分属不同帧，以及工具参数的分片形态
	for _, want := range []string{
		"frame[0]",
		`"reasoning_content":"先看看目录"`,
		`"content":"好的"`,
		`"name":"ls"`,
		// 归一化结果
		`"reasoning_content":"先看看目录"`,
		`"streaming":true`,
		// 请求体（判断"模型收到了什么"）
		`"messages"`,
		`列一下 /tmp`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("转储缺少 %q\n实际内容:\n%s", want, dumpPreview(text))
		}
	}
}

// TestLLMDumpRedactsSensitive 转储内容必须脱敏：请求历史里出现的 API key
// 不能原样落盘。
func TestLLMDumpRedactsSensitive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRBUDDY_HOME", home)
	infra.SetEnabledForTest(true)
	defer infra.SetEnabledForTest(false)

	const leaked = "sk-abcdefghijklmnopqrstuvwxyz012345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 用流式：转储的原始帧只在流式路径上采集，这条用例关心的是
		// 请求体脱敏，走流式才能同时覆盖到帧采集分支
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Model: "m"}, srv.Client(), nil)
	if _, err := c.Chat(t.Context(), Options{
		Messages: []map[string]any{{"role": "user", "content": "我的 key 是 " + leaked}},
		Stream:   true,
	}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(home, "logs", "llm-dump.log"))
	if err != nil {
		t.Fatalf("转储文件未生成: %v", err)
	}
	if strings.Contains(string(raw), leaked) {
		t.Error("转储文件里出现了未脱敏的 API key")
	}
	// RedactAPIKey 保留前后各 4 位（见 infra/redact.go）
	if !strings.Contains(string(raw), "sk-a...2345") {
		t.Errorf("期望看到脱敏后的形态，实际:\n%s", dumpPreview(string(raw)))
	}
}

// TestAppendNoopWhenDisabled 未开启时 Append 应为空操作（不建文件）。
func TestAppendNoopWhenDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRBUDDY_HOME", home)
	infra.SetEnabledForTest(false)

	infra.Append(&infra.LLMCallDump{Content: "不应落盘"})

	if _, err := os.Stat(filepath.Join(home, "logs", "llm-dump.log")); err == nil {
		t.Fatal("未开启转储时不应创建 llm-dump.log")
	}
}

func dumpPreview(s string) string {
	if len(s) > 1200 {
		return s[:1200] + "…(截断)"
	}

	return s
}
