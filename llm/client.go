package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aron1982/hr-agent-core/infra"
)

// defaultHTTPClient 调用方未注入客户端时的兜底（无超时限制，仅用于测试路径）。
var defaultHTTPClient = &http.Client{}

// Client 负责一次 OpenAI Chat Completions 调用的转发。
//
// 一个 Client 绑定一份 Config（渠道 + 模型）。它不保存任何跨请求状态 ——
// SSE 累积状态在每次 Chat 内部创建，因此同一个 Client 可被多会话并发使用。
type Client struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
}

// NewClient 创建客户端。httpClient / logger 为 nil 时使用默认值。
func NewClient(cfg Config, httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Client{cfg: cfg, http: httpClient, logger: logger}
}

// Config 返回该客户端绑定的配置副本。
func (c *Client) Config() Config {
	return c.cfg
}

// Options 单次调用的可变参数（每回合/每轮都可能不同，故不放进 Config）。
type Options struct {
	Messages []map[string]any
	Tools    []map[string]any

	Temperature float64
	MaxTokens   int

	// Stream 为 true 时请求 SSE 并逐块回调正文与思考链。
	Stream      bool
	OnChunk     func(string)
	OnReasoning func(string)
}

// Chat 执行一次调用。
func (c *Client) Chat(ctx context.Context, opts Options) (*Result, error) {
	kwargs := c.buildBody(opts)

	body, err := json.Marshal(kwargs)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// LLM 转储（临时分析用，默认关闭）：请求体先留一份，回调结束后
	// 连同响应一并落盘。未开启时 dump 为 nil，全程无额外开销。
	var dump *infra.LLMCallDump
	if infra.Enabled() {
		dump = &infra.LLMCallDump{
			Provider:  c.cfg.BaseURL,
			Model:     c.cfg.Model,
			Streaming: opts.Stream,
			Messages:  len(opts.Messages),
			Tools:     len(opts.Tools),
			Request:   redactRequestBody(kwargs),
		}
		defer func() { infra.Append(dump) }()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(c.cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	c.applyHeaders(req)

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if dump != nil {
			dump.ElapsedMs = time.Since(start).Milliseconds()
			dump.Error = err.Error()
		}

		return nil, networkError(err)
	}
	defer resp.Body.Close()

	var result *Result
	if opts.Stream {
		result, err = c.readStream(resp, opts, dump)
	} else {
		result, err = c.readOnce(resp)
	}
	if dump != nil {
		dump.ElapsedMs = time.Since(start).Milliseconds()
		if err != nil {
			dump.Error = err.Error()
		}
		if result != nil {
			dump.Content = result.Content
			dump.ReasoningContent = result.ReasoningContent
			dump.ToolCalls = result.ToolCalls
			dump.FinishReason = result.FinishReason
			dump.Usage = result.Usage
		}
	}

	return result, err
}

// redactRequestBody 请求体脱敏后转成可转储的形态。
//
// 请求体里唯一需要处理的是 messages[].content —— 系统提示词与历史里可能
// 含用户粘贴的凭据、门禁 cookie 等。RedactSensitive 是纯文本模式匹配，
// 对结构化请求体够用（转储只用于分析，不参与任何逻辑）。
func redactRequestBody(kwargs map[string]any) any {
	raw, err := json.Marshal(kwargs)
	if err != nil {
		return nil
	}

	return json.RawMessage(infra.RedactSensitive(string(raw)))
}

// networkError 把网络层失败分类。
//
// 用户主动中断（session.interrupt / ctx 取消）**不可重试** —— 重试会立刻
// 再被取消；超时与连接失败则可重试。
func networkError(err error) *Error {
	reason := ReasonNetwork
	switch {
	case errors.Is(err, context.Canceled):
		reason = ReasonCanceled
	case errors.Is(err, context.DeadlineExceeded):
		reason = ReasonTimeout
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			reason = ReasonTimeout
		}
	}

	return &Error{Message: err.Error(), Reason: reason, Retryable: reason.Retryable()}
}

// Endpoint 由 base_url 追加固定路径。
//
// **不做任何 /v1 猜测补齐**：各家的路径层级不同（智谱 /api/paas/v4、
// 火山 /api/v3、DashScope /compatible-mode/v1），任何启发式都会拼错其中
// 一部分。base_url 必须是完整前缀，这条约束由渠道目录与自定义渠道的
// 录入提示共同保证。
//
// 导出供「测试连接」（serve 包）复用：测试必须与真实调用打同一个地址，
// 否则会出现"测试通过、实际发不出请求"（或反之）的假象。
func Endpoint(baseURL string) string {
	// 去掉粘贴带入的首尾空白与零宽字符：这类不可见字符会让 url.Parse 把整个
	// 地址当成相对路径（首段 "https:" 含冒号）而报
	// "first path segment in URL cannot contain colon"。
	baseURL = strings.TrimSpace(strings.TrimLeft(baseURL, "\uFEFF\u200B"))
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// buildBody 按固定顺序组装请求体。
//
//  1. 基础字段（model / messages / tools / tool_choice）
//  2. 采样参数（标准键名）
//  3. 渠道差异（applyProfile 可能改写键名、钳制数值、注入思考字段）
//  4. 用户 ExtraBody（最高优先级）
func (c *Client) buildBody(opts Options) map[string]any {
	kwargs := map[string]any{
		"model":    c.cfg.Model,
		"messages": opts.Messages,
	}

	if len(opts.Tools) > 0 {
		kwargs["tools"] = opts.Tools
		kwargs["tool_choice"] = "auto"
	}

	kwargs["temperature"] = opts.Temperature

	if opts.MaxTokens > 0 {
		kwargs[maxTokensKeyStandard] = opts.MaxTokens
	}

	if opts.Stream {
		kwargs["stream"] = true
	}

	applyProfile(kwargs, c.cfg)

	// 部分渠道（如 MiniMax）流式默认不回 usage，需显式要求。
	// 放在 applyProfile 之后、mergeExtraBody 之前：用户 ExtraBody 仍可覆盖。
	if opts.Stream && c.cfg.Profile.StreamIncludeUsage {
		kwargs["stream_options"] = map[string]any{"include_usage": true}
	}
	mergeExtraBody(kwargs, c.cfg.ExtraBody)

	return kwargs
}

// structuralKeys 这三个键由本层管理，用户 ExtraBody 不得覆盖。
var structuralKeys = map[string]bool{
	"model":    true,
	"messages": true,
	"tools":    true,
}

// mergeExtraBody 合并用户自定义扩展字段。
//
// 跳过结构性键，其余以用户显式配置为准（可覆盖 temperature 等内置默认值）。
func mergeExtraBody(kwargs map[string]any, extra map[string]any) {
	for k, v := range extra {
		if structuralKeys[k] {
			continue
		}

		kwargs[k] = v
	}
}

// applyHeaders 写入请求头。
//
// 认证统一为 `Authorization: Bearer <key>` —— 本项目覆盖的国内渠道全部
// 使用该形式，因此不做 per-provider 分支。ExtraHeaders 用于订阅端点要求
// 的客户端标识（缺失可能 403）与 OpenRouter 一类的榜单标识。
func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")

	if value := authHeaderValue(c.cfg.APIKey); value != "" {
		req.Header.Set("Authorization", value)
	}

	for k, v := range c.cfg.ExtraHeaders {
		if k == "" || v == "" {
			continue
		}

		req.Header.Set(k, v)
	}
}

// authHeaderValue 组装 Bearer 鉴权头的值。
//
// 用户可能已经手写了 "Bearer " 前缀（部分配置界面如此），避免拼成
// "Bearer Bearer xxx"。
func authHeaderValue(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}

	if strings.HasPrefix(strings.ToLower(apiKey), "bearer ") {
		return apiKey
	}

	return "Bearer " + apiKey
}

// readOnce 处理一次性（非流式）响应。
func (c *Client) readOnce(resp *http.Response) (*Result, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newHTTPError(resp.StatusCode, resp.Header, body)
	}

	var raw completionResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析响应 JSON 失败: %w", err)
	}

	// 部分中转会以 200 返回错误体，必须显式检查。
	if raw.Error != nil {
		reason := classifyReason(resp.StatusCode, raw.Error.Code, raw.Error.Message)

		return nil, &Error{
			StatusCode: resp.StatusCode,
			Code:       raw.Error.Code,
			// message 截断：网关可能把整个 HTML 错误页透传进 message
			Message:   truncate(raw.Error.Message, 500),
			RawBody:   truncate(string(body), 500),
			Reason:    reason,
			Retryable: reason.Retryable(),
		}
	}

	if len(raw.Choices) == 0 {
		// 空响应可重试（上游偶发的空 completion），与 hermes-agent 的
		// 空响应重试语义一致。
		return nil, &Error{
			StatusCode: resp.StatusCode,
			Message:    "响应中没有 choices",
			RawBody:    truncate(string(body), 500),
			Reason:     ReasonEmptyResponse,
			Retryable:  true,
		}
	}

	return normalizeChoice(raw), nil
}

// readStream 处理 SSE 流式响应。
func (c *Client) readStream(resp *http.Response, opts Options, dump *infra.LLMCallDump) (*Result, error) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return nil, newHTTPError(resp.StatusCode, resp.Header, body)
	}

	// 转储开启时收集原始帧：归一化后 reasoning 被并入单一字符串、
	// 工具参数被逐帧累积，事后无法还原模型真实的输出形态
	var rawFrames []string
	var framesPtr *[]string
	if dump != nil {
		framesPtr = &rawFrames
	}

	content, reasoning, toolCalls, usage, err := parseSSEStream(resp.Body, opts.OnChunk, opts.OnReasoning, framesPtr)
	if dump != nil {
		dump.RawFrames = rawFrames
	}
	if err != nil {
		return nil, err
	}

	c.logger.Info("LLM 调用完成（流式）",
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"tool_calls", len(toolCalls),
	)

	return &Result{
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        toolCalls,
		Usage:            usage,
	}, nil
}

// completionResponse 非流式 chat.completions 响应。
//
// 字段取值刻意宽松：真实上游会返回旧格式 function_call、把 reasoning 放在
// reasoning_content 或 reasoning 里、cache 统计放在两个不同位置 —— 全部
// 兼容读取，不做严格校验。
type completionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			// ReasoningDetails MiniMax（reasoning_split=true）的结构化思考内容。
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			Refusal          string            `json:"refusal"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			// 旧格式：单个 function_call（早期 OpenAI 与部分兼容实现）
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usagePayload `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// normalizeChoice 把上游响应归一化为 Result。
func normalizeChoice(raw completionResponse) *Result {
	choice := raw.Choices[0]
	msg := choice.Message

	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	content := msg.Content

	toolCalls := make([]ToolCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}

		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	// 旧格式降级：没有 tool_calls 但有 function_call
	if len(toolCalls) == 0 && msg.FunctionCall != nil && msg.FunctionCall.Name != "" {
		toolCalls = append(toolCalls, ToolCall{
			Name:      msg.FunctionCall.Name,
			Arguments: msg.FunctionCall.Arguments,
		})
	}

	// 拒答：仅在没有正文也没有工具调用时提升为正文，否则用户会看到一段
	// 没有上下文的拒答文本混在正常回复里。
	if content == "" && len(toolCalls) == 0 && msg.Refusal != "" {
		content = msg.Refusal
		if finishReason == "stop" {
			finishReason = "content_filter"
		}
	}

	return &Result{
		Content:          content,
		ReasoningContent: reasoningText(msg.ReasoningContent, msg.Reasoning, msg.ReasoningDetails),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage:            raw.Usage.toUsage(),
	}
}
