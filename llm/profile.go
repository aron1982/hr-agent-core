package llm

import (
	"encoding/json"
	"strings"
)

// maxTokensKeyStandard 标准的输出上限键名。
const maxTokensKeyStandard = "max_tokens"

// maxTokensKeyCompletion OpenAI o1 / gpt-5 系使用的键名。
const maxTokensKeyCompletion = "max_completion_tokens"

// thinkingSuffix 以模型名后缀表达思考开关时使用的后缀（DeepSeek 风格）。
const thinkingSuffix = "-thinking"

// applyProfile 把渠道级与模型级差异应用到请求体。
//
// 调用时机：基础字段（model / messages / tools）与采样参数就位之后，
// 合并用户 ExtraBody 之前 —— 用户配置始终拥有最终决定权。
func applyProfile(kwargs map[string]any, cfg Config) {
	applyTemperature(kwargs, cfg.ModelProfile)
	applyTopP(kwargs, cfg.Profile)
	applyMaxTokensField(kwargs, cfg.Profile)
	applyReasoning(kwargs, cfg)
	applyReasoningSplit(kwargs, cfg.Profile)

	if cfg.Profile.StripImageDataURLPrefix {
		stripImageDataURLPrefix(kwargs)
	}
}

// applyReasoningSplit 要求上游把思考内容拆到独立字段（MiniMax reasoning_split）。
//
// 顶层字段而非 extra_body：OpenAI SDK 里的 extra_body 只是「把键合并进请求体」
// 的客户端语法糖，原始 HTTP 请求里它就是顶层键。不拆分的后果是思考内容混在
// content 里带 <think> 标签下发，被当作正文展示。
func applyReasoningSplit(kwargs map[string]any, p ChannelProfile) {
	if !p.ReasoningSplit {
		return
	}

	kwargs["reasoning_split"] = true
}

// applyTemperature 应用模型级的 temperature 覆盖。
//
// 两个方向都存在真实用例：kimi-k2.6 只接受 1.0（强制），Kimi K3 完全不
// 接受该字段（省略）。
func applyTemperature(kwargs map[string]any, p ModelProfile) {
	if p.OmitTemperature {
		delete(kwargs, "temperature")

		return
	}

	if p.TemperatureFixed > 0 {
		kwargs["temperature"] = p.TemperatureFixed
	}
}

// applyTopP 钳制 top_p 上限。
//
// GLM 对 top_p >= 1 返回参数错误，需要钳到 0.99；其余渠道零值不生效。
func applyTopP(kwargs map[string]any, p ChannelProfile) {
	if p.TopPMax <= 0 {
		return
	}

	f, ok := toFloat(kwargs["top_p"])
	if !ok {
		return
	}

	if f > p.TopPMax {
		kwargs["top_p"] = p.TopPMax
	}
}

// applyMaxTokensField 按渠道要求改写输出上限的键名。
func applyMaxTokensField(kwargs map[string]any, p ChannelProfile) {
	field := strings.TrimSpace(p.MaxTokensField)
	if field == "" || field == maxTokensKeyStandard {
		return
	}

	v, ok := kwargs[maxTokensKeyStandard]
	if !ok {
		return
	}

	delete(kwargs, maxTokensKeyStandard)
	kwargs[field] = v
}

// applyReasoning 按渠道声明的形状注入思考参数。
//
// 三种情形：
//   - ReasoningNone：不注入，交给模型自身默认行为；
//   - ReasoningModelNameSuffix：由模型名后缀决定（无需外部开关），
//     同时把后缀从实际发送的 model 上剥掉；
//   - 其余形状：仅在 Config.Thinking 显式设置时注入，未设置时不干预 ——
//     这些字段属于"未知字段"，宁可不发也不要触发严格渠道的 400。
func applyReasoning(kwargs map[string]any, cfg Config) {
	shape := cfg.Profile.ReasoningShape
	if shape == ReasoningNone {
		return
	}

	if shape == ReasoningModelNameSuffix {
		model, _ := kwargs["model"].(string)
		if !strings.HasSuffix(model, thinkingSuffix) {
			return
		}

		kwargs["model"] = strings.TrimSuffix(model, thinkingSuffix)
		kwargs["thinking"] = map[string]any{"type": thinkingType(true, cfg.Profile)}

		return
	}

	if cfg.Thinking == nil {
		return
	}

	enabled := *cfg.Thinking

	switch shape {
	case ReasoningExtraBodyThinking:
		ensureMap(kwargs, "extra_body")["thinking"] = map[string]any{"type": thinkingType(enabled, cfg.Profile)}
	case ReasoningExtraBodyEnable:
		ensureMap(kwargs, "extra_body")["enable_thinking"] = enabled
	case ReasoningTopLevelThinking:
		kwargs["thinking"] = map[string]any{"type": thinkingType(enabled, cfg.Profile)}
	}
}

// thinkingType 把布尔开关转成各家通用的 type 字符串。
//
// 开启值可由系列覆盖（ThinkingOnValue）：MiniMax 只认 "adaptive"，发 "enabled"
// 会 400。关闭值目前统一为 "disabled"（尚无用例需要别的写法）。
func thinkingType(enabled bool, p ChannelProfile) string {
	if !enabled {
		return "disabled"
	}

	if v := strings.TrimSpace(p.ThinkingOnValue); v != "" {
		return v
	}

	return "enabled"
}

// stripImageDataURLPrefix 剥离图片 base64 的 data URL 前缀。
//
// GLM 要求 content 里的 image_url 只给裸 base64（`data:image/png;base64,`
// 前缀会导致参数校验失败），其余厂商接受带前缀的标准写法。
func stripImageDataURLPrefix(kwargs map[string]any) {
	messages, ok := kwargs["messages"].([]map[string]any)
	if !ok {
		return
	}

	for _, msg := range messages {
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok || partMap["type"] != "image_url" {
				continue
			}

			stripImagePart(partMap)
		}
	}
}

// stripImagePart 处理单个 image_url content part。
//
// image_url 有两种写法：字符串 `"image_url": "data:..."` 与对象
// `"image_url": {"url": "data:..."}`，都要覆盖。
func stripImagePart(part map[string]any) {
	switch v := part["image_url"].(type) {
	case string:
		part["image_url"] = stripDataURLPrefix(v)
	case map[string]any:
		if url, ok := v["url"].(string); ok {
			v["url"] = stripDataURLPrefix(url)
		}
	}
}

// stripDataURLPrefix 去掉 `data:<mime>;base64,` 前缀，返回裸数据。
func stripDataURLPrefix(url string) string {
	if !strings.HasPrefix(url, "data:") {
		return url
	}

	idx := strings.Index(url, ",")
	if idx < 0 {
		return url
	}

	return url[idx+1:]
}

// ensureMap 取 kwargs 里的子 map，缺失时创建。
func ensureMap(kwargs map[string]any, key string) map[string]any {
	if existing, ok := kwargs[key].(map[string]any); ok {
		return existing
	}

	created := map[string]any{}
	kwargs[key] = created

	return created
}

// toFloat 把 JSON 解出的数值统一成 float64。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
