package core

import "strings"

// CodingContext 编码上下文，描述当前代码项目的环境信息
type CodingContext struct {
	Language     string   // 编程语言
	ProjectPath  string   // 项目根路径
	Files        []string // 涉及的文件列表
	Dependencies []string // 依赖列表
}

// ExtractCodingContext 从代码片段中提取编码上下文
func ExtractCodingContext(code string) *CodingContext {
	ctx := &CodingContext{}

	// 从代码特征推断语言
	ctx.Language = detectLanguage(code)

	// 提取 import/include/require 等依赖语句
	ctx.Dependencies = extractDependencies(code, ctx.Language)

	return ctx
}

// detectLanguage 根据代码特征推断编程语言
func detectLanguage(code string) string {
	features := map[string]string{
		"package ":     "go",
		"func ":        "go",
		"import (":     "go",
		"def ":         "python",
		"import ":      "python", // Python 的 import 在非 go 上下文
		"from ":        "python",
		"function ":    "javascript",
		"const ":       "javascript",
		"public class": "java",
		"fn ":          "rust",
		"#include":     "cpp",
	}

	for pattern, lang := range features {
		if strings.Contains(code, pattern) {
			return lang
		}
	}

	return ""
}

// extractDependencies 从代码中提取依赖声明
func extractDependencies(code string, language string) []string {
	var deps []string
	lines := strings.Split(code, "\n")

	switch language {
	case "go":
		inImport := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "import (" {
				inImport = true
				continue
			}
			if inImport {
				if trimmed == ")" {
					inImport = false
					continue
				}
				// 提取引号内的包名
				if start := strings.Index(trimmed, `"`); start >= 0 {
					if end := strings.Index(trimmed[start+1:], `"`); end >= 0 {
						deps = append(deps, trimmed[start+1:start+1+end])
					}
				}
			} else if strings.HasPrefix(trimmed, `import "`) {
				// 单行 import
				pkg := strings.TrimPrefix(trimmed, `import "`)
				pkg = strings.TrimSuffix(pkg, `"`)
				deps = append(deps, pkg)
			}
		}
	case "python":
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "import ") {
				pkg := strings.TrimPrefix(trimmed, "import ")
				if idx := strings.Index(pkg, " as "); idx > 0 {
					pkg = pkg[:idx]
				}
				deps = append(deps, strings.TrimSpace(pkg))
			} else if strings.HasPrefix(trimmed, "from ") {
				parts := strings.Fields(trimmed)
				if len(parts) >= 2 {
					deps = append(deps, parts[1])
				}
			}
		}
	case "javascript":
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "require(") {
				if start := strings.Index(trimmed, `"`); start >= 0 {
					if end := strings.Index(trimmed[start+1:], `"`); end >= 0 {
						deps = append(deps, trimmed[start+1:start+1+end])
					}
				} else if start := strings.Index(trimmed, `'`); start >= 0 {
					if end := strings.Index(trimmed[start+1:], `'`); end >= 0 {
						deps = append(deps, trimmed[start+1:start+1+end])
					}
				}
			}
		}
	}

	return deps
}
