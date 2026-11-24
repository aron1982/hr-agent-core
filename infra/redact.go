package infra

import (
	"regexp"
	"strings"
)

// RedactAPIKey 脱敏 API key（保留前后各 4 个字符，中间用 *** 替换）
// 长度不足 floor 的短值完全替换为 "***"
func RedactAPIKey(key string) string {
	const head = 4
	const tail = 4
	const floor = 12
	const placeholder = "***"

	if key == "" {
		return ""
	}
	if len(key) < floor {
		return placeholder
	}
	return key[:head] + "..." + key[len(key)-tail:]
}

// 已知 API key 前缀模式
var knownPrefixPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`),        // OpenAI / OpenRouter / Anthropic
	regexp.MustCompile(`ghp_[A-Za-z0-9]{10,}`),         // GitHub PAT
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{10,}`), // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), // Slack tokens
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{30,}`),       // Google API keys
	regexp.MustCompile(`pplx-[A-Za-z0-9]{10,}`),        // Perplexity
	regexp.MustCompile(`hf_[A-Za-z0-9]{10,}`),          // HuggingFace
	regexp.MustCompile(`sk_live_[A-Za-z0-9]{10,}`),     // Stripe live
	regexp.MustCompile(`sk_test_[A-Za-z0-9]{10,}`),     // Stripe test
	regexp.MustCompile(`gsk_[A-Za-z0-9]{10,}`),         // Groq Cloud
	regexp.MustCompile(`xai-[A-Za-z0-9]{30,}`),         // xAI (Grok)
}

// RedactSensitive 脱敏消息中的敏感模式（匹配 API key、token、密码等）
func RedactSensitive(text string) string {
	if text == "" {
		return text
	}

	result := text

	// 脱敏已知 API key 前缀模式
	for _, p := range knownPrefixPatterns {
		result = p.ReplaceAllStringFunc(result, func(match string) string {
			return RedactAPIKey(match)
		})
	}

	// 脱敏 Authorization 头 (Bearer xxx, Basic xxx)
	authHeaderRe := regexp.MustCompile(`(?i)(Authorization:\s*)(\S+)`)
	result = authHeaderRe.ReplaceAllString(result, "${1}***")

	// 脱敏 x-api-key 等头
	apiKeyHeaderRe := regexp.MustCompile(`(?i)((?:x-api-key|x-goog-api-key|api-key)\s*:\s*)(\S+)`)
	result = apiKeyHeaderRe.ReplaceAllString(result, "${1}***")

	// 脱敏 ENV 赋值中的敏感值 (KEY=value)
	envAssignRe := regexp.MustCompile(`(?i)((?:API_?KEY|TOKEN|SECRET|PASSWORD)\w*)\s*=\s*(\S+)`)
	result = envAssignRe.ReplaceAllStringFunc(result, func(match string) string {
		parts := envAssignRe.FindStringSubmatch(match)
		if len(parts) >= 3 && parts[2] != "" {
			return parts[1] + "=***"
		}
		return match
	})

	// 脱敏数据库连接字符串中的密码 (protocol://user:password@host)
	dbConnRe := regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^:]+:)([^@]+)(@)`)
	result = dbConnRe.ReplaceAllString(result, "${1}***${3}")

	// 脱敏 JWT token (以 eyJ 开头的 base64 编码)
	jwtRe := regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
	result = jwtRe.ReplaceAllStringFunc(result, func(match string) string {
		return RedactAPIKey(match)
	})

	// 脱敏 URL 中的 userinfo (scheme://user:password@host)
	urlUserRe := regexp.MustCompile(`(https?|wss?|ftp)://([^/\s:@]+):([^/\s@]+)@`)
	result = urlUserRe.ReplaceAllString(result, "${1}://${2}:***@")

	// 脱敏 JSON 字段中的敏感值 ("apiKey": "value")
	jsonFieldRe := regexp.MustCompile(`(?i)("(?:api_?[Kk]ey|token|secret|password|access_token|refresh_token)")\s*:\s*"([^"]+)"`)
	result = jsonFieldRe.ReplaceAllString(result, `${1}: "***"`)

	return result
}

// maskToken 对 token 进行脱敏（内部辅助函数，保留前后6/4字符）
func maskToken(token string) string {
	if len(token) < 18 {
		return "***"
	}
	return token[:6] + "***" + token[len(token)-4:]
}

// 确保 maskToken 可用（避免未使用警告）
var _ = maskToken

// 精简版 redactQueryString 脱敏查询参数中的值
func redactQueryString(query string) string {
	sensitiveParams := map[string]bool{
		"access_token":  true,
		"refresh_token": true,
		"id_token":      true,
		"token":         true,
		"api_key":       true,
		"apikey":        true,
		"client_secret": true,
		"password":      true,
		"secret":        true,
		"key":           true,
		"code":          true,
	}

	parts := strings.Split(query, "&")
	for i, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			key := strings.ToLower(kv[0])
			if sensitiveParams[key] {
				parts[i] = kv[0] + "=***"
			}
		}
	}
	return strings.Join(parts, "&")
}

// 确保 redactQueryString 可用（避免未使用警告，供后续扩展使用）
var _ = redactQueryString
