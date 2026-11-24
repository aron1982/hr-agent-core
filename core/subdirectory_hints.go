package core

import (
	"os"
	"path/filepath"
	"strings"
)

// hintFileNames 需要查找的上下文提示文件名（按优先级排序）
var hintFileNames = []string{
	"AGENTS.md", "agents.md",
	"CLAUDE.md", "claude.md",
	".cursorrules",
}

const maxHintChars = 8000 // 单个提示文件的最大字符数

// SubdirectoryHintTracker 子目录提示追踪器，在 agent 访问新目录时发现并加载项目上下文文件
type SubdirectoryHintTracker struct {
	workingDir string            // 工作根目录
	hints      map[string]string // 已加载的提示：目录路径 -> 提示文本
	loadedDirs map[string]bool   // 已扫描过的目录
}

// NewSubdirectoryHintTracker 创建新的子目录提示追踪器
func NewSubdirectoryHintTracker(workingDir string) *SubdirectoryHintTracker {
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		abs = workingDir
	}
	t := &SubdirectoryHintTracker{
		workingDir: abs,
		hints:      make(map[string]string),
		loadedDirs: make(map[string]bool),
	}
	// 工作根目录标记为已加载（由启动上下文处理）
	t.loadedDirs[abs] = true
	return t
}

// AddHint 为指定目录添加一条提示（可用于手动注入提示）
func (t *SubdirectoryHintTracker) AddHint(dir, hint string) {
	if !t.isWithinWorkingDir(dir) {
		return
	}
	existing := t.hints[dir]
	if existing != "" {
		existing += "\n\n"
	}
	t.hints[dir] = existing + hint
}

// GetHints 返回所有已收集的提示文本（用双换行分隔）
func (t *SubdirectoryHintTracker) GetHints() string {
	var parts []string
	for _, hint := range t.hints {
		if hint != "" {
			parts = append(parts, hint)
		}
	}
	return strings.Join(parts, "\n\n")
}

// CheckPath 检查给定路径所在的目录，若有新的提示文件则加载并返回提示文本
func (t *SubdirectoryHintTracker) CheckPath(rawPath string) string {
	dirs := t.extractDirectories(rawPath)
	if len(dirs) == 0 {
		return ""
	}

	var allHints []string
	for _, d := range dirs {
		hint := t.loadHintsForDirectory(d)
		if hint != "" {
			allHints = append(allHints, hint)
		}
	}

	return strings.Join(allHints, "\n\n")
}

// extractDirectories 从路径中提取需要检查的目录
func (t *SubdirectoryHintTracker) extractDirectories(rawPath string) []string {
	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return nil
	}

	// 如果是文件，取其父目录
	fi, err := os.Stat(abs)
	if err == nil && !fi.IsDir() {
		abs = filepath.Dir(abs)
	}

	// 向上遍历最多 5 层父目录
	var dirs []string
	for range 5 {
		if abs == "" || abs == "." {
			break
		}
		if t.loadedDirs[abs] {
			break
		}
		if t.isWithinWorkingDir(abs) {
			dirs = append(dirs, abs)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}

	// 反转以保证从根到叶的顺序
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}

	return dirs
}

// loadHintsForDirectory 从指定目录加载提示文件
func (t *SubdirectoryHintTracker) loadHintsForDirectory(dir string) string {
	if !t.isWithinWorkingDir(dir) {
		return ""
	}
	t.loadedDirs[dir] = true

	for _, filename := range hintFileNames {
		hintPath := filepath.Join(dir, filename)
		data, err := os.ReadFile(hintPath)
		if err != nil {
			continue
		}

		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}

		// 截断过长内容
		if len(content) > maxHintChars {
			content = content[:maxHintChars] + "\n\n[...truncated " + filename + "]"
		}

		// 计算相对路径用于展示
		relPath := hintPath
		if rel, err := filepath.Rel(t.workingDir, hintPath); err == nil {
			relPath = rel
		}

		formatted := "[Subdirectory context discovered: " + relPath + "]\n" + content
		t.hints[dir] = formatted
		return formatted
	}

	return ""
}

// isWithinWorkingDir 检查目录是否在工作目录树内
func (t *SubdirectoryHintTracker) isWithinWorkingDir(dir string) bool {
	rel, err := filepath.Rel(t.workingDir, dir)
	if err != nil {
		return false
	}
	// 相对路径不能以 ".." 开头（不在工作目录内）
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) || rel == "."
}
