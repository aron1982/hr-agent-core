package core

import (
	"net"
	"net/url"
	"strings"
)

// ModelMetadata 模型能力元数据
type ModelMetadata struct {
	Name            string // 模型显示名
	ContextWindow   int    // 上下文窗口大小（tokens）
	MaxOutputTokens int    // 最大输出 tokens
	Provider        string // 提供商名称
}

// KnownModels 已知模型的元数据表（硬编码兜底，优先使用远程获取的数据）
var KnownModels = map[string]ModelMetadata{
	// Anthropic
	"claude-sonnet-4-20250514": {Name: "Claude Sonnet 4", ContextWindow: 200000, MaxOutputTokens: 64000, Provider: "anthropic"},
	"claude-opus-4-20250514":   {Name: "Claude Opus 4", ContextWindow: 200000, MaxOutputTokens: 64000, Provider: "anthropic"},
	// OpenAI
	"gpt-4o":      {Name: "GPT-4o", ContextWindow: 128000, MaxOutputTokens: 16384, Provider: "openai"},
	"gpt-4o-mini": {Name: "GPT-4o Mini", ContextWindow: 128000, MaxOutputTokens: 16384, Provider: "openai"},
	"gpt-5.4":     {Name: "GPT-5.4", ContextWindow: 1050000, MaxOutputTokens: 32768, Provider: "openai"},
	"gpt-5.5":     {Name: "GPT-5.5", ContextWindow: 1050000, MaxOutputTokens: 32768, Provider: "openai"},
	// Google
	"gemini-2.5-pro":   {Name: "Gemini 2.5 Pro", ContextWindow: 1048576, MaxOutputTokens: 65536, Provider: "google"},
	"gemini-2.5-flash": {Name: "Gemini 2.5 Flash", ContextWindow: 1048576, MaxOutputTokens: 65536, Provider: "google"},
	// DeepSeek
	"deepseek-flash":  {Name: "DeepSeek Flash", ContextWindow: 1000000, MaxOutputTokens: 393216, Provider: "deepseek"},
	"deepseek-v4-pro": {Name: "DeepSeek V4 Pro", ContextWindow: 1000000, MaxOutputTokens: 393216, Provider: "deepseek"},
}

// FetchModelMetadata 根据模型名查找元数据，优先查 KnownModels
func FetchModelMetadata(modelName string) (*ModelMetadata, error) {
	if meta, ok := KnownModels[modelName]; ok {
		return &meta, nil
	}
	// 模糊匹配：KnownModels 中的 key 是否为 modelName 的子串
	for key, meta := range KnownModels {
		if strings.Contains(modelName, key) {
			return &meta, nil
		}
	}
	return nil, nil // 未找到，返回 nil 而非 error，由调用方决定默认值
}

// localHosts 本地/回环地址集合
var localHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"0.0.0.0":   true,
}

// containerLocalSuffixes Docker/Podman/Lima 内部 DNS 后缀
var containerLocalSuffixes = []string{
	".docker.internal",
	".containers.internal",
	".lima.internal",
}

// IsLocalEndpoint 判断 baseURL 是否指向本地端点
func IsLocalEndpoint(rawURL string) bool {
	u := normalizeURL(rawURL)
	if u == "" {
		return false
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	if host == "" {
		return false
	}

	// 本地主机名
	if localHosts[host] {
		return true
	}

	// Docker/Podman/Lima 内部 DNS 名
	for _, suffix := range containerLocalSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}

	// 无点号的主机名（Docker Compose 服务名等）
	if !strings.Contains(host, ".") {
		return true
	}

	// IP 地址检查
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
		// Tailscale CGNAT 100.64.0.0/10
		if ip4 := ip.To4(); ip4 != nil {
			if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
				return true
			}
		}
		return false
	}

	// 字符串匹配私有 IP 段（net.ParseIP 可能失败的回退）
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	// 简化：检查是否在私有 IP 范围内
	if parts[0] == "10" {
		return true
	}
	if parts[0] == "172" {
		// 172.16.0.0 – 172.31.255.255
		var second int
		for _, c := range parts[1] {
			if c < '0' || c > '9' {
				return false
			}
			second = second*10 + int(c-'0')
		}
		return second >= 16 && second <= 31
	}
	if parts[0] == "192" && parts[1] == "168" {
		return true
	}
	if parts[0] == "100" {
		var second int
		for _, c := range parts[1] {
			if c < '0' || c > '9' {
				return false
			}
			second = second*10 + int(c-'0')
		}
		return second >= 64 && second <= 127
	}

	return false
}

// normalizeURL 标准化 baseURL，去除尾部斜杠
func normalizeURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	return rawURL
}

// EstimateTokensByChars 按字符数估算 token 数（4 字符 ≈ 1 token）。
//
// 这是**唯一**的估算实现：上下文压缩（estimateMessageTokens）
// 也走这里，避免两处各自内联同一公式后漂移。
func EstimateTokensByChars(chars int) int {
	if chars <= 0 {
		return 0
	}

	return (chars + 3) / 4
}

// EstimateTokensRough 粗略估算文本 token 数。
func EstimateTokensRough(text string) int {
	return EstimateTokensByChars(len(text))
}
