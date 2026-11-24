package core

import "regexp"

// ContextReference 上下文引用，描述代码中的符号引用
type ContextReference struct {
	Type    string // 引用类型："file", "function", "class", "module"
	Path    string // 文件路径
	Line    int    // 行号
	Content string // 引用内容
}

// referencePatterns 常见的代码引用模式
var referencePatterns = []*regexp.Regexp{
	// 文件引用: `path/to/file.ext` 或 path/to/file.ext
	regexp.MustCompile("`([a-zA-Z0-9_./-]+\\.[a-zA-Z]{1,10})`"),
	regexp.MustCompile(`([a-zA-Z0-9_./-]+\.(?:go|py|js|ts|java|rs|cpp|c|h))`),
	// 函数/方法引用: funcName()
	regexp.MustCompile(`([A-Za-z_]\w*)\s*\(`),
}

// ExtractReferences 从文本中提取上下文引用
func ExtractReferences(text string) []ContextReference {
	seen := make(map[string]bool)
	var refs []ContextReference

	for _, pattern := range referencePatterns {
		matches := pattern.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			content := match[1]
			if seen[content] {
				continue
			}
			seen[content] = true

			ref := ContextReference{
				Content: content,
			}
			ref.inferType()
			refs = append(refs, ref)
		}
	}

	return refs
}

// inferType 根据内容推断引用类型
func (r *ContextReference) inferType() {
	content := r.Content

	// 包含文件扩展名 → 文件引用
	fileExts := []string{".go", ".py", ".js", ".ts", ".java", ".rs", ".cpp", ".c", ".h",
		".yaml", ".yml", ".json", ".toml", ".xml", ".md", ".txt"}
	for _, ext := range fileExts {
		if len(content) > len(ext) && content[len(content)-len(ext):] == ext {
			r.Type = "file"
			r.Path = content
			return
		}
	}

	// 包含 . 且不含括号 → module/class 引用
	if regexp.MustCompile(`\.`).MatchString(content) {
		r.Type = "module"
		return
	}

	// 默认 → function 引用
	r.Type = "function"
}

// ResolveReferences 解析引用列表（占位实现）
func ResolveReferences(refs []ContextReference) ([]ContextReference, error) {
	// 当前为占位实现，后续可扩展为从文件系统读取实际内容
	return refs, nil
}
