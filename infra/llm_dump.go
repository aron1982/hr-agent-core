// LLM 原始报文转储（临时分析用）。
//
// 用途：分析对话显示功能时，需要看清「模型到底返回了什么」——正文、
// 思考链、工具调用参数各自长什么样、出现在哪一帧、以什么顺序交错。
// 这些信息在现有链路里会被逐层归一化（core 只取 content、落库只存最终
// 文本），事后无法还原，所以必须在最靠近模型的位置抓。
//
// 与 backend.log 的分工：slog 记的是「发生了什么」，这里记的是「原文」。
// 内容可能很大（含整段思考与工具参数），单独成文件，避免淹没常规日志。
//
// 脱敏：走 RedactSensitive —— 转储里可能出现用户粘贴的凭据、模型回显的
// 上下文密钥等，不能因为是临时日志就跳过。
//
// 开关：环境变量 HARMONY_LLM_DUMP=1 启用；未启用时全部函数退化为空操作
// （只有一个布尔判断），不产生任何 IO。
package infra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aron1982/hr-agent-core/apphome"
)

// LLMCallDump 一次 LLM 调用的完整转储。
type LLMCallDump struct {
	Time      string `json:"time"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Streaming bool   `json:"streaming"`
	Messages  int    `json:"messages"`
	Tools     int    `json:"tools"`

	// Request 归一化后的请求体（已脱敏）
	Request any `json:"request,omitempty"`

	// RawFrames SSE 原始帧，按到达顺序。流式调用才有。
	RawFrames []string `json:"raw_frames,omitempty"`

	// Result 归一化后的结果
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        any    `json:"tool_calls,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	Usage            any    `json:"usage,omitempty"`

	// Error 非空表示本次调用失败
	Error string `json:"error,omitempty"`

	// ElapsedMs 调用耗时
	ElapsedMs int64 `json:"elapsed_ms"`
}

// llmDumper LLM 转储的全局单例。
var llmDumper = &dumper{
	// 单文件上限，与 backend.log 同量级：分析场景下这个量级足够，
	// 超出后从头部截断（保留最近的内容）。
	maxFileSize: 32 << 20,
}

func init() {
	if os.Getenv("HARMONY_LLM_DUMP") == "1" {
		llmDumper.enabled.Store(true)
	}
}

// dumper 转储器。
//
// enabled 用 atomic 而非锁：Enabled() 在每次 LLM 调用前都要读它，
// 是热路径上的读操作，不该与写文件的锁互相牵连。SetEnabledForTest
// 只在测试里用，生产环境这个值进程内不变。
type dumper struct {
	enabled     atomic.Bool
	mu          sync.Mutex
	maxFileSize int64
}

// Enabled 报告转储是否启用。调用方据此决定是否做字符串拼接等额外开销。
func Enabled() bool { return llmDumper.enabled.Load() }

// SetEnabledForTest 供测试切换转储开关。
//
// 正常路径下开关只在进程启动时从环境变量读一次（那时改代价最小）；
// 测试需要在同一进程内反复开关，因此单独暴露。生产代码不应调用。
func SetEnabledForTest(on bool) { llmDumper.enabled.Store(on) }

// DumpPath 转储文件路径：<数据根>/logs/llm-dump.log。
//
// 与 backend.log 同目录（见 main.setupFileLogging），便于一并取用。
// 数据根解析失败时返回空串 —— 文件打不开不应影响主流程。
func DumpPath() string {
	dbPath, err := apphome.StateDBPath()
	if err != nil || dbPath == "" {
		return ""
	}

	return filepath.Join(filepath.Dir(dbPath), "logs", "llm-dump.log")
}

// Append 转储一条记录（NDJSON，每行一条）。
func Append(d *LLMCallDump) {
	if d == nil || !llmDumper.enabled.Load() {
		return
	}

	d.Time = time.Now().Format(time.RFC3339)
	line, err := json.Marshal(d)
	if err != nil {
		return
	}

	llmDumper.mu.Lock()
	defer llmDumper.mu.Unlock()

	path := DumpPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	// 超限从头部截断，保留最近内容（分析时新的调用更有参考价值）
	if info, err := os.Stat(path); err == nil && info.Size() > llmDumper.maxFileSize {
		// 关闭后重开为 O_TRUNC，避免边写边截断的竞态
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			_ = f.Close()
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	// 逐帧单独成行：raw_frames 可能极大，塞进一行会让 NDJSON 失去可读性
	if len(d.RawFrames) > 0 {
		meta := *d
		meta.RawFrames = nil
		if head, err := json.Marshal(meta); err == nil {
			fmt.Fprintf(f, "%s\n", head)
		}
		for i, frame := range d.RawFrames {
			_, _ = fmt.Fprintf(f, "    frame[%d] %s\n", i, RedactSensitive(frame))
		}
		return
	}
	_, _ = fmt.Fprintf(f, "%s\n", RedactSensitive(string(line)))
}
