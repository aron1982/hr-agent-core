package infra

import (
	"os"
	"path/filepath"
	"strings"
)

// IsPathTraversal 检查路径是否包含 ".." 遍历攻击
func IsPathTraversal(path string) bool {
	// 清理路径后检查是否仍包含 ".." 组件
	cleaned := filepath.Clean(path)
	return strings.Contains(cleaned, "..")
}

// SanitizePath 清理危险路径，移除路径遍历组件
func SanitizePath(path string) string {
	// 使用 filepath.Clean 标准化路径
	cleaned := filepath.Clean(path)
	// 移除前导的路径分隔符（防止绝对路径绕过）
	cleaned = strings.TrimLeft(cleaned, string(os.PathSeparator))
	return cleaned
}

// IsSafePath 检查路径是否在允许的根目录下
// 使用路径解析防止符号链接绕过
func IsSafePath(path string, allowedRoots []string) bool {
	if IsPathTraversal(path) {
		return false
	}

	// 解析为绝对路径
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}

	// 尝试解析符号链接
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// 如果符号链接无法解析（文件不存在），使用原始路径
		resolved = absPath
	}

	for _, root := range allowedRoots {
		absRoot, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			resolvedRoot = absRoot
		}

		// 确保 resolved 以 resolvedRoot + 分隔符开头，或完全相等
		if resolved == resolvedRoot {
			return true
		}
		rel, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return true
		}
	}

	return false
}
