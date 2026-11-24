package infra

import "sync"

// 内置中英文字符串映射
var messages = map[string]map[string]string{
	"starting":      {"en": "Starting HrBuddy Agent...", "zh": "正在启动 HrBuddy Agent..."},
	"stopping":      {"en": "Stopping HrBuddy Agent...", "zh": "正在停止 HrBuddy Agent..."},
	"shutdown":      {"en": "HrBuddy Agent stopped.", "zh": "HrBuddy Agent 已停止。"},
	"tool_calls":    {"en": "Executing tool calls", "zh": "正在执行工具调用"},
	"thinking":      {"en": "Thinking...", "zh": "思考中..."},
	"error":         {"en": "Error", "zh": "错误"},
	"warning":       {"en": "Warning", "zh": "警告"},
	"retry":         {"en": "Retrying...", "zh": "正在重试..."},
	"connected":     {"en": "Connected", "zh": "已连接"},
	"disconnected":  {"en": "Disconnected", "zh": "已断开"},
	"loading":       {"en": "Loading...", "zh": "加载中..."},
	"done":          {"en": "Done", "zh": "完成"},
	"cancelled":     {"en": "Cancelled", "zh": "已取消"},
	"timeout":       {"en": "Timed out", "zh": "超时"},
	"not_found":     {"en": "Not found", "zh": "未找到"},
	"permission":    {"en": "Permission denied", "zh": "权限不足"},
	"config_saved":  {"en": "Configuration saved", "zh": "配置已保存"},
	"config_loaded": {"en": "Configuration loaded", "zh": "配置已加载"},
	"first_run":     {"en": "Welcome to HrBuddy Agent!", "zh": "欢迎使用 HrBuddy Agent！"},
	"goodbye":       {"en": "Goodbye!", "zh": "再见！"},
}

// 支持的语言列表
var supportedLanguages = map[string]bool{
	"en": true,
	"zh": true,
	"ja": true,
	"de": true,
	"es": true,
	"fr": true,
}

// 当前语言（默认英文）
var currentLanguage = "en"
var langMu sync.RWMutex

// SetLanguage 设置当前语言
// 不支持的语言会回退到 "en"
func SetLanguage(lang string) {
	langMu.Lock()
	defer langMu.Unlock()

	if supportedLanguages[lang] {
		currentLanguage = lang
	} else {
		currentLanguage = "en"
	}
}

// GetLanguage 获取当前语言
func GetLanguage() string {
	langMu.RLock()
	defer langMu.RUnlock()
	return currentLanguage
}

// T 翻译键值获取
// 如果 key 不存在，返回 key 本身
// 如果语言不存在对应文本，回退到英文
func T(key string, lang string) string {
	if lang == "" {
		lang = GetLanguage()
	}

	// 查找消息条目
	entry, ok := messages[key]
	if !ok {
		return key
	}

	// 优先查找请求的语言
	text, ok := entry[lang]
	if ok {
		return text
	}

	// 回退到英文
	text, ok = entry["en"]
	if ok {
		return text
	}

	// 最终回退：返回 key 本身
	return key
}

// RegisterMessage 注册新的翻译消息
// 用于扩展内置消息映射
func RegisterMessage(key string, langValues map[string]string) {
	if messages == nil {
		messages = make(map[string]map[string]string)
	}
	if existing, ok := messages[key]; ok {
		for lang, val := range langValues {
			existing[lang] = val
		}
	} else {
		messages[key] = langValues
	}
}
